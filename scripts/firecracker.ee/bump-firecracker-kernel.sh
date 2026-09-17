#!/usr/bin/env bash
# Moves the guest kernel pin in firecracker-ci-assets.sh to the newest kernel of
# the same line (e.g. 6.1.x) in the Firecracker CI bucket. Run by the
# bump-firecracker-kernel workflow; not shipped in the golden-build bundle.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_CI_ASSETS_BIN="${FIRECRACKER_CI_ASSETS_BIN:-${SCRIPT_DIR}/firecracker-ci-assets.sh}"
ARCH=x86_64
PR_BODY=""

usage() {
	cat >&2 <<EOF
Usage: $0 [--pr-body PATH]

Rewrites FIRECRACKER_CI_DEFAULT_KERNEL_VERSION / FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256
in ${FIRECRACKER_CI_ASSETS_BIN} when the bucket has a newer kernel of the pinned
line. With --pr-body, writes a Markdown summary with links to the evidence.
Writes changed/old/new to \$GITHUB_OUTPUT when set.
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--pr-body)
		PR_BODY="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage
		exit 1
		;;
	esac
done

# Guarded by BASH_SOURCE, so this only defines variables and functions.
# shellcheck source=scripts/firecracker.ee/firecracker-ci-assets.sh
source "$FIRECRACKER_CI_ASSETS_BIN"

current="$FIRECRACKER_CI_DEFAULT_KERNEL_VERSION"
line="${current%.*}"
prefix="firecracker-ci/${FIRECRACKER_CI_VERSION}/${ARCH}/vmlinux-"

keys="$(
	curl -fsSL "${FIRECRACKER_CI_S3_BASE}/?prefix=${prefix}&list-type=2" |
		tr '<' '\n' |
		sed -n 's#^Key>\(firecracker-ci/[^<]*\)#\1#p'
)"
candidates="$(grep -E "^${prefix}[^/]+$" <<<"$keys" | grep -v '\.config$' | sed 's#.*/##' | paste -sd ' ' -)"
newest="$(grep -E "^${prefix}${line//./\\.}\.[0-9]+$" <<<"$keys" | sort -V | tail -n 1)"
newest="${newest##*vmlinux-}"
if [[ -z "$newest" ]]; then
	echo "ERROR: no vmlinux-${line}.* under ${FIRECRACKER_CI_S3_BASE}/${prefix}" >&2
	exit 1
fi

emit() {
	if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
		printf 'changed=%s\nold=%s\nnew=%s\n' "$1" "$current" "$newest" >>"$GITHUB_OUTPUT"
	fi
}

if [[ "$newest" == "$current" ]]; then
	echo "unchanged (${current})"
	emit false
	exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
url="${FIRECRACKER_CI_S3_BASE}/${prefix}${newest}"

# Firecracker publishes the kernel config next to each vmlinux; its header names
# the version the binary was built from.
config_header="# Linux/x86 ${newest} Kernel Configuration"
curl -fsSL "${url}.config" -o "${work}/config"
if ! grep -qxF "$config_header" "${work}/config"; then
	echo "ERROR: ${url}.config does not declare '${config_header}'" >&2
	exit 1
fi

headers="$(curl -fsSI "$url" | tr -d '\r')"
last_modified="$(sed -n 's/^[Ll]ast-[Mm]odified: //p' <<<"$headers")"
size="$(sed -n 's/^[Cc]ontent-[Ll]ength: //p' <<<"$headers")"

echo "==> Downloading ${url}..."
curl -fsSL "$url" -o "${work}/vmlinux"
firecracker_ci_assets_require_elf_kernel "${work}/vmlinux"
sha256="$(sha256sum "${work}/vmlinux" | awk '{print $1}')"

sed \
	-e "s/^FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=.*/FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=${newest}/" \
	-e "s/^FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=.*/FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=${sha256}/" \
	"$FIRECRACKER_CI_ASSETS_BIN" >"${work}/assets.sh"
if ! grep -qx "FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=${newest}" "${work}/assets.sh" ||
	! grep -qx "FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=${sha256}" "${work}/assets.sh"; then
	echo "ERROR: failed to rewrite the pin in ${FIRECRACKER_CI_ASSETS_BIN}" >&2
	exit 1
fi
cat "${work}/assets.sh" >"$FIRECRACKER_CI_ASSETS_BIN"

if [[ -n "$PR_BODY" ]]; then
	cat >"$PR_BODY" <<EOF
Bumps the pinned Firecracker CI guest kernel ${current} -> ${newest}.

SHA-256 \`${sha256}\` computed from the artifact below; CI re-downloads and re-verifies it on linux/amd64.

Evidence:
- Artifact in Firecracker's CI bucket for ${FIRECRACKER_CI_VERSION} (what Firecracker's own ${FIRECRACKER_CI_VERSION} test suite boots):
  ${url}
  Last-Modified: ${last_modified}, ${size} bytes
- Its kernel config, header \`${config_header}\`:
  ${url}.config
- Amazon Linux source tag Firecracker builds from (\`resources/rebuild.sh\` picks \`microvm-kernel-<ver>-*.amzn2023\`):
  https://github.com/amazonlinux/linux/tags?q=microvm-kernel-${newest}
- Firecracker PRs mentioning the version:
  https://github.com/firecracker-microvm/firecracker/pulls?q=is%3Apr+${newest}
- Upstream stable release: https://cdn.kernel.org/pub/linux/kernel/v${newest%%.*}.x/ChangeLog-${newest}

Kernels currently under ${prefix%vmlinux-}: ${candidates}
EOF
fi

echo "${current} -> ${newest} (sha256 ${sha256})"
emit true
