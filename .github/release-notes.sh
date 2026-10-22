#!/usr/bin/env bash
# Writes the notes for a new release to stdout, for the "Release" workflow. Run it from the root of
# the checkout the release is built from, with the release's archives in dist/:
#
#     .github/release-notes.sh TAG [MILESTONE]
#
# A version release opens with its milestone's description, as a summary. Features and bug fixes
# come from the issues the release finishes: those of MILESTONE when it's given, or else those
# closed since the last release. Tasks (label "task") are features and bugs (label "bug") are bug
# fixes. Issues closed as not planned are left out, and so are those labeled "internal", which are
# about how Carryon is built, tested or released rather than what it does.
#
# The requirements come from the code, so they never go stale: the distributions from the package
# manager backends in internal/pkgmgr, the architectures from the archives in dist/, and the
# oldest restic from MinVersion in internal/engine. Installation and a link to all changes since
# the last release come last. Needs gh and jq, with GH_TOKEN and GITHUB_REPOSITORY set.
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

# either A B C prints "A, B or C".
either() {
  local out=$1
  shift
  while (( $# > 1 )); do out+=", $1"; shift; done
  if (( $# )); then out+=" or $1"; fi
  printf '%s' "$out"
}

if [[ $version == *-* ]]; then
  printf '> [!WARNING]\n> Pre-release for testing. Not recommended for everyday use.\n\n'
fi

# A version's milestone describes its goal: that opens the notes, as a summary.
if [[ -n $milestone ]]; then
  summary=$(gh api "repos/$repo/milestones?state=all&per_page=100" |
    jq -r --arg title "$milestone" '.[] | select(.title == $title) | .description // empty' | tr -d '\r')
  if [[ -n $summary ]]; then printf '%s\n\n' "$summary"; fi
fi

section "Features" task
section "Bug fixes" bug
if (( ! listed )); then
  printf 'No user-facing changes. This release updates how Carryon is built and released.\n\n'
fi

# Requirements, from the code.
distros=()
for dir in internal/pkgmgr/*/; do
  case $(basename "$dir") in
    pacman) distros+=("Arch Linux and Arch-based distributions such as CachyOS, EndeavourOS and Manjaro") ;;
    apt) distros+=("Debian, Ubuntu and distributions based on them") ;;
    dnf) distros+=("Fedora and distributions based on it") ;;
    *) distros+=("Distributions that use $(basename "$dir")") ;;
  esac
done
# x86_64 first, since most people use it.
archs=()
for archive in dist/carryon-"$version"-linux-*.tar.gz; do
  [[ -e $archive ]] || continue
  archive=${archive##*-linux-}
  archive=${archive%.tar.gz}
  if [[ $archive == x86_64 ]]; then archs=("$archive" "${archs[@]}"); else archs+=("$archive"); fi
done
if (( ${#archs[@]} == 0 )); then archs=(x86_64); fi
restic=$(sed -n 's/^const MinVersion = "\(.*\)"$/\1/p' internal/engine/version.go)

printf '## Requirements\n\n'
if (( ${#distros[@]} == 1 )); then
  printf -- '- **Distribution:** %s\n' "${distros[0]}"
else
  printf -- '- **Distributions:**\n'
  printf -- '  - %s\n' "${distros[@]}"
fi
printf -- '- **Architecture:** %s\n' "$(either "${archs[@]}")"
printf -- '- **restic:** %s or newer\n\n' "$restic"

# Installation: a section for each architecture, with one complete block for each case, so nothing
# is left to piece together.
printf '## Installation\n\n'
printf 'Each architecture has its own archive. Upgrading keeps your settings and backups.\n\n'
for arch in "${archs[@]}"; do
  archive=carryon-$version-linux-$arch.tar.gz
  case $arch in
    x86_64) about="Most desktops and laptops, with an Intel or AMD processor." ;;
    aarch64) about="64-bit ARM computers, such as a Raspberry Pi 4 or 5." ;;
    *) about="" ;;
  esac
  printf '### %s\n\n' "$arch"
  if [[ -n $about ]]; then printf '%s\n\n' "$about"; fi
  cat <<EOF
Download \`$archive\` and \`sha256sums\` from the assets below.

#### New installation

Verify the download, install the files, then set up backups:

\`\`\`sh
sha256sum --check --ignore-missing sha256sums
sudo tar -xzf $archive -C / --strip-components=1 --no-overwrite-dir
sudo carryon init
\`\`\`

#### Upgrade

Verify the download, install the files over the old ones, then restart Carryon's services:

\`\`\`sh
sha256sum --check --ignore-missing sha256sums
sudo tar -xzf $archive -C / --strip-components=1 --no-overwrite-dir
sudo systemctl daemon-reload
systemctl --user restart carryon-agent.service
\`\`\`

EOF
done

if [[ -n $prev ]]; then
  printf '## Full changelog\n\n[%s...%s](https://github.com/%s/compare/%s...%s)\n' "$prev" "$tag" "$repo" "$prev" "$tag"
else
  printf '## Full changelog\n\n[%s](https://github.com/%s/commits/%s)\n' "$tag" "$repo" "$tag"
fi
