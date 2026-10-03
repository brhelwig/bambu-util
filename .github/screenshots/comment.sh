#!/usr/bin/env bash
# Leaves one comment on the pull request saying which screens look different
# from the base branch, with a link to the run's artifact holding them.
#
# The images themselves stay in the artifact rather than the comment, so a
# pull request that touches every screen does not bury its own conversation.
set -euo pipefail

REPO="${GITHUB_REPOSITORY}"
CHANGED="${GITHUB_WORKSPACE}/shots/changed/changed.json"
count=$(jq '.changed | length' "$CHANGED")
total=$(jq '.total' "$CHANGED")
baseline=$(jq '.baseline' "$CHANGED")

marker="## Screenshots"
existing=$(gh api "repos/${REPO}/issues/${PR}/comments" --paginate --jq \
  ".[] | select(.user.login == \"github-actions[bot]\") | select(.body | startswith(\"${marker}\")) | .id" | sed -n 1p)

if [ "$count" = 0 ]; then
  # Nothing to show. Only touch an earlier comment, so a link to shots that no
  # longer match the branch does not linger; never start a new one to say so.
  if [ -z "$existing" ]; then
    echo "no screens changed; nothing to comment"
    exit 0
  fi
  {
    echo "$marker"
    echo
    echo "No screen looks different from \`${BASE_REF}\` as of \`${SHA:0:7}\`."
  } > /tmp/comment.md
else
  {
    echo "$marker"
    echo
    if [ "$baseline" = true ]; then
      echo "${count} of ${total} screens look different from \`${BASE_REF}\` as of \`${SHA:0:7}\`:"
    else
      echo "\`${BASE_REF}\` could not be captured to compare against, so all ${total} screens are included as of \`${SHA:0:7}\`:"
    fi
    echo
    jq -r '.changed[] | "- **\(.title)**" + (if .reason == "new" then " (new)" else "" end) + " — \(.note)"' "$CHANGED"
    echo
    echo "**[Download the screenshots](${ARTIFACT_URL})** — each changed screen, with the base"
    echo "branch's version beside it as \`*.base.png\`. Artifacts need a GitHub login and expire"
    echo "after ${RETENTION_DAYS} days."
    echo
    echo "<sub>Only the status endpoint is answered by the harness; the page and its script are"
    echo "the real ones. Notification support is stood in for on the settings shots, because"
    echo "headless Chromium refuses notifications outright.</sub>"
  } > /tmp/comment.md
fi

# One comment per pull request, edited in place, so a run of pushes does not
# bury the conversation.
if [ -n "$existing" ]; then
  gh api -X PATCH "repos/${REPO}/issues/comments/${existing}" -F body=@/tmp/comment.md >/dev/null
  echo "updated comment ${existing}"
else
  gh api -X POST "repos/${REPO}/issues/${PR}/comments" -F body=@/tmp/comment.md >/dev/null
  echo "posted a new comment"
fi
