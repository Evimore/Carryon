#!/bin/bash
# Checks an installed file list against packaging/layout.txt: every expected file is there and
# nothing else is. Reads paths relative to / (or to DESTDIR), one per line, on standard input:
#
#   (cd "$DESTDIR" && find . -type f -o -type l) | sed 's|^\./||' | packaging/check-layout.sh
#   pacman -Qlq carryon-git | grep -v '/$' | sed 's|^/||' | packaging/check-layout.sh
set -euo pipefail

src=$(cd "$(dirname "$0")/.." && pwd)

expected=()
while IFS= read -r line; do
  [[ -z $line || $line == \#* ]] && continue
  expected+=("$line")
done <"$src/packaging/layout.txt"
for f in "$src"/dist/systemd/*; do
  expected+=("usr/lib/systemd/system/${f##*/}")
done
for f in "$src"/catalog/*.toml; do
  expected+=("usr/share/carryon/catalog/${f##*/}")
done

mapfile -t actual < <(grep -v '^$' | sort -u)
if [ ${#actual[@]} -eq 0 ]; then
  echo "check-layout: no files given" >&2
  exit 1
fi

status=0
for want in "${expected[@]}"; do
  found=
  for f in "${actual[@]}"; do
    # shellcheck disable=SC2053 # $want is a pattern on purpose
    if [[ $f == $want ]]; then found=1; break; fi
  done
  if [ -z "$found" ]; then
    echo "missing: $want" >&2
    status=1
  fi
done
for f in "${actual[@]}"; do
  ok=
  for want in "${expected[@]}"; do
    # shellcheck disable=SC2053
    if [[ $f == $want ]]; then ok=1; break; fi
  done
  if [ -z "$ok" ]; then
    echo "not in packaging/layout.txt: $f" >&2
    status=1
  fi
done

if [ $status -eq 0 ]; then
  echo "check-layout: all ${#actual[@]} files where docs/design.md says"
fi
exit $status
