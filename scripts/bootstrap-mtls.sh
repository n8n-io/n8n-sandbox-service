#!/usr/bin/env bash
# Generate a private CA plus leaf PEMs for mutual TLS between API and runners.
#
# Produces per-service subdirectories in the output directory:
#   ca.crt / ca.key                                  — Root CA (top-level)
#   api/ca.crt                                       — CA copy for API
#   api/grpc-server.crt / grpc-server.key            — API registration gRPC server
#   api/control-grpc-api-client.crt / ...key         — API SandboxControl gRPC client
#   runner/ca.crt                                    — CA copy for runners
#   runner/grpc-client.crt / grpc-client.key         — Runner registration gRPC client
#   runner/control-grpc-server.crt / ...key          — Runner SandboxControl gRPC server
#
# Requires openssl + bash.
# Skips generation when the full PEM set already exists (use --force to override).
#
# All options accept CLI flags (preferred), environment variables, or built-in
# defaults, in that priority order. Run with --help for the full interface.
set -euo pipefail

usage() {
	cat <<-'USAGE'
	Usage: bootstrap-mtls.sh [OPTIONS]

	Generate mTLS certificates for n8n-sandbox-service API ↔ runner communication.
	Certificates are organized into api/ and runner/ subdirectories.

	Options:
	  -o, --out-dir DIR              Output directory (default: ./.tls, env: OUT_DIR)
	  -n, --runners N                Number of runners; generates control SANs as
	                                   PREFIX-1,...,PREFIX-N,localhost
	                                   (default: 2, env: NUM_RUNNERS)
	      --api-san NAME             DNS SAN for API server cert
	                                   (default: sandbox-api, env: API_DNS)
	      --client-san NAME          DNS SAN for runner client cert
	                                   (default: sandbox-runner-client, env: CLIENT_DNS)
	      --control-san-prefix PFX   Prefix for auto-generated runner control SANs;
	                                   generates PFX-1,...,PFX-N,localhost
	                                   (default: runner, env: CONTROL_SAN_PREFIX)
	      --control-sans NAMES       Comma-separated DNS SANs for runner control server
	                                   cert; overrides --runners and --control-san-prefix
	                                   (env: CONTROL_SANS)
	      --control-client-san NAME  DNS SAN for API control client cert
	                                   (default: sandbox-api-control-client,
	                                   env: CONTROL_API_CLIENT_DNS)
	      --world-readable           Set key file permissions to 644 instead of 600
	      --force                    Force regeneration even if certs exist
	                                   (env: SANDBOX_TLS_REGEN=1)
	  -h, --help                     Show this help

	Examples:
	  # Local development with defaults (2 runners)
	  bootstrap-mtls.sh

	  # CI with explicit SANs
	  bootstrap-mtls.sh -o /tmp/tls --api-san my-api --control-sans "runner-a,runner-b"

	  # Production VM with 3 runners
	  bootstrap-mtls.sh -o /etc/sandbox/tls --runners 3

	  # Docker Compose init container with prefix-based SANs
	  bootstrap-mtls.sh --out-dir /tls --api-san my-api \
	      --control-san-prefix my-runner --runners 2
	USAGE
}

# --- Argument parsing (sets variables; unset vars fall through to env/default) ---

while [[ $# -gt 0 ]]; do
	case "$1" in
		-o|--out-dir)            OUT_DIR="$2";              shift 2 ;;
		-n|--runners)            NUM_RUNNERS="$2";          shift 2 ;;
		--api-san)               API_DNS="$2";              shift 2 ;;
		--client-san)            CLIENT_DNS="$2";           shift 2 ;;
		--control-san-prefix)    CONTROL_SAN_PREFIX="$2";   shift 2 ;;
		--control-sans)          CONTROL_SANS="$2";         shift 2 ;;
		--control-client-san)    CONTROL_API_CLIENT_DNS="$2"; shift 2 ;;
		--world-readable)        WORLD_READABLE=1;          shift ;;
		--force)                 FORCE=1;                   shift ;;
		-h|--help)               usage; exit 0 ;;
		*)
			echo "Unknown option: $1" >&2
			echo "Run with --help for usage." >&2
			exit 1
			;;
	esac
