#!/usr/bin/env bash
# Create a branch commit via GitHub GraphQL (auto-signed) and open a PR.
#
# Usage:
#   create-signed-commit-pr.sh \
#     --base <base-branch> \
#     --branch <head-branch> \
#     --title <pr-title> \
#     --body <pr-body> | --body-file <path> \
#     --label <label> \
#     --message <commit-headline> \
#     -- <file>...
#
# Requires: gh, jq, base64. Auth via GH_TOKEN / GITHUB_TOKEN.
set -euo pipefail

BASE=""
BRANCH=""
TITLE=""
BODY=""
BODY_FILE=""
LABEL=""
MESSAGE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE="$2"; shift 2 ;;
    --branch) BRANCH="$2"; shift 2 ;;
    --title) TITLE="$2"; shift 2 ;;
    --body) BODY="$2"; shift 2 ;;
    --body-file) BODY_FILE="$2"; shift 2 ;;
    --label) LABEL="$2"; shift 2 ;;
    --message) MESSAGE="$2"; shift 2 ;;
    --) shift; break ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

if [[ -n "$BODY_FILE" ]]; then
  BODY="$(cat "$BODY_FILE")"
fi

if [[ -z "$BASE" || -z "$BRANCH" || -z "$TITLE" || -z "$BODY" || -z "$MESSAGE" || $# -lt 1 ]]; then
  echo "usage: $0 --base BASE --branch BRANCH --title TITLE (--body BODY|--body-file PATH) --label LABEL --message MESSAGE -- FILE..." >&2
  exit 2
fi

REPO="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

ref_sha() {
  local ref="$1"
  gh api "repos/${REPO}/git/ref/heads/${ref}" --jq .object.sha 2>/dev/null || true
}

base_sha="$(ref_sha "$BASE")"
if [[ -z "$base_sha" ]]; then
  echo "base branch not found: ${BASE}" >&2
  exit 1
fi

# Always start the PR branch from the current base tip so retries stay a
# single signed commit on latest base.
if [[ -n "$(ref_sha "$BRANCH")" ]]; then
  gh api "repos/${REPO}/git/refs/heads/${BRANCH}" \
    --method PATCH \
    -f sha="${base_sha}" \
    -F force=true \
    --jq .object.sha >/dev/null
else
  gh api "repos/${REPO}/git/refs" \
    --method POST \
    -f ref="refs/heads/${BRANCH}" \
    -f sha="${base_sha}" \
    --jq .object.sha >/dev/null
fi
head_sha="$base_sha"

additions='[]'
for path in "$@"; do
  if [[ ! -f "$path" ]]; then
    echo "file not found: ${path}" >&2
    exit 1
  fi
  contents="$(base64 -w0 <"$path")"
  additions="$(jq -nc \
    --argjson arr "$additions" \
    --arg path "$path" \
    --arg contents "$contents" \
    '$arr + [{path: $path, contents: $contents}]')"
done

response="$(
  jq -n \
    --arg repo "$REPO" \
    --arg branch "$BRANCH" \
    --arg oid "$head_sha" \
    --arg headline "$MESSAGE" \
    --argjson additions "$additions" \
    '{
      query: "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }",
      variables: {
        input: {
          branch: {
            repositoryNameWithOwner: $repo,
            branchName: $branch
          },
          message: { headline: $headline },
          fileChanges: { additions: $additions },
          expectedHeadOid: $oid
        }
      }
    }' | gh api graphql --input -
)"

oid="$(jq -r '.data.createCommitOnBranch.commit.oid // empty' <<<"$response")"
if [[ -z "$oid" ]]; then
  echo "createCommitOnBranch failed:" >&2
  jq . <<<"$response" >&2
  exit 1
fi

if ! gh pr view "$BRANCH" --repo "$REPO" --json number >/dev/null 2>&1; then
  args=(pr create --repo "$REPO" --base "$BASE" --head "$BRANCH" --title "$TITLE" --body "$BODY")
  if [[ -n "$LABEL" ]]; then
    args+=(--label "$LABEL")
  fi
  gh "${args[@]}"
fi
