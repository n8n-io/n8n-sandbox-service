# syntax=docker/dockerfile:1

# =============================================================================
# Stage 1: Build the service server binary
# =============================================================================
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

# =============================================================================
# Stage 2: Service image
# =============================================================================
FROM docker:29.3.1-dind-alpine3.23 AS service

RUN apk add --no-cache tini

COPY --from=builder /out/server /usr/local/bin/sandbox-server
COPY scripts/start-service.sh /usr/local/bin/start-service.sh

RUN chmod +x /usr/local/bin/start-service.sh \
    && mkdir -p /var/sandboxes

ENV SANDBOX_DATA_DIR=/var/sandboxes
ENV SANDBOX_LISTEN_ADDR=:8080
ENV SANDBOX_DOCKER_HOST=unix:///var/run/docker.sock
ENV SANDBOX_INTER_SANDBOX_NETWORK_ENABLED=false

EXPOSE 8080

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/start-service.sh"]
