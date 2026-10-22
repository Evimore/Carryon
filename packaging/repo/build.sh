#!/usr/bin/env bash
# Packages a release's tarballs as `carryon` for Carryon's pacman repository, one package for each
# architecture. Nothing is compiled: each package holds the files of its tarball, as released.
#
#     packaging/repo/build.sh VERSION DIST OUT
#
# DIST holds carryon-VERSION-linux-x86_64.tar.gz and carryon-VERSION-linux-aarch64.tar.gz, and the
# packages go into OUT. Needs makepkg, and a user other than root, since makepkg refuses root.
set -euo pipefail

die() { echo "build.sh: $*" >&2; exit 1; }

(( $# == 3 )) || die "usage: build.sh VERSION DIST OUT"
version=$1 dist=$2 out=$3
# Pre-releases never reach the repository. pacman versions can't hold a "-" anyway.
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "$version isn't a stable version like 0.3.0, and only stable releases are packaged."

here=$(cd "$(dirname "$0")" && pwd)
dist=$(realpath "$dist")
mkdir -p "$out"
out=$(realpath "$out")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arch in x86_64 aarch64; do
  tarball=carryon-$version-linux-$arch.tar.gz
  [[ -f $dist/$tarball ]] || die "$dist/$tarball is missing."
  dir=$work/$arch
  mkdir "$dir"
  cp "$here/PKGBUILD" "$here/../aur/carryon/carryon.install" "$dist/$tarball" "$dir/"
  sum=$(sha256sum "$dist/$tarball" | cut -d' ' -f1)
  sed -i -e "s/^pkgver=.*/pkgver=$version/" -e "s/^sha256sums_$arch=.*/sha256sums_$arch=('$sum')/" \
    "$dir/PKGBUILD"
  # makepkg packages for the architecture its config names, and nothing is compiled, so CARCH is
  # all another architecture needs.
  { cat /etc/makepkg.conf; printf '\nCARCH=%s\n' "$arch"; } >"$dir/makepkg.conf"
  (cd "$dir" && PKGDEST=$out makepkg --config "$dir/makepkg.conf" --nodeps --noconfirm --clean)
done
ls -l "$out"
