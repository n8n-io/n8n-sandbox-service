#!/usr/bin/env bash
# Idempotent shell equivalent of EnsureHostNAT in
# internal/runner/runtime/firecracker.ee/network/network.go
set -euo pipefail

maybe_sudo() {
	if [[ "$(id -u)" -eq 0 ]]; then
		"$@"
	else
		sudo "$@"
	fi
}

maybe_sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null

default_iface="$(ip route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -n 1)"
if [[ -z "$default_iface" ]]; then
	echo "ERROR: could not determine default network interface" >&2
	exit 1
fi

maybe_sudo iptables -t nat -C POSTROUTING -o "$default_iface" -j MASQUERADE 2>/dev/null \
	|| maybe_sudo iptables -t nat -A POSTROUTING -o "$default_iface" -j MASQUERADE
maybe_sudo iptables -C FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
	|| maybe_sudo iptables -A FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
maybe_sudo iptables -C FORWARD -i fc-veth+ -j ACCEPT 2>/dev/null \
	|| maybe_sudo iptables -A FORWARD -i fc-veth+ -j ACCEPT
