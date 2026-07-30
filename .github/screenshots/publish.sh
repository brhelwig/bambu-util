#!/usr/bin/env bash
# Pushes the captured screenshots to an orphan branch and leaves one comment on
# the pull request pointing at them.
#
# The orphan branch exists because a comment cannot carry an image directly —
# it has to already be reachable by URL. Keeping it orphaned means these never
# appear in the history of anything that ships.
set -euo pipefail

BRANCH=screenshots
DIR="pr-${PR}/${SHA:0:7}"
REPO="${GITHUB_REPOSITORY}"

git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"

# Take the branch as it stands, or start one with no history behind it.
if git ls-remote --exit-code --heads origin "$BRANCH" >/dev/null 2>&1; then
  git fetch origin "$BRANCH" --depth 1
  git worktree add /tmp/shots-branch "origin/$BRANCH"
  git -C /tmp/shots-branch switch -c "$BRANCH" || git -C /tmp/shots-branch switch "$BRANCH"
else
  git worktree add --detach /tmp/shots-branch
  git -C /tmp/shots-branch checkout --orphan "$BRANCH"
  git -C /tmp/shots-branch rm -rf . >/dev/null 2>&1 || true
fi

mkdir -p "/tmp/shots-branch/${DIR}"
cp "${GITHUB_WORKSPACE}"/shots/*.png "/tmp/shots-branch/${DIR}/"

git -C /tmp/shots-branch add -A
if git -C /tmp/shots-branch diff --cached --quiet; then
  echo "screenshots unchanged; nothing to push"
else
  git -C /tmp/shots-branch commit -q -m "Screenshots for #${PR} at ${SHA:0:7}"
  git -C /tmp/shots-branch push -q origin "$BRANCH"
fi

raw() { echo "https://raw.githubusercontent.com/${REPO}/${BRANCH}/${DIR}/$1"; }

{
  echo "## Screenshots"
  echo
  echo "Every state the printer can report, rendered from this branch at \`${SHA:0:7}\`."
  echo "The page and its script are the real ones; only the status endpoint is answered"
  echo "by the harness, so each shot is the page reacting to a payload it cannot tell"
  echo "from a live printer. The camera card shows its genuine empty state — no footage"
  echo "is staged."
  echo
  echo "Headless Chromium refuses notifications outright, so the browser'"'"'s own"
  echo "notification support is stood in for on the settings shots. The card'"'"'s logic is"
  echo "the real one; only the browser underneath it is simulated."
  echo
  for f in "${GITHUB_WORKSPACE}"/shots/*.png; do
    name=$(basename "$f" .png)
    title=$(echo "${name#*-}" | tr '-' ' ')
    echo "### ${title^}"
    echo
    echo "<img src=\"$(raw "${name}.png")\" width=\"320\">"
    echo
  done
} > /tmp/comment.md

# One comment per pull request, edited in place, so a run of pushes does not
# bury the conversation.
marker="## Screenshots"
existing=$(gh api "repos/${REPO}/issues/${PR}/comments" --jq \
  ".[] | select(.user.login == \"github-actions[bot]\") | select(.body | startswith(\"${marker}\")) | .id" | head -1)

if [ -n "$existing" ]; then
  gh api -X PATCH "repos/${REPO}/issues/comments/${existing}" -F body=@/tmp/comment.md >/dev/null
  echo "updated comment ${existing}"
else
  gh api -X POST "repos/${REPO}/issues/${PR}/comments" -F body=@/tmp/comment.md >/dev/null
  echo "posted a new comment"
fi
