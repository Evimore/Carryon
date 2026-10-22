#!/usr/bin/env bash
# Writes the notes for a new release to stdout, for the "Release" workflow:
#
#     .github/release-notes.sh TAG COMMIT [MILESTONE]
#
# What's new and what's fixed come from the issues the release finishes: those of MILESTONE when
# it's given, or else those closed since the last release. Tasks (label "task") are what's new and
# bugs (label "bug") what's fixed; issues closed as not planned are left out. How to install comes
# next, then GitHub's own notes, which list every merged pull request. Needs gh, with GH_TOKEN and
# GITHUB_REPOSITORY set.
set -euo pipefail

tag=$1 commit=$2 milestone=${3:-}
repo=$GITHUB_REPOSITORY
version=${tag#v}

if [[ -n $milestone ]]; then
  scope="milestone:\"$milestone\""
else
  since=$(gh release list --repo "$repo" --exclude-drafts --exclude-pre-releases --limit 1 \
    --json publishedAt --jq '.[0].publishedAt // empty')
  scope="closed:>=${since:-1970-01-01}"
fi

# section TITLE LABEL prints the issues with LABEL under TITLE, or nothing when there are none.
section() {
  local lines
  lines=$(gh issue list --repo "$repo" --state closed --limit 200 \
    --search "is:closed reason:completed label:$2 $scope" \
    --json number,title --jq 'sort_by(.number) | .[] | "- \(.title) (#\(.number))"')
  if [[ -n $lines ]]; then
    printf '## %s\n\n%s\n\n' "$1" "$lines"
  fi
}

section "What's new" task
section "Fixed" bug

cat <<EOF
## Install

Download the tarball for your machine and \`sha256sums\`, check them, and unpack the tarball into \`/\`:

\`\`\`sh
sha256sum --check --ignore-missing sha256sums
sudo tar -xzf carryon-$version-linux-x86_64.tar.gz -C / --strip-components=1 --no-overwrite-dir
\`\`\`

New to Carryon? Set up backups with \`sudo carryon init\`; it needs restic 0.17 or newer. Updating?
Then run \`sudo systemctl daemon-reload\` and \`systemctl --user restart carryon-agent.service\`.

EOF

gh api "repos/$repo/releases/generate-notes" -f tag_name="$tag" -f target_commitish="$commit" \
  --jq .body
