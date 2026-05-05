SHELL := /bin/bash

MODULE  := github.com/n8n-io/sandbox-service
BINDIR  := bin

.PHONY: all daemon runner api test clean docker docker-local docker-arm64 docker-amd64 docker-api-arm64 docker-api-amd64 docker-runner-arm64 docker-runner-amd64 docker-sandbox-arm64 docker-sandbox-amd64 fmt fmt-check vet playground up down sdk sdk-install sdk-build sdk-typecheck sdk-test sdk-fmt sdk-fmt-check sdk-lint

all: daemon runner api

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

## runner: Build the runner service (Linux).
runner:
	GOOS=linux go build -o $(BINDIR)/runner ./cmd/runner

## api: Build the public API gateway server (Linux).
api:
	GOOS=linux go build -o $(BINDIR)/api ./cmd/api

## playground: Start the playground UI (http://localhost:5173).
playground: sdk-install sdk-build
	cd playground && npm install && npm start

## test: Run all tests.
test:
	go test ./...

## clean: Remove compiled binaries.
clean:
	rm -rf $(BINDIR)

ARCH := $(shell uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')

## docker: Build API + runner images for linux/amd64.
docker: docker-api-amd64 docker-runner-amd64

## docker-local: Build API, runner, and sandbox images for current architecture.
docker-local: docker-api-$(ARCH) docker-runner-$(ARCH) docker-sandbox-$(ARCH)

## docker-arm64: Build API, runner, and sandbox images for linux/arm64.
docker-arm64: docker-api-arm64 docker-runner-arm64 docker-sandbox-arm64

## docker-amd64: Build API, runner, and sandbox images for linux/amd64.
docker-amd64: docker-api-amd64 docker-runner-amd64 docker-sandbox-amd64

## docker-api-arm64: Build the API image for linux/arm64.
docker-api-arm64:
	docker buildx build -f Dockerfile.api --platform linux/arm64 -t n8n-sandbox-api:latest-arm64 --load .

## docker-api-amd64: Build the API image for linux/amd64.
docker-api-amd64:
	docker buildx build -f Dockerfile.api --platform linux/amd64 -t n8n-sandbox-api:latest-amd64 --load .

## docker-runner-arm64: Build the runner image for linux/arm64.
docker-runner-arm64:
	docker buildx build -f Dockerfile.runner --platform linux/arm64 -t n8n-sandbox-runner:latest-arm64 --load .

## docker-runner-amd64: Build the runner image for linux/amd64.
docker-runner-amd64:
	docker buildx build -f Dockerfile.runner --platform linux/amd64 -t n8n-sandbox-runner:latest-amd64 --load .

## docker-sandbox-arm64: Build the sandbox image for linux/arm64.
docker-sandbox-arm64:
	docker buildx build -f Dockerfile.sandbox --platform linux/arm64 -t n8n-sandbox:latest-arm64 --load .

## docker-sandbox-amd64: Build the sandbox image for linux/amd64.
docker-sandbox-amd64:
	docker buildx build -f Dockerfile.sandbox --platform linux/amd64 -t n8n-sandbox:latest-amd64 --load .

## sdk: Install, build, typecheck, and test the SDK.
sdk: sdk-install sdk-build sdk-typecheck sdk-test

## sdk-install: Install SDK dependencies.
sdk-install:
	cd sdk && pnpm install

## sdk-build: Build the SDK.
sdk-build:
	cd sdk && pnpm build

## sdk-typecheck: Typecheck the SDK.
sdk-typecheck:
	cd sdk && pnpm typecheck

## sdk-test: Run SDK tests.
sdk-test:
	cd sdk && pnpm test

## sdk-fmt: Format SDK code with oxfmt.
sdk-fmt:
	cd sdk && pnpm fmt

## sdk-fmt-check: Check SDK code formatting.
sdk-fmt-check:
	cd sdk && pnpm fmt:check

## sdk-lint: Lint SDK code with oxlint.
sdk-lint:
	cd sdk && pnpm lint

## up: Bootstrap local .tls/ if needed, build images, start Compose (mTLS on gRPC by default).
up:
	./scripts/run-locally.sh

## down: Stop and remove all local Compose services (same compose files as make up).
down:
	bash scripts/docker-compose-local.sh down
