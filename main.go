// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// sfu-ws is a many-to-many websocket based SFU
package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"sync"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// nolint
var (
	addr     = flag.String("addr", ":8080", "http service address")
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	indexTemplate = &template.Template{}

	// lock for peerConnections and trackLocals
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP

	log *zap.Logger
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
}

func main() {
	// Initialize zap logger
	var err error
	log, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer log.Sync()

	// Parse the flags passed to program
	flag.Parse()

	// Init other state
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}

	// Read index.html from disk into memory, serve whenever anyone requests /
	indexHTML, err := os.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexTemplate = template.Must(template.New("").Parse(string(indexHTML)))

	// websocket handler
	http.HandleFunc("/websocket", websocketHandler)

	// index.html handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err = indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			log.Error("Failed to parse index template", zap.Error(err))
		}
	})

	// request a keyframe every 3 seconds
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			dispatchKeyFrame()
		}
	}()

	// start HTTP server
	if err = http.ListenAndServe(*addr, nil); err != nil { //nolint: gosec
		log.Error("Failed to start http server", zap.Error(err))
	}
}

// Add to list of tracks and fire renegotation for all PeerConnections.
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP { // nolint
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	trackLocals[t.ID()] = trackLocal

	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections.
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		signalPeerConnections()
	}()

	delete(trackLocals, t.ID())
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks.
func signalPeerConnections() { // nolint
	listLock.Lock()
	defer func() {
		listLock.Unlock()
		dispatchKeyFrame()
	}()

	attemptSync := func() (tryAgain bool) {
		for i := range peerConnections {
			if peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)

				return true // We modified the slice, start from the beginning
			}

			// map of sender we already are seanding, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					if err := peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loopback
			for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
						return true
					}
				}
			}

			offer, err := peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				return true
			}

			if err = peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				log.Error("Failed to marshal offer to json", zap.Error(err))

				return true
			}

			log.Info("Send offer to client", zap.Any("offer", offer))

			if err = peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
		}

		return tryAgain
	}

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			go func() {
				time.Sleep(time.Second * 3)
				signalPeerConnections()
			}()

			return
		}

		if !attemptSync() {
			break
		}
	}
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call.
func dispatchKeyFrame() {
	listLock.Lock()
	defer listLock.Unlock()

	for i := range peerConnections {
		for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

// Handle incoming websockets.
func websocketHandler(w http.ResponseWriter, r *http.Request) { // nolint
	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("Failed to upgrade HTTP to Websocket", zap.Error(err))

		return
	}

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}} // nolint

	// When this frame returns close the Websocket
	defer c.Close() //nolint

	// Create new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Error("Failed to creates a PeerConnection", zap.Error(err))

		return
	}

	// When this frame returns close the PeerConnection
	defer peerConnection.Close() //nolint

	// Accept one audio and one video track incoming
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			log.Error("Failed to add transceiver", zap.Error(err))

			return
		}
	}

	// Add our new PeerConnection to global list
	listLock.Lock()
	peerConnections = append(peerConnections, peerConnectionState{peerConnection, c})
	listLock.Unlock()

	// Trickle ICE. Emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		// If you are serializing a candidate make sure to use ToJSON
		// Using Marshal will result in errors around `sdpMid`
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Error("Failed to marshal candidate to json", zap.Error(err))

			return
		}

		log.Info("Send candidate to client", zap.String("candidate", string(candidateString)))

		if writeErr := c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			log.Error("Failed to write JSON", zap.Error(writeErr))
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		log.Info("Connection state change", zap.String("state", p.String()))

		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				log.Error("Failed to close PeerConnection", zap.Error(err))
			}
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections()
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Info("Got track", zap.String("trackID", t.ID()), zap.String("streamID", t.StreamID()))

		// TrackLocal track we send out to other PeerConnections
		trackLocal := addTrack(t)

		// Read RTP from incoming Track and send to local track, which broadcasts to others
		rtpBuf := make([]byte, 1500) // nolint:gomnd
		for {
			i, _, readErr := t.Read(rtpBuf)
			if readErr != nil {
				removeTrack(trackLocal)

				log.Info("Track ended", zap.String("trackID", t.ID()))

				return
			}

			if _, writeErr := trackLocal.Write(rtpBuf[:i]); writeErr != nil {
				log.Error("Failed to write to trackLocal", zap.Error(writeErr))
			}
		}
	})

	// Wait for messages from the client
	for {
		messageType, message, err := c.ReadMessage()
		if err != nil {
			log.Error("Failed to read message", zap.Error(err))

			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var wsMsg websocketMessage

		if err = json.Unmarshal(message, &wsMsg); err != nil {
			log.Error("Failed to unmarshal websocket message", zap.Error(err))

			return
		}

		log.Info("Got websocket message", zap.String("event", wsMsg.Event))

		switch wsMsg.Event {
		case "answer":
			var answer webrtc.SessionDescription

			if err = json.Unmarshal([]byte(wsMsg.Data), &answer); err != nil {
				log.Error("Failed to unmarshal answer", zap.Error(err))

				return
			}

			if err = peerConnection.SetRemoteDescription(answer); err != nil {
				log.Error("Failed to set remote description", zap.Error(err))

				return
			}

			signalPeerConnections()

		case "candidate":
			var candidate webrtc.ICECandidateInit

			if err = json.Unmarshal([]byte(wsMsg.Data), &candidate); err != nil {
				log.Error("Failed to unmarshal candidate", zap.Error(err))

				return
			}

			if err = peerConnection.AddICECandidate(candidate); err != nil {
				log.Error("Failed to add ICE candidate", zap.Error(err))

				return
			}
		}
	}
}

// threadSafeWriter serializes writes to a websocket.Conn to avoid concurrent write issues.
type threadSafeWriter struct {
	*websocket.Conn
	lock sync.Mutex
}

func (w *threadSafeWriter) WriteJSON(v interface{}) error {
	w.lock.Lock()
	defer w.lock.Unlock()

	return w.Conn.WriteJSON(v)
}
