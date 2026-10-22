#!/usr/bin/env bash
# Writes the notes for a new release to stdout, for the "Release" workflow:
#
#     .github/release-notes.sh TAG [MILESTONE]
#
# What's new and what's fixed come from the issues the release finishes: those of MILESTONE when
# it's given, or else those closed since the last release. Tasks (label "task") are what's new and
# bugs (label "bug") what's fixed. Issues closed as not planned are left out, and so are those
# labeled "internal", which are about how Carryon is built, tested or released rather than what
# it does. How to install and a link to all changes since the last release come last. Needs gh,
# with GH_TOKEN and GITHUB_REPOSITORY set.
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

section "What's new" task
section "Fixed" bug
if (( ! listed )); then
  printf 'Nothing you would notice: this release changes how Carryon is built or released.\n\n'
fi

if [[ -n $prev ]]; then
  changes="https://github.com/$repo/compare/$prev...$tag"
else
  changes="https://github.com/$repo/commits/$tag"
fi

cat <<EOF
## Install

Check the tarball for your machine against \`sha256sums\`, then unpack it into \`/\`:

\`\`\`sh
sha256sum --check --ignore-missing sha256sums
sudo tar -xzf carryon-$version-linux-x86_64.tar.gz -C / --strip-components=1 --no-overwrite-dir
\`\`\`

New to Carryon? Set it up with \`sudo carryon init\`. It needs restic 0.17 or newer.

**All changes:** $changes
EOF
