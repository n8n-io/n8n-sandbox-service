#!/usr/bin/env bash
# Shared helpers for e2e/run-*.sh (source from scripts in e2e/).
# Expects: set -euo pipefail in the caller.

# Maps host uname -m to Docker image arch tag (amd64/arm64).
e2e_docker_arch() {
	uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/'
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
		bash "$project_dir/scripts/bootstrap-mtls.sh" \
			--out-dir "$tls_dir" \
			--api-san "$api_dns" \
			--control-sans "$control_sans"
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

# Adds /etc/hosts entries for cross-VM Firecracker mTLS DNS names.
e2e_install_firecracker_hosts() {
	local control_private_ip=$1 peer_private_ip=$2
	local block="# n8n-sandbox-firecracker-e2e
${control_private_ip} sandbox-api-e2e-mtls runner-control-a
${peer_private_ip} runner-control-b"
	if grep -q 'n8n-sandbox-firecracker-e2e' /etc/hosts 2>/dev/null; then
		sudo sed -i '/# n8n-sandbox-firecracker-e2e/,+2d' /etc/hosts
	fi
	printf '%s\n' "$block" | sudo tee -a /etc/hosts >/dev/null
}

e2e_shell_quote() {
	printf '%q' "$1"
}

# Optional FIRECRACKER_* overrides forwarded to setup-firecracker-e2e-vm.sh.
e2e_firecracker_setup_remote_env() {
	local remote_env=""
	if [[ -n "${FIRECRACKER_VERSION:-}" ]]; then
		remote_env+=" FIRECRACKER_VERSION=$(e2e_shell_quote "$FIRECRACKER_VERSION")"
	fi
	if [[ -n "${FIRECRACKER_TARBALL_SHA256:-}" ]]; then
		remote_env+=" FIRECRACKER_TARBALL_SHA256=$(e2e_shell_quote "$FIRECRACKER_TARBALL_SHA256")"
	fi
	if [[ -n "${FIRECRACKER_CI_VERSION:-}" ]]; then
		remote_env+=" FIRECRACKER_CI_VERSION=$(e2e_shell_quote "$FIRECRACKER_CI_VERSION")"
	fi
	if [[ -n "${FIRECRACKER_E2E_ROOTFS_SIZE_MB:-}" ]]; then
		remote_env+=" FIRECRACKER_E2E_ROOTFS_SIZE_MB=$(e2e_shell_quote "$FIRECRACKER_E2E_ROOTFS_SIZE_MB")"
	fi
	printf '%s' "$remote_env"
}

e2e_pack_repo_tarball() {
	local dest=$1 project_dir=$2
	local GNUTAR
	GNUTAR=$(command -v gtar || command -v tar)
	COPYFILE_DISABLE=1 "$GNUTAR" czf "$dest" \
		--no-xattrs \
		--exclude=.git \
		--exclude='.DS_Store' \
		--exclude='._*' \
		--exclude=bin \
		--exclude=dist \
		--exclude=node_modules \
		--exclude='e2e/infra/.terraform' \
		--exclude='e2e/infra/*.tfstate*' \
		-C "$project_dir" .
}

# Builds an SSH ProxyCommand that reaches a jump host with an explicit identity.
# ProxyJump ignores the command-line -i identity for the jump hop, so we spell
# the jump connection out here to force it to use the ephemeral e2e key.
e2e_ssh_proxy_command() {
	local ssh_key=$1 admin=$2 jump_host=$3
	printf 'ssh -i "%s" -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -W %%h:%%p %s@%s' \
		"$ssh_key" "$admin" "$jump_host"
}

# SSH to a Firecracker e2e host. Pass jump_host to reach a peer over private IP.
e2e_ssh_firecracker_host() {
	local ssh_key=$1 admin=$2 host=$3 jump_host=$4
	shift 4
	local -a opts=(-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -i "$ssh_key")
	if [[ -n "$jump_host" ]]; then
		opts+=(-o "ProxyCommand=$(e2e_ssh_proxy_command "$ssh_key" "$admin" "$jump_host")")
	fi
	ssh "${opts[@]}" "${admin}@${host}" "$@"
}

e2e_scp_to_firecracker_host() {
	local ssh_key=$1 admin=$2 local_path=$3 remote_path=$4 host=$5 jump_host=$6
	local -a opts=(-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -i "$ssh_key")
	if [[ -n "$jump_host" ]]; then
		opts+=(-o "ProxyCommand=$(e2e_ssh_proxy_command "$ssh_key" "$admin" "$jump_host")")
	fi
	scp "${opts[@]}" "$local_path" "${admin}@${host}:${remote_path}"
}

e2e_scp_dir_to_firecracker_host() {
	local ssh_key=$1 admin=$2 local_path=$3 remote_path=$4 host=$5 jump_host=$6
	local -a opts=(-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6 -r -i "$ssh_key")
	if [[ -n "$jump_host" ]]; then
		opts+=(-o "ProxyCommand=$(e2e_ssh_proxy_command "$ssh_key" "$admin" "$jump_host")")
	fi
	scp "${opts[@]}" "$local_path" "${admin}@${host}:${remote_path}"
}

# Wait for SSH, transfer the repo, and run setup-firecracker-e2e-vm.sh on one host.
e2e_setup_firecracker_host() {
	local label=$1 ssh_key=$2 admin=$3 ssh_host=$4 jump_host=$5 repo_tgz=$6 remote_env=$7

	echo "==> [${label}] Waiting for SSH on ${ssh_host}..."
	local i
	for i in $(seq 1 60); do
		if e2e_ssh_firecracker_host "$ssh_key" "$admin" "$ssh_host" "$jump_host" echo ready 2>/dev/null; then
			echo "==> [${label}] SSH is available."
			break
		fi
		if [[ "$i" -eq 60 ]]; then
			echo "==> [${label}] SSH failed after 3 minutes. Last attempt:" >&2
			e2e_ssh_firecracker_host "$ssh_key" "$admin" "$ssh_host" "$jump_host" echo ready || true
			return 1
		fi
		echo "==> [${label}] Waiting for SSH... ($i/60)"
		sleep 3
	done

	echo "==> [${label}] Transferring code..."
	e2e_scp_to_firecracker_host "$ssh_key" "$admin" "$repo_tgz" "/tmp/repo.tar.gz" "$ssh_host" "$jump_host"
	e2e_ssh_firecracker_host "$ssh_key" "$admin" "$ssh_host" "$jump_host" \
		"mkdir -p ~/project && tar xzf /tmp/repo.tar.gz -C ~/project && rm /tmp/repo.tar.gz"

	echo "==> [${label}] Running Firecracker VM setup..."
	# shellcheck disable=SC2086
	e2e_ssh_firecracker_host "$ssh_key" "$admin" "$ssh_host" "$jump_host" \
		"${remote_env} bash ~/project/e2e/infra/scripts/setup-firecracker-e2e-vm.sh"
}

# SSH to the peer VM over its private IP via the control VM jump host.
e2e_ssh_peer() {
	local control_ip=$1 peer_private_ip=$2 ssh_key=$3 admin=$4
	shift 4
	e2e_ssh_firecracker_host "$ssh_key" "$admin" "$peer_private_ip" "$control_ip" "$@"
}

# SCP a local file to the peer VM via the control VM jump host.
e2e_scp_to_peer() {
	local control_ip=$1 peer_private_ip=$2 ssh_key=$3 admin=$4 local_path=$5 remote_path=$6
	e2e_scp_to_firecracker_host "$ssh_key" "$admin" "$local_path" "$remote_path" "$peer_private_ip" "$control_ip"
}

# SCP a directory tree to the peer VM via the control VM jump host.
e2e_scp_dir_to_peer() {
	local control_ip=$1 peer_private_ip=$2 ssh_key=$3 admin=$4 local_path=$5 remote_path=$6
	e2e_scp_dir_to_firecracker_host "$ssh_key" "$admin" "$local_path" "$remote_path" "$peer_private_ip" "$control_ip"
}

# Returns the TCP port from a host:port listen address.
e2e_addr_port() {
	local addr=$1
	echo "${addr##*:}"
}

# Best-effort release of listeners left from a prior e2e stack on the same VM.
e2e_kill_tcp_listeners() {
	local port
	for port in "$@"; do
		[[ -n "$port" ]] || continue
		sudo fuser -k "${port}/tcp" >/dev/null 2>&1 || true
	done
}

# Stops a root-owned background process started via sudo (runner-firecracker).
e2e_stop_supervised_pid() {
	local pid=$1
	[[ -n "$pid" ]] || return 0
	sudo kill -TERM "$pid" >/dev/null 2>&1 || true
	local i
	for i in $(seq 1 30); do
		if ! sudo kill -0 "$pid" >/dev/null 2>&1; then
			wait "$pid" >/dev/null 2>&1 || true
			return 0
		fi
		sleep 0.2
	done
	sudo kill -KILL "$pid" >/dev/null 2>&1 || true
	wait "$pid" >/dev/null 2>&1 || true
}

# Stops a process owned by the current user (API host process).
e2e_stop_pid() {
	local pid=$1
	[[ -n "$pid" ]] || return 0
	kill -TERM "$pid" >/dev/null 2>&1 || true
	local i
	for i in $(seq 1 30); do
		if ! kill -0 "$pid" >/dev/null 2>&1; then
			wait "$pid" >/dev/null 2>&1 || true
			return 0
		fi
		sleep 0.2
	done
	kill -KILL "$pid" >/dev/null 2>&1 || true
	wait "$pid" >/dev/null 2>&1 || true
}

# e2e_store_backend returns sqlite (default) or postgres from E2E_STORE.
e2e_store_backend() {
	echo "${E2E_STORE:-sqlite}"
}

# e2e_configure_api_store starts Postgres when E2E_STORE=postgres and sets
# API_STORE_ENV / API_DATA_VOLUME_ARGS for the API container(s).
e2e_configure_api_store() {
	local network=$1 postgres_container=$2
	API_STORE_ENV=()
	if [[ "$(e2e_store_backend)" != "postgres" ]]; then
		return 0
	fi

	E2E_POSTGRES_CONTAINER="$postgres_container"
	echo "Starting Postgres for API store ($E2E_POSTGRES_CONTAINER)..."
	docker run -d \
		--network "$network" \
		--name "$E2E_POSTGRES_CONTAINER" \
		-e POSTGRES_USER=sandbox \
		-e POSTGRES_PASSWORD=sandbox \
		-e POSTGRES_DB=sandbox \
		postgres:16-alpine >/dev/null

	e2e_wait_for_postgres "$E2E_POSTGRES_CONTAINER"

	API_DATA_VOLUME_ARGS=()
	API_STORE_ENV=(
		-e "SANDBOX_API_STORE=postgres"
		-e "SANDBOX_API_POSTGRES_HOST=$E2E_POSTGRES_CONTAINER"
		-e "SANDBOX_API_POSTGRES_PORT=5432"
		-e "SANDBOX_API_POSTGRES_USER=sandbox"
		-e "SANDBOX_API_POSTGRES_PASSWORD=sandbox"
		-e "SANDBOX_API_POSTGRES_DB=sandbox"
		-e "SANDBOX_API_POSTGRES_SSLMODE=disable"
	)
}

e2e_wait_for_postgres() {
	local container=$1
	local i
	for i in $(seq 1 30); do
		if docker exec "$container" pg_isready -U sandbox -d sandbox >/dev/null 2>&1; then
			echo "Postgres is ready."
			return 0
		fi
		sleep 1
	done
	echo "Postgres failed to start within 30s" >&2
	docker logs "$container" 2>&1 || true
	return 1
}

e2e_stop_postgres_container() {
	local container=${1:-${E2E_POSTGRES_CONTAINER:-}}
	[[ -n "$container" ]] || return 0
	docker stop "$container" >/dev/null 2>&1 || true
	docker rm "$container" >/dev/null 2>&1 || true
}

# e2e_start_api_grpc_proxy runs nginx TCP passthrough to two API pods (simulates a k8s Service).
# The proxy gets network-alias $alias so runners can dial a stable gRPC address.
e2e_start_api_grpc_proxy() {
	local network=$1 proxy_name=$2 alias=$3 api1=$4 api2=$5 conf_dir=$6
	cat >"$conf_dir/grpc-proxy.conf" <<EOF
events {
	worker_connections 1024;
}
stream {
	upstream api_grpc {
		server ${api1}:9090 max_fails=1 fail_timeout=5s;
		server ${api2}:9090 max_fails=1 fail_timeout=5s;
	}
	server {
		listen 9090;
		proxy_pass api_grpc;
		proxy_connect_timeout 5s;
	}
}
EOF
	docker rm -f "$proxy_name" >/dev/null 2>&1 || true
	docker run -d \
		--network "$network" \
		--network-alias "$alias" \
		--name "$proxy_name" \
		-v "$conf_dir/grpc-proxy.conf:/etc/nginx/nginx.conf:ro" \
		nginx:1.27-alpine >/dev/null
}

# Sets E2E_API_CONTAINER_ENV_ARGS with shared API container docker args (TLS, keys,
# optional idle TTL, store backend). Caller appends: API_DOCKER_RUN+=("${E2E_API_CONTAINER_ENV_ARGS[@]}")
e2e_api_container_env_args() {
	local api_key=$1 reg_token=$2 runner_api_key=$3 tls_dir=$4
	E2E_API_CONTAINER_ENV_ARGS=(
		-v "$tls_dir/api:/grpc-tls:ro"
		-e "SANDBOX_API_KEYS=$api_key"
		-e "SANDBOX_API_METRICS_ENABLED=true"
		-e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$reg_token"
		-e "SANDBOX_API_RUNNER_API_KEY=$runner_api_key"
		-e "SANDBOX_API_GRPC_TLS_CERT_FILE=/grpc-tls/grpc-server.crt"
		-e "SANDBOX_API_GRPC_TLS_KEY_FILE=/grpc-tls/grpc-server.key"
		-e "SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt"
		-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/grpc-tls/ca.crt"
		-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-api-client.crt"
		-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-api-client.key"
	)
	if ((${#API_STORE_ENV[@]})); then
		E2E_API_CONTAINER_ENV_ARGS+=("${API_STORE_ENV[@]}")
	fi
	if ((${#API_IDLE_ENV[@]})); then
		E2E_API_CONTAINER_ENV_ARGS+=("${API_IDLE_ENV[@]}")
	fi
}
