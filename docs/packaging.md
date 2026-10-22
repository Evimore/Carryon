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
`sha256sums`, [attests](#build-provenance) the tarballs and attaches them to the release, in a
couple of minutes. The notes come from `.github/release-notes.sh`. A stable release also goes into
the [pacman repository](#the-pacman-repository). On pull requests the workflow only builds the
tarballs, as a dry run.

Before a release, run the backup and the deep check through the real systemd services on a real
machine ([design.md, "Testing"](design.md#testing)).

### Build provenance

Before it attaches the tarballs, the "Publish" job attests their build provenance with
`actions/attest-build-provenance`: a record, signed for that workflow run and kept by GitHub, of the
workflow, commit and run that built each tarball. Anyone can check a download with the
[GitHub CLI](https://cli.github.com), as [SECURITY.md](../SECURITY.md#checking-a-download) explains:

```sh
gh attestation verify carryon-0.3.0-linux-x86_64.tar.gz --repo Evimore/Carryon
```

- Only the "Publish" job gets `attestations: write`, along with `id-token: write` to sign. It never
  runs in the dry run, so pull requests attest nothing.
- If attesting fails, the job stops before it attaches anything, so no tarball is ever attached
  without an attestation. Re-run the failed jobs to try again.
- Attaching a release's files again ("Run workflow" with the version of a release that exists)
  attests them again. GitHub records the commit the workflow ran from, so that attestation names
  the commit `main` was at then, not the tag's; the run it names shows which tag it built.
- `carryon-bin` doesn't use attestations: it keeps checking the tarballs against the sha256 sums in
  its `PKGBUILD`, which match the release's `sha256sums`.

## The pacman repository

Carryon has its own signed pacman repository on GitHub Pages, at `https://evimore.github.io/Carryon`,
so Arch users install it with `pacman` and upgrade it with the rest of the system. The site holds:

| Path | What |
|---|---|
| `x86_64/`, `aarch64/` | the `carryon` package and the repository database, each file with its signature |
| `carryon.asc`, `fingerprint.txt` | the public signing key and its fingerprint |
| `install.sh` | the installer, from `packaging/repo/install.sh` |

The installer adds the key to pacman's keyring once its fingerprint matches the one written into
the installer, writes `/etc/pacman.d/carryon.conf` and includes it from `/etc/pacman.conf`, then
runs `pacman -Syu carryon`. On a machine where Carryon was installed from the release tarball, it
lets pacman replace the files the package contains, and only those that no package owns. Running it
again changes nothing.

### How it's published

The repository holds the latest stable release only. Pre-releases never reach it. For each stable
release, the "Release" workflow:

1. packages the release's tarballs as `carryon` for x86_64 and aarch64 with
   `packaging/repo/build.sh`, in an Arch container, and checks them with namcap. Nothing is
   compiled, so the aarch64 package is made on x86_64 by setting `CARCH` in a makepkg config;
2. signs the packages and a fresh database with `packaging/repo/publish.sh`, in a job that runs in
   the `pacman-repo` environment, the only one that sees the key. It first checks that the key is
   the one the repository variable `PACMAN_REPO_FINGERPRINT` names;
3. publishes the release, whose notes then open with the repository;
4. deploys the site to GitHub Pages, in the `github-pages` environment.

The site is rebuilt from scratch each time, so nothing from an earlier release lingers. Until the
key is set up, step 2 is skipped with a notice, the release is made as usual and its notes leave the
repository out. A problem with the key doesn't stop the release either: the run shows the error and
the repository stays as it was.

The "Package" workflow tries the whole thing on each pull request: it builds both packages from
tarballs of the checkout, publishes a repository signed with a throwaway key to a folder, installs
from it with the installer, runs the installer again to check it changes nothing, and moves a
tarball install onto the package.

### Setting up the pacman repository

Do this once, on any Linux machine with `gpg`, and on GitHub's website.

1. **Make the signing key.** It has no passphrase, since the workflow signs without anyone there:

   ```sh
   gpg --batch --passphrase '' --quick-gen-key \
     "Carryon packages <100224882+Evimore@users.noreply.github.com>" ed25519 sign never
   gpg --list-secret-keys "Carryon packages"       # the 40-character line is the fingerprint
   gpg --armor --export-secret-keys FINGERPRINT > carryon-packages.asc
   ```

   Keep a copy of `carryon-packages.asc` somewhere safe and offline, such as a password manager.
   Anyone who has it can sign packages that every Carryon user's pacman accepts.
2. **Create the `pacman-repo` environment with the key.** On GitHub, open **Settings →
   Environments → New environment** and name it `pacman-repo`. Leave its deployment branches and
   tags unrestricted. Under **Environment secrets**, add `PACMAN_REPO_KEY` and paste the whole of
   `carryon-packages.asc`, from `-----BEGIN PGP PRIVATE KEY BLOCK-----` to the `END` line. Then
   delete `carryon-packages.asc` from the machine (`shred -u carryon-packages.asc`), keeping only
   the safe copy.
3. **Add the fingerprint.** Open **Settings → Secrets and variables → Actions → Variables → New
   repository variable**, name it `PACMAN_REPO_FINGERPRINT` and paste the fingerprint. It is a
   repository variable, not one of the environment's, and it isn't secret: users check the key
   against it.
4. **Let GitHub Actions publish the site.** Open **Settings → Pages** and set **Source** to
   **GitHub Actions**. That creates the `github-pages` environment, which lets only `main` deploy.
   A release published by hand runs on its tag, so to have those deploy too, open **Settings →
   Environments → github-pages** and add the tag `v*` under **Deployment branches and tags**.

The next stable release publishes the repository. To publish it right away, republish the latest
release as below.

### Republishing

On the **Actions** tab, open **Release**, then **Run workflow**, and type the latest stable version,
like `0.3.0`. That attaches the release's files again and rebuilds the repository from them. Typing
an older version rebuilds nothing, since the repository only holds the latest release.

### Replacing the key

If the key is lost or someone else may have it:

1. Make a new key as in step 1 above.
2. Replace the secret `PACMAN_REPO_KEY` in the `pacman-repo` environment, and the repository
   variable `PACMAN_REPO_FINGERPRINT`.
3. Republish, as above.

pacman refuses the repository until users trust the new key, so tell them in the next release's
notes to run the installer again. It adds the new key. To also drop the old one:
`sudo pacman-key --delete OLD-FINGERPRINT`.

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
