#!/usr/bin/env bash
# Moves the guest kernel pin in firecracker-ci-assets.sh to the newest kernel of
# the same line (e.g. 6.1.x) in Firecracker's weekly CI builds. Run by the
# bump-firecracker-kernel workflow; not shipped in the golden-build bundle.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_CI_ASSETS_BIN="${FIRECRACKER_CI_ASSETS_BIN:-${SCRIPT_DIR}/firecracker-ci-assets.sh}"
ARCH=x86_64
GITHUB_REPO=firecracker-microvm/firecracker
# Build dates searched for the pinned line, newest first: about a month of
# weekly builds. None of them carrying it means upstream stopped building that
# line.
SEARCH_DATES=6
# Build directory names: <date>-<12-hex Firecracker commit>-<n>.
BUILD_RE='^[0-9]{8}-[0-9a-f]{12}-[0-9]+$'
# Config lines the guest's protections rely on: the user namespace block in the
# boot args is a sysctl, and the guest loads no modules and runs no
# unprivileged BPF. A kernel without one of them is never pinned.
REQUIRED_CONFIG=(
	"CONFIG_PROC_SYSCTL=y"
	"# CONFIG_MODULES is not set"
	"CONFIG_BPF_UNPRIV_DEFAULT_OFF=y"
)
PR_BODY=""

usage() {
	cat >&2 <<EOF
Usage: $0 [--pr-body PATH]

Rewrites the kernel pin (version, SHA-256, path) in ${FIRECRACKER_CI_ASSETS_BIN}
when a CI build from the ${SEARCH_DATES} newest build dates has a newer kernel
of the pinned line.
With --pr-body, writes a Markdown summary with links to the evidence. Writes
changed/old/new to \$GITHUB_OUTPUT when set.
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
current_path="$FIRECRACKER_CI_DEFAULT_VMLINUX_PATH"
line="${current%.*}"
line_re="^${line//./\\.}\.[0-9]+$"

# Each listing is fetched in an assignment of its own, so a failed request
# aborts the run instead of silently dropping builds. A listing is one page of
# up to 1000 entries; one that does not fit would hide builds.
s3_list() {
	local listing
	# `|| return`: inside a command substitution bash clears errexit.
	listing="$(curl -fsSL "${FIRECRACKER_CI_S3_BASE}/?list-type=2&$1")" || return 1
	if grep -q '<IsTruncated>true</IsTruncated>' <<<"$listing"; then
		echo "ERROR: S3 listing for '$1' is truncated" >&2
		return 1
	fi
	printf '%s\n' "$listing"
}

listing="$(s3_list "prefix=firecracker-ci/&delimiter=/")"
# Dated build directories from the newest dates. The name starts with the build
# date, and the cut is by date so that every build of a day is kept: the rest
# of the name says nothing about order, which upload time settles below.
# `|| true`: with pipefail a no-match grep would abort before the error below.
builds="$(
	tr '<' '\n' <<<"$listing" |
		sed -n 's#^Prefix>firecracker-ci/\([^/]*\)/$#\1#p' |
		grep -E "$BUILD_RE" | sort -r |
		awk -F '-' -v n="$SEARCH_DATES" '!seen[$1]++ { dates++ } dates <= n' || true
)"
if [[ -z "$builds" ]]; then
	echo "ERROR: no dated CI builds under ${FIRECRACKER_CI_S3_BASE}/firecracker-ci/" >&2
	exit 1
fi

# "build<TAB>kernel version<TAB>upload time" for every vmlinux in those builds.
# The bucket lists Key before LastModified within each entry.
kernels=""
while read -r build; do
	prefix="firecracker-ci/${build}/${ARCH}/vmlinux-"
	listing="$(s3_list "prefix=${prefix}")"
	kernels+="$(
		tr '<' '\n' <<<"$listing" |
			awk -v prefix="$prefix" -v build="$build" '
				/^Key>/ { key = substr($0, 5); next }
				/^LastModified>/ && index(key, prefix) == 1 {
					print build "\t" substr(key, length(prefix) + 1) "\t" substr($0, 14)
				}
			'
	)"$'\n'
done <<<"$builds"

# The newest kernel of the line; of the builds carrying it, the latest upload.
# ENVIRON rather than -v: awk would read the regex's backslashes as escapes.
pick="$(
	LINE_RE="$line_re" awk -F '\t' '$2 ~ ENVIRON["LINE_RE"]' <<<"$kernels" |
		sort -t $'\t' -k2,2V -k3,3 | tail -n 1
)"
if [[ -z "$pick" ]]; then
	echo "ERROR: no vmlinux-${line}.* in the CI builds of the ${SEARCH_DATES} newest dates ($(paste -sd ' ' - <<<"$builds")); upstream may have stopped building the ${line} line" >&2
	exit 1
fi
IFS=$'\t' read -r newest_build newest _ <<<"$pick"

emit() {
	if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
		printf 'changed=%s\nold=%s\nnew=%s\n' "$1" "$current" "$newest" >>"$GITHUB_OUTPUT"
	fi
}

# The version alone decides: a weekly rebuild of the same kernel is not a bump.
if [[ "$newest" == "$current" ]]; then
	echo "unchanged (${current})"
	emit false
	exit 0
