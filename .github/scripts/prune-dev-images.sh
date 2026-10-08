#!/usr/bin/env bash
# Deletes old versions of the GHCR -dev packages that alpha and staging push to.
#
# A version is deleted when it is older than RETENTION_DAYS, is not among the
# KEEP_NEWEST newest tagged versions, and is not tagged alpha. A version that a
# kept image index lists (a platform image or an attestation) is never deleted.
# DRY_RUN=true lists what would be deleted and deletes nothing.
#
# GH_TOKEN (default: gh auth token) needs admin on the packages to delete them.
# In a workflow, GITHUB_TOKEN with packages:write has that in the repository
# whose workflows pushed them.
set -euo pipefail

ORG=n8n-io
# Fixed rather than passed in: the token that deletes these can usually delete
# the release packages, and other repositories' packages, as well. These are
# the packages .github/actions/dev-image-build-push pushes; change both together.
PACKAGES=(
	n8n-sandbox-service-api-dev
	n8n-sandbox-service-runner-dind-dev
	n8n-sandbox-service-runner-firecracker-dev
	n8n-sandbox-service-sandbox-dev
)
RETENTION_DAYS="${RETENTION_DAYS:-30}"
KEEP_NEWEST="${KEEP_NEWEST:-50}"
DRY_RUN="${DRY_RUN:-false}"

MANIFEST_TYPES='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

die() {
	echo "ERROR: $*" >&2
	exit 1
}

[[ $# -eq 0 ]] || die "usage: $0 (takes no arguments)"
[[ "$RETENTION_DAYS" =~ ^[1-9][0-9]*$ ]] || die "RETENTION_DAYS must be a positive integer"
[[ "$KEEP_NEWEST" =~ ^[1-9][0-9]*$ ]] || die "KEEP_NEWEST must be a positive integer"
[[ "$DRY_RUN" == true || "$DRY_RUN" == false ]] || die "DRY_RUN must be true or false"

GH_TOKEN="${GH_TOKEN:-$(gh auth token)}"
export GH_TOKEN

failures=0

# Prints a pull token for one package on ghcr.io.
registry_token() {
	printf 'user = "token:%s"\n' "$GH_TOKEN" |
		curl -fsS -K - "https://ghcr.io/token?service=ghcr.io&scope=repository:${ORG}/$1:pull" |
		jq -er '.token'
}

# Prints the digests an image index lists, and nothing for a single image.
index_children() {
	local pkg=$1 digest=$2 token=$3
	printf 'header = "Authorization: Bearer %s"\n' "$token" |
		curl -fsS -K - -H "Accept: ${MANIFEST_TYPES}" \
			"https://ghcr.io/v2/${ORG}/${pkg}/manifests/${digest}" |
		jq -r '.manifests[]?.digest'
}

prune_package() {
	local pkg=$1 versions plan kept token protected="" children digest deletions
	local id tagged tagged_failed=false

	versions=$(gh api --paginate "/orgs/${ORG}/packages/container/${pkg}/versions?per_page=100" \
		--jq '.[] | {id, digest: .name, created_at, tags: .metadata.container.tags}' | jq -s .)

	plan=$(jq --argjson days "$RETENTION_DAYS" --argjson keep "$KEEP_NEWEST" '
		(now - $days * 86400) as $cutoff
		| (map(select(.tags | length > 0)) | sort_by(.created_at, .id) | reverse | .[:$keep] | map(.id)) as $newest
		| map(. + {keep: (
			(.created_at | fromdateiso8601) >= $cutoff
			or (.id as $id | any($newest[]; . == $id))
			or any(.tags[]; . == "alpha")
		)})' <<<"$versions")

	if jq -e 'all(.keep)' <<<"$plan" >/dev/null; then
		echo "==> ${pkg}: $(jq length <<<"$plan") versions, deleting none"
		return 0
	fi

	# Reading every kept index before deleting anything means a registry error
	# stops the run here, rather than after a child of a kept image is gone.
	kept=$(jq -r '.[] | select(.keep) | .digest' <<<"$plan")
	token=$(registry_token "$pkg")
	while IFS= read -r digest; do
		[[ -n "$digest" ]] || continue
		children=$(index_children "$pkg" "$digest" "$token")
		protected+="${children}"$'\n'
	done <<<"$kept"

	# Tagged versions go first, so a failed delete can still hold back the
	# untagged versions it lists.
	deletions=$(jq --argjson protected "$(jq -Rn '[inputs | select(length > 0)]' <<<"$protected")" '
		[.[] | select((.keep | not) and (.digest as $d | any($protected[]; . == $d) | not))]
		| sort_by((.tags | length) == 0)' <<<"$plan")

	echo "==> ${pkg}: $(jq length <<<"$plan") versions, deleting $(jq length <<<"$deletions")"
	jq -r '.[] | "    \(.id) \(.digest) \(.created_at) [\(.tags | join(" "))]"' <<<"$deletions"
	if [[ "$DRY_RUN" == true ]]; then
		return 0
	fi

	while IFS=$'\t' read -r id tagged; do
		if [[ "$tagged" == false && "$tagged_failed" == true ]]; then
			echo "::warning::${pkg}: kept the untagged versions, since a tagged version that failed to delete may list them."
			break
		fi
		if ! gh api -X DELETE "/orgs/${ORG}/packages/container/${pkg}/versions/${id}" --silent; then
			echo "::error::${pkg}: could not delete version ${id}."
			failures=$((failures + 1))
			if [[ "$tagged" == true ]]; then
				tagged_failed=true
			fi
		fi
	done < <(jq -r '.[] | [.id, ((.tags | length) > 0)] | @tsv' <<<"$deletions")
}

for pkg in "${PACKAGES[@]}"; do
	prune_package "$pkg"
done

if [[ "$failures" -gt 0 ]]; then
	die "${failures} version(s) could not be deleted"
fi
