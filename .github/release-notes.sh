#!/usr/bin/env bash
# Writes the notes for a new release to stdout, for the "Release" workflow:
#
#     .github/release-notes.sh TAG [MILESTONE]
#
# Features and bug fixes come from the issues the release finishes: those of MILESTONE when it's
# given, or else those closed since the last release. Tasks (label "task") are features and bugs
# (label "bug") are bug fixes. Issues closed as not planned are left out, and so are those labeled
# "internal", which are about how Carryon is built, tested or released rather than what it does.
# Requirements, installation and a link to all changes since the last release come last. Needs gh,
# with GH_TOKEN and GITHUB_REPOSITORY set.
#
# GitHub shows a line break inside a paragraph as a break, so every paragraph is a single line.
set -euo pipefail

tag=$1 milestone=${2:-}
repo=$GITHUB_REPOSITORY
version=${tag#v}

# The last release that isn't a pre-release, and when it was published.
last=$(gh release list --repo "$repo" --exclude-drafts --exclude-pre-releases --limit 1 \
  --json tagName,publishedAt --jq '.[0] // empty | "\(.tagName) \(.publishedAt)"')
prev=${last% *} since=${last#* }

if [[ -n $milestone ]]; then
  scope="milestone:\"$milestone\""
else
  scope="closed:>=${since:-1970-01-01}"
fi

# section TITLE LABEL prints the issues with LABEL under TITLE, or nothing when there are none.
listed=0
section() {
  local lines
  lines=$(gh issue list --repo "$repo" --state closed --limit 200 \
    --search "is:closed reason:completed label:$2 -label:internal $scope" \
    --json number,title --jq 'sort_by(.number) | .[] | "- \(.title) (#\(.number))"')
  if [[ -n $lines ]]; then
    printf '## %s\n\n%s\n\n' "$1" "$lines"
    listed=1
  fi
}

if [[ $version == *-* ]]; then
  printf '> [!WARNING]\n> Pre-release for testing. Not recommended for everyday use.\n\n'
fi

section "Features" task
section "Bug fixes" bug
if (( ! listed )); then
  printf 'No user-facing changes. This release updates how Carryon is built and released.\n\n'
fi

if [[ -n $prev ]]; then
  changelog="[$prev...$tag](https://github.com/$repo/compare/$prev...$tag)"
else
  changelog="[$tag](https://github.com/$repo/commits/$tag)"
fi

cat <<EOF
## Requirements

- Linux on x86_64 or aarch64
- restic 0.17 or newer

## Installation

### Verify and extract

Download the archive for your architecture and \`sha256sums\`, then run:

\`\`\`sh
sha256sum --check --ignore-missing sha256sums
sudo tar -xzf carryon-$version-linux-x86_64.tar.gz -C / --strip-components=1 --no-overwrite-dir
\`\`\`

On aarch64, use \`carryon-$version-linux-aarch64.tar.gz\` instead.

### First installation

\`\`\`sh
sudo carryon init
\`\`\`

### Upgrade

\`\`\`sh
sudo systemctl daemon-reload
systemctl --user restart carryon-agent.service
\`\`\`

## Full changelog

$changelog
EOF
