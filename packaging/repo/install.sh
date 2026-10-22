#!/usr/bin/env bash
# Installs Carryon from its own pacman repository, so `pacman -Syu` keeps it up to date:
#
#     curl -fsSLO https://evimore.github.io/Carryon/install.sh
#     sudo bash install.sh [--noconfirm]
#
# It adds the repository's signing key to pacman's keyring, after checking its fingerprint, writes
# /etc/pacman.d/carryon.conf and includes it from /etc/pacman.conf, then installs the carryon
# package. Files that an install from the release tarball left behind, which no package owns, are
# replaced by the package's own. It is safe to run again: it only does what isn't done yet.
#
# publish.sh fills in url and fingerprint when it publishes the repository.
set -euo pipefail

url=https://evimore.github.io/Carryon
fingerprint=0000000000000000000000000000000000000000
conf=/etc/pacman.d/carryon.conf

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

confirm=()
for arg in "$@"; do
  case $arg in
    --noconfirm) confirm=(--noconfirm) ;;
    -h | --help) sed -n '2,12s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) die "unknown option $arg. The only option is --noconfirm." ;;
  esac
done

command -v pacman >/dev/null ||
  die "pacman isn't installed. This installer is for Arch Linux and distributions based on it. Elsewhere, install Carryon from the archive on the release page."
(( EUID == 0 )) || die "the installer changes pacman's settings, so run it as root: sudo bash $0"
arch=$(uname -m)
case $arch in
  x86_64 | aarch64) ;;
  *) die "Carryon has no package for $arch. It has them for x86_64 and aarch64." ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fetch() { curl -fsSL --retry 3 -o "$2" "$url/$1" || die "couldn't download $url/$1. Check your connection and try again."; }

# 1. The signing key, trusted by pacman once its fingerprint matches.
gpgdir=$(pacman-conf GPGDir)
trusted() {
  gpg --homedir "$gpgdir" --batch --with-colons --list-keys "$fingerprint" 2>/dev/null |
    awk -F: '$1 == "pub" { print $2; exit }' | grep -q '^[fu]$'
}
if trusted; then
  say "pacman trusts Carryon's signing key already."
else
  say "Adding Carryon's signing key, $fingerprint."
  fetch carryon.asc "$tmp/carryon.asc"
  mkdir -m 700 "$tmp/gnupg"
  keys=$(gpg --homedir "$tmp/gnupg" --batch --with-colons --show-keys "$tmp/carryon.asc" 2>/dev/null |
    awk -F: '$1 == "pub" { pub = 1; next } pub && $1 == "fpr" { print $10; pub = 0 }')
  [[ $keys == "$fingerprint" ]] ||
    die "the key at $url/carryon.asc isn't Carryon's signing key $fingerprint, so nothing was changed."
  pacman-key --add "$tmp/carryon.asc"
  pacman-key --lsign-key "$fingerprint"
fi

# 2. The repository, in a file of its own that pacman.conf includes.
# shellcheck disable=SC2016 # pacman expands $arch itself.
printf '[carryon]\nSigLevel = Required\nServer = %s/$arch\n' "$url" >"$tmp/carryon.conf"
if cmp -s "$tmp/carryon.conf" "$conf"; then
  say "$conf is set up already."
else
  say "Writing $conf."
  install -Dm644 "$tmp/carryon.conf" "$conf"
fi
if grep -qxF "Include = $conf" /etc/pacman.conf; then
  say "/etc/pacman.conf includes it already."
elif pacman-conf --repo-list | grep -qx carryon; then
  say "/etc/pacman.conf has a [carryon] repository of its own, so it is left as it is."
else
  say "Including it from /etc/pacman.conf."
  printf '\nInclude = %s\n' "$conf" >>/etc/pacman.conf
fi

# 3. Files from an install of the release tarball. pacman refuses to overwrite files no package
# owns, so let it replace those the package has, and only those.
overwrite=()
if ! pacman -Q carryon >/dev/null 2>&1; then
  fetch "$arch/carryon.files" "$tmp/carryon.files"
  bsdtar -xOf "$tmp/carryon.files" '*/files' >"$tmp/files" ||
    die "couldn't read the list of the package's files from $url/$arch/carryon.files."
  grep -qx 'usr/bin/carryon' "$tmp/files" ||
    die "the list of the package's files at $url/$arch/carryon.files doesn't have usr/bin/carryon."
  while IFS= read -r path; do
    [[ -n $path && $path != */ && $path != %*% ]] || continue
    [[ -e /$path || -L /$path ]] || continue
    if LC_ALL=C pacman -Qoq "/$path" 2>&1 >/dev/null | grep -q '^error: No package owns'; then
      # --overwrite takes a pattern, so escape its special characters.
      overwrite+=(--overwrite "$(printf '/%s' "$path" | sed 's/[][*?\\]/\\&/g')")
    fi
  done <"$tmp/files"
  if (( ${#overwrite[@]} )); then
    say "Replacing $(( ${#overwrite[@]} / 2 )) files of an earlier install from the release archive."
  fi
fi

# 4. The package. -Syu, since Arch doesn't support installing on top of an outdated system.
say "Installing carryon. pacman brings the rest of the system up to date too."
pacman -Syu --needed "${confirm[@]}" "${overwrite[@]}" carryon

if (( ${#overwrite[@]} )); then
  say "Carryon now comes from its pacman repository, and your settings and backups are as they were. To restart Carryon in your desktop session, run: systemctl --user restart carryon-agent.service"
fi
say "Done. pacman -Syu keeps Carryon up to date from now on."