done

# --- Resolve final values: CLI arg (already set above) > env var > default ---

OUT_DIR="${OUT_DIR:-./.tls}"
NUM_RUNNERS="${NUM_RUNNERS:-2}"
API_DNS="${API_DNS:-sandbox-api}"
CLIENT_DNS="${CLIENT_DNS:-sandbox-runner-client}"
CONTROL_SAN_PREFIX="${CONTROL_SAN_PREFIX:-runner}"
CONTROL_API_CLIENT_DNS="${CONTROL_API_CLIENT_DNS:-sandbox-api-control-client}"
WORLD_READABLE="${WORLD_READABLE:-0}"
FORCE="${FORCE:-${SANDBOX_TLS_REGEN:-0}}"

API_DIR="$OUT_DIR/api"
RUNNER_DIR="$OUT_DIR/runner"

# --- Build control SANs from prefix + --runners if --control-sans was not provided ---

if [[ -z "${CONTROL_SANS:-}" ]]; then
	sans=""
	for i in $(seq 1 "$NUM_RUNNERS"); do
		[[ -n "$sans" ]] && sans+=","
		sans+="${CONTROL_SAN_PREFIX}-${i}"
	done
	CONTROL_SANS="${sans},localhost"
elif [[ "$CONTROL_SANS" != *localhost* ]]; then
	CONTROL_SANS="${CONTROL_SANS},localhost"
fi

# --- Idempotency: skip if all PEMs already exist ---

tls_complete() {
	[[ -f "$API_DIR/ca.crt" ]] \
		&& [[ -f "$API_DIR/grpc-server.crt" ]] && [[ -f "$API_DIR/grpc-server.key" ]] \
		&& [[ -f "$API_DIR/control-grpc-api-client.crt" ]] && [[ -f "$API_DIR/control-grpc-api-client.key" ]] \
		&& [[ -f "$RUNNER_DIR/ca.crt" ]] \
		&& [[ -f "$RUNNER_DIR/grpc-client.crt" ]] && [[ -f "$RUNNER_DIR/grpc-client.key" ]] \
		&& [[ -f "$RUNNER_DIR/control-grpc-server.crt" ]] && [[ -f "$RUNNER_DIR/control-grpc-server.key" ]]
}

if [[ "$FORCE" != "1" ]] && tls_complete; then
	echo "TLS material already present in $OUT_DIR (use --force to regenerate)"
	exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
	echo "Error: openssl is required but not found in PATH" >&2
	exit 1
fi

mkdir -p "$API_DIR" "$RUNNER_DIR"
umask 077

TMPDIR_WORK=$(mktemp -d)
trap 'rm -rf "$TMPDIR_WORK"' EXIT

echo "Generating mTLS material in $OUT_DIR"
echo "  API server SAN:      DNS:${API_DNS}"
echo "  Runner client SAN:   DNS:${CLIENT_DNS}"
echo "  Control server SANs: ${CONTROL_SANS}"
echo "  Control client SAN:  DNS:${CONTROL_API_CLIENT_DNS}"

# --- CA ---

openssl genrsa -out "$OUT_DIR/ca.key" 4096
openssl req -new -x509 -days 3650 -key "$OUT_DIR/ca.key" -out "$OUT_DIR/ca.crt" \
	-subj "/O=n8n-sandbox/CN=sandbox-mtls-ca"

cp "$OUT_DIR/ca.crt" "$API_DIR/ca.crt"
cp "$OUT_DIR/ca.crt" "$RUNNER_DIR/ca.crt"

# --- API registration gRPC server cert (serverAuth) ---

