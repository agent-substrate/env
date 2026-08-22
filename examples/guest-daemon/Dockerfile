# Multi-stage Dockerfile for the Agent Substrate Guest Daemon binary
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /guest-daemon ./examples/guest-daemon

# Minimal runtime containing only the compiled guest daemon binary
FROM gcr.io/distroless/static-debian12

COPY --from=builder /guest-daemon /guest-daemon

EXPOSE 8080

ENTRYPOINT ["/guest-daemon"]

