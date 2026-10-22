#!/usr/bin/env bash
# Lays out Carryon's pacman repository in SITE, signed, ready to be served at URL:
#
#     packaging/repo/publish.sh PACKAGES SITE URL FINGERPRINT
#
# PACKAGES holds the packages build.sh made. FINGERPRINT names the signing key, which must be in
# gpg's keyring (GNUPGHOME), without a passphrase. SITE must not exist: the repository is made
# fresh each time, so a package that was removed can't linger in its database. It gets:
#
#     x86_64/, aarch64/   the packages and the database, each file with its signature
#     carryon.asc         the public key
#     fingerprint.txt     its fingerprint
#     install.sh          the installer, with URL and FINGERPRINT filled in
#     index.html          a page that says how to install
#
# Needs gpg and repo-add.
set -euo pipefail

die() { echo "publish.sh: $*" >&2; exit 1; }

(( $# == 4 )) || die "usage: publish.sh PACKAGES SITE URL FINGERPRINT"
packages=$1 site=$2 url=${3%/} fingerprint=${4//[[:space:]]/}
fingerprint=${fingerprint^^}
here=$(cd "$(dirname "$0")" && pwd)
[[ $fingerprint =~ ^[0-9A-F]{40}$ ]] || die "\"$4\" isn't a key fingerprint: 40 hexadecimal digits."
[[ ! -e $site ]] || die "$site exists already. The repository is made fresh each time."

sign() { gpg --batch --yes --local-user "$fingerprint" --detach-sign --no-armor "$1"; }

for arch in x86_64 aarch64; do
  pkgs=()
  for pkg in "$packages"/carryon-[0-9]*-"$arch".pkg.tar.*; do
    if [[ -f $pkg && $pkg != *.sig ]]; then pkgs+=("$pkg"); fi
  done
  (( ${#pkgs[@]} == 1 )) || die "expected one $arch package in $packages, found ${#pkgs[@]}."
  mkdir -p "$site/$arch"
  cp "${pkgs[0]}" "$site/$arch/"
  pkg=$site/$arch/${pkgs[0]##*/}
  sign "$pkg"
  repo-add --sign --key "$fingerprint" "$site/$arch/carryon.db.tar.gz" "$pkg"
  # repo-add links carryon.db to carryon.db.tar.gz. Pages serves files, so copy them instead.
  for link in "$site/$arch"/*; do
    if [[ -L $link ]]; then cp --remove-destination "$(realpath "$link")" "$link"; fi
  done
  for file in "$site/$arch"/*.sig; do
    gpg --batch --verify "$file" "${file%.sig}" 2>/dev/null || die "the signature $file doesn't verify."
  done
done

gpg --batch --armor --export "$fingerprint" >"$site/carryon.asc"
printf '%s\n' "$fingerprint" >"$site/fingerprint.txt"
sed -e "s|^url=.*|url=$url|" -e "s|^fingerprint=.*|fingerprint=$fingerprint|" \
  "$here/install.sh" >"$site/install.sh"
cat >"$site/index.html" <<HTML
<!doctype html>
<meta charset="utf-8">
<title>Carryon packages</title>
<h1>Carryon packages</h1>
<p>The pacman repository of <a href="https://github.com/Evimore/Carryon">Carryon</a>, for Arch Linux and distributions based on it. To install Carryon and keep it up to date with the rest of the system:</p>
<pre>curl -fsSLO $url/install.sh
sudo bash install.sh</pre>
<p>The packages are signed with the key <a href="carryon.asc">$fingerprint</a>.</p>
HTML
find "$site" -type f | sort
