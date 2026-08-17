#!/usr/bin/env bash
# Writes the single release version that ties the API, runner, and sandbox images
# together: the VERSION file and the Helm chart's appVersion. Idempotent.
set -euo pipefail

VERSION="${1:?usage: set-release-version.sh <version>}"
# Reject rather than normalize (e.g. strip a leading "v"): this string becomes the
# published image tags and the chart's appVersion, and release validate accepts
# exactly this shape.
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "ERROR: version must be x.y.z, got '${VERSION}'" >&2
	exit 1
fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${ROOT}/charts/n8n-sandbox-service/Chart.yaml"

printf '%s' "$VERSION" >"${ROOT}/VERSION"

read_yaml_scalar() {
	sed -n "s/^$1: *\"\{0,1\}\([^\"]*\)\"\{0,1\}$/\1/p" "$CHART"
}

if [[ "$(read_yaml_scalar appVersion)" == "$VERSION" ]]; then
	exit 0
fi

# The chart version tracks chart packaging, so it needs its own bump whenever the
# chart starts shipping a new appVersion — Helm requires a unique chart version
# per publish.
chart_version="$(read_yaml_scalar version)"
IFS='.' read -r major minor patch <<<"$chart_version"
sed -i.bak \
	-e "s/^version: .*/version: \"${major}.${minor}.$((patch + 1))\"/" \
	-e "s/^appVersion: .*/appVersion: \"${VERSION}\"/" \
	"$CHART"
rm -f "${CHART}.bak"
