# Packaging and releases

How Carryon is built, installed and published. For maintainers; users only need the
[README](../README.md#install).

## One install step

Every package uses the `Makefile`, so the install layout lives in one place
([design.md, "Install layout"](design.md#install-layout)):

```sh
make                                  # build/carryon, build/man/carryon.1, build/completions/*
sudo make install                     # PREFIX=/usr; packages add DESTDIR="$pkgdir"
make check-install                    # install into a temporary folder and check the layout
make dist GOARCH=arm64                # a release tarball for another architecture
```

- `make` builds one static binary (`CGO_ENABLED=0`, `-trimpath`) stamped with the version, commit
  and date. `VERSION`, `COMMIT` and `GO_LDFLAGS` (extra linker flags, "-s -w" for releases) can be
  set on the command line. The date comes from `SOURCE_DATE_EPOCH`, or the commit's time.
- The man page and the bash, zsh and fish completions are generated from the commands by
  `internal/tools/gendocs`, so they always match `--help`.
- `make install` never compiles: run `make` first, as yourself.
- The units, the pacman hook and the binary expect `PREFIX=/usr`. Nothing under `/etc` or `/var` is
  installed: `carryon init` creates those, so packages keep `backup=()` empty.
- `packaging/layout.txt` lists every file a package installs. `packaging/check-layout.sh` compares an
  installed file list with it; CI runs it on `make install`, the release tarballs and the installed
  pacman package. A new file to install goes into the `Makefile` and `layout.txt` together.

### Why not -buildmode=pie

The Arch Go packaging guidelines ask for `-buildmode=pie`. Without cgo, Go's linker can only make a
PIE that is dynamically linked against the system's loader, and Carryon is one static binary on
purpose: the same file runs from the recovery kit on any distribution, including a live USB. So the
packages build without PIE. namcap's warnings about it are expected: the binary "lacks PIE" and
"FULL RELRO". So is "Dependency included, but may not be needed ('restic')", since Carryon runs
restic instead of linking to it, and "Reference to x86_64" in `carryon-bin`, whose sources are per
architecture. CI fails on namcap errors.

## AUR packages

`packaging/aur/` has one folder per AUR package. Each folder is what goes into that package's AUR
git repository: `PKGBUILD`, `.SRCINFO` and `carryon.install` (the same in all three).

| Package | Builds |
|---|---|
| `carryon` | a tagged release, from the source tarball GitHub makes for the tag |
| `carryon-bin` | nothing: installs the release tarball from the GitHub release |
| `carryon-git` | the latest commit on `main` |

The "Package" workflow builds `carryon-git` from each pull request's checkout with `makepkg` in an
Arch container, runs namcap, installs it with `pacman -U`, and checks `carryon version` and the file
layout. It also checks that every `.SRCINFO` matches its `PKGBUILD`.

After changing a `PKGBUILD`, regenerate its `.SRCINFO` on an Arch machine:

```sh
cd packaging/aur/carryon
makepkg --printsrcinfo > .SRCINFO
```

## Making a release

Releases are made on GitHub's website: no git, no tag to type and no notes to write.

- **A new version:** when every issue in a version's milestone is closed, close the milestone
  (**Issues → Milestones → Close**). Closing `v0.3 Comfort` releases 0.3.0 from `main`. The notes
  open with the milestone's description, list its tasks under "Features" and its bugs under "Bug
  fixes", then give the requirements, how to install on each architecture and a link to all
  changes. Issues labeled `internal` stay out of the notes. A milestone that still has open issues, or isn't named after a
  version, releases nothing, and the workflow run says why.
- **Fixes between versions:** on the **Actions** tab, open **Release**, then **Run workflow**. Left
  empty, it releases the next fix version from `main` (0.2.1 after 0.2.0), with notes listing the
  issues closed since the last release. Or type the version yourself: one with a `-`, like
  `0.3.0-rc1`, makes a pre-release, to try a release first. The version of a release that exists
  builds and attaches its files again.
- **By hand:** a release published on the **Releases** page, with a tag named like `v0.2.0`, gets
  its files attached. You write its notes.

Each way, the "Release" workflow builds static binaries for x86_64 and aarch64, packs a tarball of
each (`carryon-0.3.0-linux-x86_64.tar.gz`: the `usr/` tree `make install` lays out), writes
`sha256sums` and attaches them to the release, in a couple of minutes. The notes come from
`.github/release-notes.sh`. On pull requests the workflow only builds the tarballs, as a dry run.

Before a release, run the backup and the deep check through the real systemd services on a real
machine ([design.md, "Testing"](design.md#testing)).

## Publishing to the AUR

You need an [AUR account](https://aur.archlinux.org/register) with your SSH public key added under
"My Account". These steps run on an Arch machine with `base-devel`, `git` and `namcap`.

### The first upload

Do this once per package, after the first release exists on GitHub:

```sh
# 1. Update the release packages' version and checksums in this repository.
cd packaging/aur/carryon
sed -i 's/^pkgver=.*/pkgver=0.1.0/; s/^pkgrel=.*/pkgrel=1/' PKGBUILD
updpkgsums                              # downloads the tag's source tarball, fills in sha256sums
makepkg --printsrcinfo > .SRCINFO
cd ../carryon-bin
sed -i 's/^pkgver=.*/pkgver=0.1.0/; s/^pkgrel=.*/pkgrel=1/' PKGBUILD
updpkgsums                              # must match the release's sha256sums file
makepkg --printsrcinfo > .SRCINFO
cd ../carryon-git
makepkg --nobuild                       # runs pkgver() and writes the new pkgver into PKGBUILD
makepkg --printsrcinfo > .SRCINFO

# 2. Build and check each package in a clean chroot (devtools), then try it.
cd ../carryon
extra-x86_64-build                      # or: makepkg -sf
namcap PKGBUILD *.pkg.tar.zst
sudo pacman -U carryon-0.1.0-1-x86_64.pkg.tar.zst && carryon version

# 3. Commit the updated PKGBUILDs and .SRCINFO files here, in a pull request.

# 4. Create each AUR repository by cloning its (empty) address and pushing the files.
cd "$(git rev-parse --show-toplevel)"
for pkg in carryon carryon-bin carryon-git; do
  git clone "ssh://aur@aur.archlinux.org/$pkg.git" "/tmp/aur-$pkg"
  cp "packaging/aur/$pkg"/{PKGBUILD,.SRCINFO,carryon.install} "/tmp/aur-$pkg/"
  git -C "/tmp/aur-$pkg" add PKGBUILD .SRCINFO carryon.install
  git -C "/tmp/aur-$pkg" commit -m "Initial upload: $pkg"
  git -C "/tmp/aur-$pkg" push origin master
done
```

The AUR only accepts the `master` branch. Its git server refuses a push whose `.SRCINFO` doesn't
match the `PKGBUILD`, so always regenerate it.

### Each release after that

1. Make the release as above, and wait for the "Release" workflow to finish.
2. For `carryon` and `carryon-bin`: set `pkgver`, reset `pkgrel=1`, run `updpkgsums` and
   `makepkg --printsrcinfo > .SRCINFO`, build and test as above.
3. `carryon-git` needs no update for a release: AUR helpers rebuild it from `main`. Push it only
   when its `PKGBUILD` changes (bump `pkgrel` for a packaging-only change).
4. Copy the files into each AUR clone, commit ("Update to 0.2.0") and push. Commit the same changes
   here.