openssl genrsa -out "$API_DIR/grpc-server.key" 2048
openssl req -new -key "$API_DIR/grpc-server.key" -out "$TMPDIR_WORK/grpc-server.csr" \
	-subj "/O=n8n-sandbox/CN=$API_DNS"

cat >"$TMPDIR_WORK/grpc-server.ext" <<EOF
[v3_req]
subjectAltName=DNS:${API_DNS}
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in "$TMPDIR_WORK/grpc-server.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$API_DIR/grpc-server.crt" -days 825 \
	-extfile "$TMPDIR_WORK/grpc-server.ext" -extensions v3_req

# --- Runner registration gRPC client cert (clientAuth) ---

openssl genrsa -out "$RUNNER_DIR/grpc-client.key" 2048
openssl req -new -key "$RUNNER_DIR/grpc-client.key" -out "$TMPDIR_WORK/grpc-client.csr" \
	-subj "/O=n8n-sandbox/CN=$CLIENT_DNS"

cat >"$TMPDIR_WORK/grpc-client.ext" <<EOF
[v3_req]
subjectAltName=DNS:${CLIENT_DNS}
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$TMPDIR_WORK/grpc-client.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$RUNNER_DIR/grpc-client.crt" -days 825 \
	-extfile "$TMPDIR_WORK/grpc-client.ext" -extensions v3_req

# --- Runner SandboxControl gRPC server cert (serverAuth) ---

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

openssl genrsa -out "$RUNNER_DIR/control-grpc-server.key" 2048
openssl req -new -key "$RUNNER_DIR/control-grpc-server.key" -out "$TMPDIR_WORK/control-grpc-server.csr" \
	-subj "/O=n8n-sandbox/CN=runner-sandbox-control"

cat >"$TMPDIR_WORK/control-grpc-server.ext" <<EOF
[v3_req]
subjectAltName=${san_line}
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in "$TMPDIR_WORK/control-grpc-server.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$RUNNER_DIR/control-grpc-server.crt" -days 825 \
	-extfile "$TMPDIR_WORK/control-grpc-server.ext" -extensions v3_req

# --- API SandboxControl gRPC client cert (clientAuth) ---

openssl genrsa -out "$API_DIR/control-grpc-api-client.key" 2048
openssl req -new -key "$API_DIR/control-grpc-api-client.key" -out "$TMPDIR_WORK/control-grpc-api-client.csr" \
	-subj "/O=n8n-sandbox/CN=$CONTROL_API_CLIENT_DNS"

cat >"$TMPDIR_WORK/control-grpc-api-client.ext" <<EOF
[v3_req]
subjectAltName=DNS:${CONTROL_API_CLIENT_DNS}
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$TMPDIR_WORK/control-grpc-api-client.csr" \
	-CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
	-out "$API_DIR/control-grpc-api-client.crt" -days 825 \
	-extfile "$TMPDIR_WORK/control-grpc-api-client.ext" -extensions v3_req

# --- Cleanup and set permissions ---

rm -f "$OUT_DIR"/*.srl

KEY_MODE=600
if [[ "$WORLD_READABLE" == "1" ]]; then
	KEY_MODE=644
fi

chmod 644 "$OUT_DIR/ca.crt" \
	"$API_DIR/ca.crt" "$API_DIR/grpc-server.crt" "$API_DIR/control-grpc-api-client.crt" \
	"$RUNNER_DIR/ca.crt" "$RUNNER_DIR/grpc-client.crt" "$RUNNER_DIR/control-grpc-server.crt"
chmod 600 "$OUT_DIR/ca.key"
chmod "$KEY_MODE" \
	"$API_DIR/grpc-server.key" "$API_DIR/control-grpc-api-client.key" \
	"$RUNNER_DIR/grpc-client.key" "$RUNNER_DIR/control-grpc-server.key"
chmod 755 "$OUT_DIR" "$API_DIR" "$RUNNER_DIR"

echo "Done — mTLS material written to $OUT_DIR/{api,runner}"
