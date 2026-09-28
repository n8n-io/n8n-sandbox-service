#!/usr/bin/env bash
# Moves the Firecracker release pin in firecracker-release.sh to the newest patch
# of the same MAJOR.MINOR line (e.g. v1.14.x) on GitHub Releases; never a minor
# or major move. Run by the firecracker-upstream workflow; not shipped in the
# golden-build bundle.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_RELEASE_BIN="${FIRECRACKER_RELEASE_BIN:-${SCRIPT_DIR}/firecracker-release.sh}"
GITHUB_REPO=firecracker-microvm/firecracker
PR_BODY=""

usage() {
	cat >&2 <<EOF
Usage: $0 [--pr-body PATH]

Rewrites FIRECRACKER_DEFAULT_VERSION / FIRECRACKER_DEFAULT_X86_64_SHA256 in
${FIRECRACKER_RELEASE_BIN} when GitHub has a newer patch release of the pinned
MAJOR.MINOR line. With --pr-body, writes a Markdown summary with links to the
evidence. Writes changed/old/new to \$GITHUB_OUTPUT when set.
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
# shellcheck source=scripts/firecracker.ee/firecracker-release.sh
source "$FIRECRACKER_RELEASE_BIN"

current="$FIRECRACKER_DEFAULT_VERSION"
line="${current%.*}"

# --paginate: Firecracker has well over one page of releases.
tags="$(
	gh api --paginate "repos/${GITHUB_REPO}/releases" \
		--jq '.[] | select(.draft == false and .prerelease == false) | .tag_name'
)"
# `|| true`: with pipefail a no-match grep would abort before the error below.
releases="$(grep -E "^${line//./\\.}\.[0-9]+$" <<<"$tags" | sort -V || true)"
candidates="$(paste -sd ' ' - <<<"$releases")"
newest="$(tail -n 1 <<<"$releases")"
if [[ -z "$newest" ]]; then
	echo "ERROR: no ${line}.* release in ${GITHUB_REPO}" >&2
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
# The pinned release has gone from the listing (yanked, marked pre-release, or a
# bad listing). Never turn that into a downgrade PR.
if [[ "$(printf '%s\n' "$current" "$newest" | sort -V | tail -n 1)" != "$newest" ]]; then
	echo "ERROR: newest ${line}.* release in ${GITHUB_REPO} is ${newest}, older than the pinned ${current}; refusing to downgrade" >&2
	exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tarball="firecracker-${newest}-${FIRECRACKER_RELEASE_ARCH}.tgz"
url="${FIRECRACKER_RELEASE_BASE_URL}/${newest}/${tarball}"
sha_url="${url}.sha256.txt"
release_url="https://github.com/${GITHUB_REPO}/releases/tag/${newest}"

# Upstream publishes the checksum next to the tarball ("<sha256>  <filename>").
# Pin that, not a locally computed digest.
curl -fsSL "$sha_url" -o "${work}/sha256.txt"
sha256="$(awk '{print $1; exit}' "${work}/sha256.txt")"
if ! [[ "$sha256" =~ ^[0-9a-f]{64}$ ]]; then
	echo "ERROR: unexpected checksum file format: ${sha_url}" >&2
	exit 1
fi

# Prove the published checksum matches the artifact before pinning it.
echo "==> Downloading ${url}..."
curl -fsSL "$url" -o "${work}/${tarball}"
if ! echo "${sha256}  ${work}/${tarball}" | sha256sum -c - >/dev/null; then
	echo "ERROR: Firecracker tarball SHA-256 mismatch: ${url} (expected ${sha256})" >&2
	exit 1
fi

sed \
	-e "s/^FIRECRACKER_DEFAULT_VERSION=.*/FIRECRACKER_DEFAULT_VERSION=${newest}/" \
	-e "s/^FIRECRACKER_DEFAULT_X86_64_SHA256=.*/FIRECRACKER_DEFAULT_X86_64_SHA256=${sha256}/" \
	"$FIRECRACKER_RELEASE_BIN" >"${work}/release.sh"
if ! grep -qx "FIRECRACKER_DEFAULT_VERSION=${newest}" "${work}/release.sh" ||
	! grep -qx "FIRECRACKER_DEFAULT_X86_64_SHA256=${sha256}" "${work}/release.sh"; then
	echo "ERROR: failed to rewrite the pin in ${FIRECRACKER_RELEASE_BIN}" >&2
	exit 1
fi
cat "${work}/release.sh" >"$FIRECRACKER_RELEASE_BIN"

if [[ -n "$PR_BODY" ]]; then
	cat >"$PR_BODY" <<EOF
Bumps the pinned Firecracker release ${current} -> ${newest}.

The new pin \`${sha256}\` is upstream's published checksum (${sha_url}). This
script downloaded the tarball and verified it against that value before
rewriting the pin; CI re-downloads the tarball against the pin. That proves the
tarball matches what upstream published, not that upstream is intact: before
merging, read the release notes for snapshot-format, API or boot changes and
label the PR \`e2e-firecracker\` so the Azure lane installs it, rebuilds the
golden snapshot and passes admission.

**Rollout.** A Firecracker bump restarts the runner, which drops stopped
sandboxes (they are never reattached on restart, by design). The golden
snapshot is rebuilt per the rollout order in \`BUNDLE.md\` (bundle first,
snapshot rebuilt, runner image last). The host binary that creates the snapshot
and the runner-image binary that restores it must be the same release, which
holds when bundle and image are built from the same commit: the pin file is
what ties them. Patch releases have changed the snapshot format before (1.16.2
did), so snapshot compatibility across this bump is not assumed.

**Evidence:**
- Release notes: ${release_url}
- Tarball: ${url}
- Checksum file: ${sha_url}

Releases currently in the ${line} line: ${candidates}
EOF
fi

echo "${current} -> ${newest} (sha256 ${sha256})"
emit true
