#!/usr/bin/env bash
# Checks Firecracker's published GitHub security advisories against the release
# pinned in firecracker-release.sh, and whether the pinned MAJOR.MINOR line is
# still inside upstream's patch-release window (RELEASE_POLICY.md). Writes a
# Markdown report; the firecracker-upstream workflow runs this and turns a
# flagged report into an issue. Not shipped in the golden-build bundle.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_RELEASE_BIN="${FIRECRACKER_RELEASE_BIN:-${SCRIPT_DIR}/firecracker-release.sh}"
GITHUB_REPO=firecracker-microvm/firecracker
# Published advisories when this check was written. Fewer in the listing means
# an API anomaly rather than upstream withdrawing history, so it is flagged.
MIN_PUBLISHED_ADVISORIES=2
REPORT=""

# Advisories reviewed and cleared for the pinned line, one "GHSA-id  verdict"
# per entry. Acknowledging an advisory is a PR that adds it here; the check then
# reports it as assessed instead of flagging it. Empty today: both published
# advisories are decided by the patched-version rule below.
FIRECRACKER_ASSESSED_ADVISORIES=()

usage() {
	cat >&2 <<EOF
Usage: $0 [--report PATH]

Checks the published security advisories of ${GITHUB_REPO} against the
release pinned in ${FIRECRACKER_RELEASE_BIN} and whether the pinned
MAJOR.MINOR line is still inside upstream's patch-release window. Prints a
Markdown report to stdout; with --report, also writes it to PATH. Exits 0
whether or not something is flagged and writes flagged=true|false to
\$GITHUB_OUTPUT when set; non-zero only when gh or the tooling fails.
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--report)
		REPORT="$2"
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

pin="${FIRECRACKER_DEFAULT_VERSION#v}"
line="${pin%.*}"
line_re="^${line//./\\.}\.[0-9]+$"
today="$(date -u +%Y-%m-%d)"
now="$(date -u +%s)"
flagged=false

# Prints the ledger verdict for a GHSA id; nothing when it is not assessed.
ledger_verdict() {
	local entry
	# ${arr[@]+...}: an empty array is unbound under set -u in bash < 4.4.
	for entry in ${FIRECRACKER_ASSESSED_ADVISORIES[@]+"${FIRECRACKER_ASSESSED_ADVISORIES[@]}"}; do
		if [[ "${entry%%[[:space:]]*}" == "$1" ]]; then
			sed -E 's/^[^[:space:]]+[[:space:]]*//' <<<"$entry"
			return 0
		fi
	done
}

# One advisory per line: ghsa_id, cve_id, severity, html_url and every
# vulnerabilities[].patched_versions joined by ",". patched_versions is free
# text ("1.14.4, 1.15.1", "v1.14.1"); vulnerable_version_range is free-form and
# not parsed. --paginate: the listing may span pages.
advisories="$(
	gh api --paginate "repos/${GITHUB_REPO}/security-advisories?state=published" \
		--jq '.[] | [.ghsa_id, (.cve_id // "-"), (.severity // "-"), (.html_url // "-"), ([.vulnerabilities[]?.patched_versions | select(. != null)] | join(","))] | @tsv'
)"

# The fields land in Markdown that becomes an issue body, so only the shapes
# GitHub documents are accepted; anything else fails the run.
require_field() {
	if ! [[ "$2" =~ $3 ]]; then
		echo "ERROR: unexpected ${1} in advisory listing: ${2}" >&2
		exit 1
	fi
}

advisory_rows=""
advisory_total=0
advisory_flagged=0
while IFS=$'\t' read -r ghsa cve severity url patched; do
	[[ -n "$ghsa" ]] || continue
	require_field ghsa_id "$ghsa" '^GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}$'
	require_field cve_id "$cve" '^(CVE-[0-9]{4}-[0-9]+|-)$'
	require_field severity "$severity" '^[a-z_-]+$'
	require_field html_url "$url" '^https://github\.com/[A-Za-z0-9._/-]+$'
	advisory_total=$((advisory_total + 1))
	# Newest fix in the pinned line named by patched_versions, if any.
	# `|| true`: with pipefail a no-match grep would abort the script.
	newest="$(
		tr ',' '\n' <<<"$patched" |
			sed -E 's/^[[:space:]]*v?//; s/[[:space:]]*$//' |
			grep -E "$line_re" | sort -V | tail -n 1 || true
	)"
	verdict="$(ledger_verdict "$ghsa")"
	if [[ -n "$verdict" ]]; then
		status="assessed: ${verdict}"
	elif [[ -z "$newest" ]]; then
		status="**flagged**: no ${line} fix listed; check whether the pin is in the vulnerable range"
		advisory_flagged=$((advisory_flagged + 1))
	elif [[ "$(printf '%s\n' "$pin" "$newest" | sort -V | tail -n 1)" != "$pin" ]]; then
		status="**flagged**: fixed in ${newest}, pin is ${pin}"
		advisory_flagged=$((advisory_flagged + 1))
	else
		status="cleared: fixed in ${newest} <= pin"
	fi
	advisory_rows+="| [${ghsa}](${url}) | ${cve} | ${severity} | ${newest:--} | ${status} |"$'\n'
done <<<"$advisories"