fi
# The newest builds carry an older kernel than the pin (rolled back, or a bad
# listing). Never turn that into a downgrade PR.
if [[ "$(printf '%s\n' "$current" "$newest" | sort -V | tail -n 1)" != "$newest" ]]; then
	echo "ERROR: newest ${line}.* kernel in the CI builds is ${newest} (${newest_build}), older than the pinned ${current}; refusing to downgrade" >&2
	exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
new_path="firecracker-ci/${newest_build}/${ARCH}/vmlinux-${newest}"
url="${FIRECRACKER_CI_S3_BASE}/${new_path}"
changelog_url="https://cdn.kernel.org/pub/linux/kernel/v${newest%%.*}.x/ChangeLog-${newest}"
# The middle field of the build name is the Firecracker commit it was built from.
commit="${newest_build#*-}"
commit="${commit%-*}"
commit_url="https://github.com/${GITHUB_REPO}/commit/${commit}"
# Everything from the bucket that reaches the PR body is format-checked first:
# the body is what a reviewer reads to decide.
candidates="$(
	awk -F '\t' -v build="$newest_build" '$1 == build && $2 ~ /^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9-]+)?$/ { print $2 }' <<<"$kernels" |
		sort -V | paste -sd ' ' -
)"

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
for want in "${REQUIRED_CONFIG[@]}"; do
	if ! grep -qxF "$want" "${work}/config"; then
		echo "ERROR: ${url}.config lacks '${want}'" >&2
		exit 1
	fi
done

# The diff against the pinned kernel's config goes into the PR body. Only
# config lines, without backticks, so nothing can close the code fence.
config_lines() {
	grep -E '^(CONFIG_[A-Z0-9_]+=.*|# CONFIG_[A-Z0-9_]+ is not set)$' "$1" | tr -d '`' | sort || true
}
curl -fsSL "${FIRECRACKER_CI_S3_BASE}/${current_path}.config" -o "${work}/current.config"
config_diff="$(diff <(config_lines "${work}/current.config") <(config_lines "${work}/config") | grep -E '^[<>]' | head -n 500 || true)"

headers="$(curl -fsSI "$url" | tr -d '\r')"
last_modified="$(sed -n 's/^[Ll]ast-[Mm]odified: //p' <<<"$headers")"
size="$(sed -n 's/^[Cc]ontent-[Ll]ength: //p' <<<"$headers")"
if ! [[ "$size" =~ ^[0-9]+$ && "$last_modified" =~ ^[A-Z][a-z]{2},\ [0-9]{2}\ [A-Z][a-z]{2}\ [0-9]{4}\ [0-9:]{8}\ GMT$ ]]; then
	echo "ERROR: unexpected Content-Length or Last-Modified for ${url}" >&2
	exit 1
fi

echo "==> Downloading ${url}..."
curl -fsSL "$url" -o "${work}/vmlinux"
firecracker_ci_assets_require_elf_kernel "${work}/vmlinux"
# The banner the kernel prints at boot names the version it was built from.
if ! grep -aqF "Linux version ${newest} " "${work}/vmlinux"; then
	echo "ERROR: ${url} does not carry the 'Linux version ${newest}' banner" >&2
	exit 1
fi
sha256="$(sha256sum "${work}/vmlinux" | awk '{print $1}')"

# `#` as the delimiter: the path has slashes. Every value is format-checked above.
sed \
	-e "s#^FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=.*#FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=${newest}#" \
	-e "s#^FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=.*#FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=${sha256}#" \
	-e "s#^FIRECRACKER_CI_DEFAULT_VMLINUX_PATH=.*#FIRECRACKER_CI_DEFAULT_VMLINUX_PATH=${new_path}#" \
	"$FIRECRACKER_CI_ASSETS_BIN" >"${work}/assets.sh"
if ! grep -qxF "FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=${newest}" "${work}/assets.sh" ||
	! grep -qxF "FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=${sha256}" "${work}/assets.sh" ||
	! grep -qxF "FIRECRACKER_CI_DEFAULT_VMLINUX_PATH=${new_path}" "${work}/assets.sh"; then
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
Firecracker PR that introduced the version, the same version in Amazon Linux), and
read the config diff below: weekly builds come from Firecracker's \`main\`, so the
guest config can change between them. Label the PR \`e2e-firecracker\` so the
Azure lane boots the new kernel, rebuilds the golden snapshot and runs the suite.

Evidence:
- Artifact in Firecracker's weekly CI build ${newest_build}:
  ${url}
  Last-Modified: ${last_modified}, ${size} bytes
- Firecracker commit the build was made from: ${commit_url}
- Its kernel config, header \`${config_header}\`:
  ${url}.config
- Amazon Linux source tag Firecracker builds from (\`resources/rebuild.sh\` picks \`microvm-kernel-<ver>-*.amzn2023\`):
  https://github.com/amazonlinux/linux/tags?q=microvm-kernel-${newest}
- Firecracker PRs mentioning the version:
  https://github.com/${GITHUB_REPO}/pulls?q=is%3Apr+${newest}
- Upstream stable release (existence checked by this script): ${changelog_url}

Kernels in ${newest_build}: ${candidates}

<details><summary>Config diff, ${current} (&lt;) to ${newest} (&gt;)</summary>

\`\`\`diff
${config_diff:-no config changes}
\`\`\`

</details>
EOF
fi

echo "${current} -> ${newest} (${new_path}, sha256 ${sha256})"
emit true
