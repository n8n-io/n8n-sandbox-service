#!/usr/bin/env bash
# Installs Docker, sysbox, Go, Node.js, and pnpm on a fresh Ubuntu VM.
# Intended to run ON the VM (not on the CI runner). Expects the project
# source to already be present at ~/project.
set -euxo pipefail

DOCKER_VERSION="5:29.4.1-1~ubuntu.24.04~noble"
# The go line in go.mod, the version CI builds with.
GO_VERSION="$(awk '$1 == "go" { print $2; exit }' ~/project/go.mod)"

# GO_VERSION goes into a download URL whose archive root unpacks into /usr/local.
if [[ ! "$GO_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "ERROR: go.mod must name a Go release such as 1.27.1; got '${GO_VERSION}'" >&2
	exit 1
fi

echo "==> Installing Docker ${DOCKER_VERSION}..."

sudo apt-get update -qq
sudo apt-get install -y -qq ca-certificates curl

sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
	-o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

# shellcheck source=/dev/null  # /etc/os-release only exists on the target VM
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
	https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" |
	sudo tee /etc/apt/sources.list.d/docker.list >/dev/null

sudo apt-get update -qq
sudo apt-get install -y -qq \
	"docker-ce=${DOCKER_VERSION}" \
	"docker-ce-cli=${DOCKER_VERSION}" \
	containerd.io \
	docker-buildx-plugin

sudo usermod -aG docker "$USER"

docker --version

echo "==> Installing sysbox..."

cd ~/project
sudo bash scripts/setup-sysbox.sh

echo "==> Installing Go ${GO_VERSION}..."

curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz
# Single quotes on purpose: $HOME must stay literal in .bashrc.
# shellcheck disable=SC2016
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >>~/.bashrc
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
go version

echo "==> Installing Node.js 24 and pnpm 10..."

curl -fsSL https://deb.nodesource.com/setup_24.x | sudo -E bash -
sudo apt-get install -y -qq nodejs
node --version

sudo npm install -g pnpm@10
pnpm --version

echo "==> Installing build tools..."

sudo apt-get install -y -qq make openssl

echo "==> VM setup complete"
