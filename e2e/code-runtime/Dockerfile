# Code sandbox image for e2e tests only. It is not published.
#
# It holds the code daemon and the test runtime in runtime.cjs. Build it from
# the repository root:
#
#   docker buildx build -f e2e/code-runtime/Dockerfile .

FROM golang:1.27-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/code-daemon ./cmd/code-daemon

FROM node:24-bookworm-slim

# The runner starts sandboxes as 1000:1000 with HOME=/home/user. The daemon
# needs its work directory, /work by default, to be writable by that user.
RUN mkdir -p /work /home/user && chown 1000:1000 /work /home/user && chmod 700 /work

COPY --from=builder /out/code-daemon /usr/local/bin/code-daemon
COPY e2e/code-runtime/runtimes.json /etc/code-runtime/runtimes.json
COPY e2e/code-runtime/runtime.cjs /opt/code-runtime/runtime.cjs

USER 1000:1000
WORKDIR /work

EXPOSE 8081

CMD ["/usr/local/bin/code-daemon"]
