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
ci_version="$FIRECRACKER_CI_DEFAULT_VERSION"
prefix="firecracker-ci/${ci_version}/${ARCH}/vmlinux-"

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
changelog_url="https://cdn.kernel.org/pub/linux/kernel/v${newest%%.*}.x/ChangeLog-${newest}"

# The bucket cannot vouch for itself, so require the version to exist upstream
# before trusting anything else about the artifact.
if ! curl -fsSI "$changelog_url" -o /dev/null; then
	echo "ERROR: ${newest} is not a stable release on kernel.org (${changelog_url})" >&2
	exit 1
fi

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

**Review is the gate.** Firecracker publishes no checksum or signature for these
kernels, so the new pin \`${sha256}\` is the SHA-256 of the artifact as the bucket
served it today: it freezes that artifact, it does not prove its origin. CI
re-downloads it against the pin, which only catches the object changing after this
run. Before merging, check the evidence below is consistent (config header, a
Firecracker PR that introduced the version, the same version in Amazon Linux).

Evidence:
- Artifact in Firecracker's CI bucket for ${ci_version} (what Firecracker's own ${ci_version} test suite boots):
  ${url}
  Last-Modified: ${last_modified}, ${size} bytes
- Its kernel config, header \`${config_header}\`:
  ${url}.config
- Amazon Linux source tag Firecracker builds from (\`resources/rebuild.sh\` picks \`microvm-kernel-<ver>-*.amzn2023\`):
  https://github.com/amazonlinux/linux/tags?q=microvm-kernel-${newest}
- Firecracker PRs mentioning the version:
  https://github.com/firecracker-microvm/firecracker/pulls?q=is%3Apr+${newest}
- Upstream stable release (existence checked by this script): ${changelog_url}

Kernels currently under ${prefix%vmlinux-}: ${candidates}
EOF
fi

echo "${current} -> ${newest} (sha256 ${sha256})"
emit true
