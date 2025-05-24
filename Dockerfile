# Stage 1: Build the Go binary
FROM golang:1.22.4 AS builder

WORKDIR /app

# Copy go.mod and go.sum
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . ./

# Build the binary (use your actual main package if not main.go)
RUN go build -o app main.go

# Expose port (optional: the port your app listens to)
EXPOSE 8080

# Run the binary
CMD ["./app"]