if ((advisory_total < MIN_PUBLISHED_ADVISORIES)); then
	flagged=true
	advisory_section="**flagged**: the listing returned ${advisory_total} published advisories, fewer than the ${MIN_PUBLISHED_ADVISORIES} known when this check was written; check the API response before trusting this run."
	if ((advisory_total > 0)); then
		advisory_section+="

| Advisory | CVE | Severity | Fix in ${line} | Status |
| --- | --- | --- | --- | --- |
${advisory_rows}"
	fi
else
	advisory_section="| Advisory | CVE | Severity | Fix in ${line} | Status |
| --- | --- | --- | --- | --- |
${advisory_rows}"
	if ((advisory_flagged > 0)); then
		flagged=true
		advisory_section+="
**flagged**: ${advisory_flagged} of ${advisory_total} advisories."
	else
		advisory_section+="
nothing flagged: ${advisory_total} advisories, all cleared or assessed."
	fi
fi

# Every MAJOR.MINOR.0 release as "tag<TAB>epoch<TAB>date". fromdateiso8601 in
# jq, so no GNU date parsing is needed.
zero_releases="$(
	gh api --paginate "repos/${GITHUB_REPO}/releases" \
		--jq '.[] | select(.draft == false and .prerelease == false) | select(.tag_name | test("^v[0-9]+\\.[0-9]+\\.0$")) | "\(.tag_name)\t\(.published_at | fromdateiso8601)\t\(.published_at[:10])"'
)"
# "line<TAB>epoch<TAB>date", oldest line first.
lines_table="$(
	while IFS=$'\t' read -r tag epoch date; do
		[[ -n "$tag" ]] || continue
		tag="${tag#v}"
		printf '%s\t%s\t%s\n' "${tag%.0}" "$epoch" "$date"
	done <<<"$zero_releases" | sort -V
)"
newest_two="$(cut -f1 <<<"$lines_table" | tail -n 2)"
# Sorted ascending, so the last line seen per major is its newest minor.
newest_per_major="$(cut -f1 <<<"$lines_table" | awk -F. '{ newest[$1] = $0 } END { for (m in newest) print newest[m] }')"

year=$((365 * 86400))
half_year=$((183 * 86400))

# RELEASE_POLICY.md: a line gets patch releases while any clause holds.
line_in_window() {
	local l=$1 age=$2
	# (a) one of the two newest lines, for a year from its .0
	if ((age < year)) && grep -qxF "$l" <<<"$newest_two"; then
		return 0
	fi
	# (b) any line, for six months from its .0
	if ((age < half_year)); then
		return 0
	fi
	# (c) the newest minor of its major, for a year from its .0
	if ((age < year)) && grep -qxF "$l" <<<"$newest_per_major"; then
		return 0
	fi
	return 1
}

window_lines=""
pin_zero_date=""
pin_in_window=false
while IFS=$'\t' read -r l epoch date; do
	[[ -n "$l" ]] || continue
	in_window=false
	if line_in_window "$l" "$((now - epoch))"; then
		in_window=true
		# Prepend: newest line first in the report.
		window_lines="${l} (.0 on ${date})${window_lines:+, ${window_lines}}"
	fi
	if [[ "$l" == "$line" ]]; then
		pin_zero_date="$date"
		pin_in_window="$in_window"
	fi
done <<<"$lines_table"

# A minor or major move is never proposed automatically: the guest kernel comes
# from the per-line firecracker-ci/<line>/ bucket prefix, the snapshot format
# may change (bitcode bumps its major on any state change) and boot/API
# behaviour can change. This check surfaces it; the move is a manual PR.
manual_move="Moving lines is a manual change: it changes the guest kernel source, may change the snapshot format, and can change boot/API behaviour."
if [[ -z "$pin_zero_date" ]]; then
	flagged=true
	window_verdict="**flagged**: pinned line ${line} has no ${line}.0 release in the ${GITHUB_REPO} listing, so it cannot be placed in the window. ${manual_move}"
elif [[ "$pin_in_window" == true ]]; then
	window_verdict="pinned line ${line} (.0 on ${pin_zero_date}) is inside the window."
else
	flagged=true
	window_verdict="**flagged**: pinned line ${line} (.0 on ${pin_zero_date}) is outside the window. ${manual_move}"
fi

report="$(
	cat <<EOF
# Firecracker upstream check

Pinned release: ${FIRECRACKER_DEFAULT_VERSION} (line ${line}). Checked ${today}.

## Advisories

${advisory_section}

## Support window

Upstream patches (RELEASE_POLICY.md): the two newest MAJOR.MINOR lines for a year from their .0, any line for six months from its .0, the newest minor of each major for a year from its .0.

Lines in the window today: ${window_lines:-none}.

${window_verdict}

## Acknowledging

To clear an advisory, add its GHSA id and a one-line verdict to \`FIRECRACKER_ASSESSED_ADVISORIES\` in \`scripts/firecracker.ee/check-firecracker-advisories.sh\`. A line move is a manual PR.
EOF
)"

printf '%s\n' "$report"
if [[ -n "$REPORT" ]]; then
	printf '%s\n' "$report" >"$REPORT"
fi
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
	printf 'flagged=%s\n' "$flagged" >>"$GITHUB_OUTPUT"
fi
echo
echo "flagged: ${flagged}"
