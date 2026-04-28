SHELL := /bin/bash

MODULE  := github.com/n8n-io/sandbox-service
BINDIR  := bin

.PHONY: all daemon server test clean docker docker-local docker-arm64 docker-amd64 docker-service-arm64 docker-service-amd64 docker-sandbox-arm64 docker-sandbox-amd64 fmt fmt-check vet

all: daemon server

## fmt: Format all Go files.
fmt:
	gofmt -w .

## fmt-check: Check that all Go files are gofmt-formatted.
fmt-check:
	@unformatted=$$(gofmt -l .); status=$$?; \
	if [ $$status -ne 0 ]; then \
		exit $$status; \
	fi; \
	if [ -n "$$unformatted" ]; then \
		echo "Files not formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

## vet: Run go vet on all packages.
vet:
	go vet ./...

## daemon: Build the sandbox daemon (static, Linux).
daemon:
	CGO_ENABLED=0 GOOS=linux go build -o $(BINDIR)/daemon ./cmd/daemon

## server: Build the HTTP API server (Linux).
server:
	GOOS=linux go build -o $(BINDIR)/server ./cmd/server

## test: Run all tests.
test:
	go test ./...

## clean: Remove compiled binaries.
clean:
	rm -rf $(BINDIR)

ARCH := $(shell uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')

## docker: Build the service image for ONLY linux/amd64.
docker: docker-service-amd64

## docker-local: Build both images for the current architecture.
docker-local: docker-service-$(ARCH) docker-sandbox-$(ARCH)

## docker-arm64: Build both service and sandbox images for linux/arm64.
docker-arm64: docker-service-arm64 docker-sandbox-arm64

## docker-amd64: Build both service and sandbox images for linux/amd64.
docker-amd64: docker-service-amd64 docker-sandbox-amd64

## docker-service-arm64: Build the service image for linux/arm64.
docker-service-arm64:
	docker buildx build --platform linux/arm64 -t n8n-sandbox-service:latest-arm64 --load .

## docker-service-amd64: Build the service image for linux/amd64.
docker-service-amd64:
	docker buildx build --platform linux/amd64 -t n8n-sandbox-service:latest-amd64 --load .

## docker-sandbox-arm64: Build the sandbox image for linux/arm64.
docker-sandbox-arm64:
	docker buildx build -f Dockerfile.sandbox --platform linux/arm64 -t n8n-sandbox:latest-arm64 --load .

## docker-sandbox-amd64: Build the sandbox image for linux/amd64.
docker-sandbox-amd64:
	docker buildx build -f Dockerfile.sandbox --platform linux/amd64 -t n8n-sandbox:latest-amd64 --load .
