#!/usr/bin/env bash
set -euo pipefail

: "${MARKDOWN_FILE:?}" "${GITHUB_REPOSITORY:?}"

if [[ "${COMMENT:-true}" != "true" ]]; then
  echo "PR comment disabled (comment: ${COMMENT:-})."
  exit 0
fi
case "${GITHUB_EVENT_NAME:-}" in
  pull_request) ;;
  *)
    echo "Not a pull request event (${GITHUB_EVENT_NAME:-none}); the report is in the job summary."
    exit 0
    ;;
esac

event="${GITHUB_EVENT_PATH:?}"
pr="$(jq -r '.pull_request.number // empty' "$event")"
head_repo="$(jq -r '.pull_request.head.repo.full_name // empty' "$event")"
base_repo="$(jq -r '.pull_request.base.repo.full_name // empty' "$event")"
if [[ -z "$pr" ]]; then
  echo "::notice::Could not determine the pull request number; the zyg verify result is in the job summary."
  exit 0
fi
if [[ -z "$head_repo" || "$head_repo" != "$base_repo" ]]; then
  echo "::notice title=zyg verify::Fork pull request: the token is read-only, so no comment was posted. The result is in the job summary."
  exit 0
fi

marker="$(head -n 1 "$MARKDOWN_FILE")"
if [[ "$marker" != "<!-- zyg-verify "* ]]; then
  echo "::warning::The report has no zyg-verify marker line; not commenting. The result is in the job summary."
  exit 0
fi

# fail reports why commenting did not work and ends the script successfully.
fail() {
  if grep -qiE '403|resource not accessible|forbidden' <<<"$1"; then
    echo "::notice title=zyg verify::The token cannot comment on this pull request (needs pull-requests: write). The result is in the job summary."
  else
    echo "::warning title=zyg verify::Could not post the PR comment: $1. The result is in the job summary."
  fi
  exit 0
}

if ! comments="$(gh api --paginate "repos/$GITHUB_REPOSITORY/issues/$pr/comments?per_page=100" 2>&1)"; then
  fail "$comments"
fi
# Only reuse comments written by a bot or a collaborator, so a drive-by comment cannot capture the marker.
existing="$(jq -s -r --arg m "$marker" '
  add // []
  | map(select((.body // "" | split("\n")[0] | rtrimstr("\r")) == $m
      and (.user.type == "Bot" or (.author_association | IN("OWNER", "MEMBER", "COLLABORATOR")))))
  | first | .id // empty' <<<"$comments")"

if [[ -n "$existing" ]]; then
  if ! result="$(gh api -X PATCH "repos/$GITHUB_REPOSITORY/issues/comments/$existing" -F "body=@$MARKDOWN_FILE" 2>&1)"; then
    fail "$result"
  fi
  echo "Updated PR comment $existing."
else
  if ! result="$(gh api -X POST "repos/$GITHUB_REPOSITORY/issues/$pr/comments" -F "body=@$MARKDOWN_FILE" 2>&1)"; then
    fail "$result"
  fi
  echo "Created PR comment."
fi
