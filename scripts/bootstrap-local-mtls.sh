#!/usr/bin/env bash
# Generate a private CA plus PEMs for:
# - API registration gRPC server + runner client (runner → API)
# - Runner SandboxControl gRPC server + API client (API → runner)
# Writes to the repo's .tls/ directory by default (gitignored).
#
# Skips generation if the full PEM set already exists unless SANDBOX_TLS_REGEN=1.
#
# Environment:
#   REPO_ROOT  — repository root (default: parent of scripts/)
#   OUT_DIR    — output directory (default: $REPO_ROOT/.tls)
#   API_DNS    — DNS SAN on the API gRPC server cert (default: n8n-sandbox-service-api-local)
#   CLIENT_DNS — DNS SAN on the runner registration client cert (default: sandbox-runner-mtls-client)
#   CONTROL_SANS — comma-separated DNS names on the runner control server cert (default: both local compose runner hostnames + localhost)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "$SCRIPT_DIR/.." && pwd)}"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/.tls}"
API_DNS="${API_DNS:-n8n-sandbox-service-api-local}"
CLIENT_DNS="${CLIENT_DNS:-sandbox-runner-mtls-client}"
CONTROL_API_CLIENT_DNS="${CONTROL_API_CLIENT_DNS:-sandbox-api-control-mtls-client}"
DEFAULT_CONTROL_SANS="n8n-sandbox-service-runner-dind-local-1,n8n-sandbox-service-runner-dind-local-2,localhost"
CONTROL_SANS="${CONTROL_SANS:-$DEFAULT_CONTROL_SANS}"

mkdir -p "$OUT_DIR"

tls_complete() {
	[[ -f "$OUT_DIR/ca.crt" ]] && [[ -f "$OUT_DIR/ca.key" ]] \
		&& [[ -f "$OUT_DIR/grpc-server.crt" ]] && [[ -f "$OUT_DIR/grpc-server.key" ]] \
		&& [[ -f "$OUT_DIR/grpc-client.crt" ]] && [[ -f "$OUT_DIR/grpc-client.key" ]] \
		&& [[ -f "$OUT_DIR/control-grpc-server.crt" ]] && [[ -f "$OUT_DIR/control-grpc-server.key" ]] \
		&& [[ -f "$OUT_DIR/control-grpc-api-client.crt" ]] && [[ -f "$OUT_DIR/control-grpc-api-client.key" ]]
}

if [[ "${SANDBOX_TLS_REGEN:-}" != "1" ]] && tls_complete; then
	echo "TLS material already present in $OUT_DIR (set SANDBOX_TLS_REGEN=1 to regenerate)"
	exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
	echo "openssl is required to bootstrap local mTLS"
	exit 1
fi

umask 077
echo "Writing mTLS material to $OUT_DIR (API SAN: DNS:$API_DNS, registration client SAN: DNS:$CLIENT_DNS)"

openssl genrsa -out "$OUT_DIR/ca.key" 4096
openssl req -new -x509 -days 3650 -key "$OUT_DIR/ca.key" -out "$OUT_DIR/ca.crt" \
	-subj "/O=n8n-sandbox-local/CN=sandbox-mtls-local-ca"

openssl genrsa -out "$OUT_DIR/grpc-server.key" 2048
openssl req -new -key "$OUT_DIR/grpc-server.key" -out "$OUT_DIR/grpc-server.csr" \
	-subj "/O=n8n-sandbox-local/CN=$API_DNS"

server_ext="$OUT_DIR/grpc-server.ext"
cat >"$server_ext" <<EOF
[v3_req]
subjectAltName=DNS:${API_DNS}
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in "$OUT_DIR/grpc-server.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$OUT_DIR/grpc-server.crt" -days 825 \
	-extfile "$server_ext" -extensions v3_req

openssl genrsa -out "$OUT_DIR/grpc-client.key" 2048
openssl req -new -key "$OUT_DIR/grpc-client.key" -out "$OUT_DIR/grpc-client.csr" \
	-subj "/O=n8n-sandbox-local/CN=$CLIENT_DNS"

client_ext="$OUT_DIR/grpc-client.ext"
cat >"$client_ext" <<EOF
[v3_req]
subjectAltName=DNS:${CLIENT_DNS}
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$OUT_DIR/grpc-client.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$OUT_DIR/grpc-client.crt" -days 825 \
	-extfile "$client_ext" -extensions v3_req

# Runner-hosted SandboxControl server cert (API dials this); SAN list must cover compose service DNS names.
IFS=',' read -r -a san_arr <<<"$CONTROL_SANS"
san_line=""
for s in "${san_arr[@]}"; do
	s="${s#"${s%%[![:space:]]*}"}"
	s="${s%"${s##*[![:space:]]}"}"
	[[ -z "$s" ]] && continue
	if [[ -n "$san_line" ]]; then
		san_line+=",DNS:$s"
	else
		san_line="DNS:$s"
	fi
done

openssl genrsa -out "$OUT_DIR/control-grpc-server.key" 2048
openssl req -new -key "$OUT_DIR/control-grpc-server.key" -out "$OUT_DIR/control-grpc-server.csr" \
	-subj "/O=n8n-sandbox-local/CN=runner-sandbox-control"

control_srv_ext="$OUT_DIR/control-grpc-server.ext"
cat >"$control_srv_ext" <<EOF
[v3_req]
subjectAltName=${san_line}
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in "$OUT_DIR/control-grpc-server.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$OUT_DIR/control-grpc-server.crt" -days 825 \
	-extfile "$control_srv_ext" -extensions v3_req

openssl genrsa -out "$OUT_DIR/control-grpc-api-client.key" 2048
openssl req -new -key "$OUT_DIR/control-grpc-api-client.key" -out "$OUT_DIR/control-grpc-api-client.csr" \
	-subj "/O=n8n-sandbox-local/CN=$CONTROL_API_CLIENT_DNS"

control_cli_ext="$OUT_DIR/control-grpc-api-client.ext"
cat >"$control_cli_ext" <<EOF
[v3_req]
subjectAltName=DNS:${CONTROL_API_CLIENT_DNS}
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$OUT_DIR/control-grpc-api-client.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$OUT_DIR/control-grpc-api-client.crt" -days 825 \
	-extfile "$control_cli_ext" -extensions v3_req

rm -f "$OUT_DIR"/*.csr "$OUT_DIR"/*.srl "$OUT_DIR"/*.ext
chmod 644 "$OUT_DIR/ca.crt" "$OUT_DIR/grpc-server.crt" "$OUT_DIR/grpc-client.crt" \
	"$OUT_DIR/control-grpc-server.crt" "$OUT_DIR/control-grpc-api-client.crt"
chmod 600 "$OUT_DIR/ca.key" "$OUT_DIR/grpc-server.key" "$OUT_DIR/grpc-client.key" \
	"$OUT_DIR/control-grpc-server.key" "$OUT_DIR/control-grpc-api-client.key"

echo "Done."
