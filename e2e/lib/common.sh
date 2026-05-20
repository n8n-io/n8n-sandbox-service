#!/usr/bin/env bash
# Shared helpers for e2e/run-*.sh (source from scripts in e2e/).
# Expects: set -euo pipefail in the caller.

# Maps host uname -m to Docker image arch tag (amd64/arm64).
e2e_docker_arch() {
	uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/'
}

# After bootstrap: certs readable by containers, keys owner-only.
e2e_normalize_tls_permissions() {
	local tls_dir=$1
	chmod 755 "$tls_dir"
	local f
	for f in "$tls_dir"/*.crt; do
		[[ -e "$f" ]] || continue
		chmod 644 "$f"
	done
	for f in "$tls_dir"/*.key; do
		[[ -e "$f" ]] || continue
		chmod 600 "$f"
	done
}

# Prepares the API container's runtime user and the host-side resources that
# user needs. Chowns the TLS dir to the image's default user when possible;
# otherwise falls back to --user host:host plus a bind-mounted data dir at
# /var/lib/n8n-sandbox-api so that UID can write the SQLite file.
# Sets globals API_DOCKER_USER and API_DATA_VOLUME_ARGS.
e2e_setup_api_container() {
	local tls_dir=$1 api_image=$2
	local uid gid
	read -r uid gid <<< "$(docker run --rm --entrypoint sh "$api_image" -c 'echo $(id -u) $(id -g)')"
	if chown -R "$uid:$gid" "$tls_dir" 2>/dev/null; then
		API_DOCKER_USER=()
		API_DATA_VOLUME_ARGS=()
		return 0
	fi
	echo "chown TLS dir to API image user $uid:$gid not permitted; using host UID for API container" >&2
	API_DOCKER_USER=(--user "$(id -u):$(id -g)")
	local data_dir="$tls_dir/api-data"
	mkdir -p "$data_dir"
	API_DATA_VOLUME_ARGS=(-v "$data_dir:/var/lib/n8n-sandbox-api")
}

e2e_bootstrap_mtls_maybe() {
	local project_dir=$1 tls_owned=$2 tls_dir=$3 api_dns=$4 control_sans=$5
	if [[ "$tls_owned" == "1" ]]; then
		echo "Bootstrapping e2e mTLS material..."
		OUT_DIR="$tls_dir" API_DNS="$api_dns" CONTROL_SANS="$control_sans" \
			bash "$project_dir/scripts/bootstrap-local-mtls.sh"
	else
		echo "Using shared e2e mTLS material from E2E_TLS_DIR..."
	fi
}

e2e_docker_network_create() {
	local name=$1
	if docker network create "$name" >/dev/null 2>&1; then
		return 0
	fi
	echo "Default Docker network pools exhausted; retrying with explicit subnet..." >&2
	local _try octet subnet
	for _try in $(seq 1 32); do
		octet=$((RANDOM % 256))
		subnet="10.240.${octet}.0/24"
		if docker network create --subnet "$subnet" "$name" >/dev/null 2>&1; then
			echo "Created network $name with subnet $subnet" >&2
			return 0
		fi
	done
	echo "Failed to create docker network $name after subnet retries" >&2
	return 1
}

e2e_wait_for_registry() {
	local port=$1
	local i
	for i in $(seq 1 30); do
		if curl -sf "http://localhost:${port}/v2/" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.5
	done
	echo "Registry on port $port failed to start within 15s" >&2
	return 1
}

e2e_wait_for_api_http() {
	local port=$1 container=$2
	local i
	echo "Waiting for API service..."
	for i in $(seq 1 60); do
		if curl -sf "http://localhost:${port}/healthz" >/dev/null 2>&1; then
			echo "API is ready."
			return 0
		fi
		if [[ "$i" -eq 60 ]]; then
			echo "API failed to start within 60s"
			docker logs "$container"
			exit 1
		fi
		sleep 1
	done
}

e2e_build_sdk_unless_skip() {
	local project_dir=$1
	if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
		echo "Building SDK..."
		make -C "$project_dir" sdk-install sdk-build
	fi
}

e2e_install_playwright_deps_if_needed() {
	local script_dir=$1
	cd "$script_dir"
	if [[ ! -d node_modules ]] || [[ ! -f node_modules/@n8n/sandbox-client/dist/index.js ]]; then
		echo "Installing dependencies..."
		pnpm install --frozen-lockfile
	fi
}
