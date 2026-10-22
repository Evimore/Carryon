# Security

## Reporting a problem

Please report security problems privately, through GitHub's **Report a vulnerability** button on the
repository's Security tab, not in a public issue. Say what you found, how to reproduce it and what
it affects.

## How Carryon protects your data

- **Backups are encrypted** by restic before they leave the machine. Without the repository password
  nobody can read them, including you, so keep the password somewhere safe outside the machine.
- **The password stays root-only** on the machine: in `/etc/carryon/repository-password` with mode
  `0600`, or encrypted with `systemd-creds`.
- **The system record holds no passwords.** Secrets such as Wi-Fi passwords and SSH host keys only
  live in the encrypted repository. Account passwords are never stored.
- **Carryon runs as root** for backups and restores, because it must read every system file and home
  folder. The per-user notification agent never runs as root, and a user's own settings can only
  change what's backed up inside their own home folder.

## Checking a download

Each release tarball comes with a build provenance attestation: a signed record, made by the
"Release" workflow as it publishes the tarball, of where the file came from. To check a tarball you
downloaded, use the [GitHub CLI](https://cli.github.com) with the file's own name:

```sh
gh attestation verify carryon-0.3.0-linux-x86_64.tar.gz --repo Evimore/Carryon
```

The tarballs are named `carryon-VERSION-linux-x86_64.tar.gz` and
`carryon-VERSION-linux-aarch64.tar.gz`. Releases up to 0.2.1 came before attestations and may not
have one.

If the check succeeds, the file was built by a GitHub Actions workflow of this repository and hasn't
changed since. The command shows the workflow, which should be `.github/workflows/release.yml` on
`main` or on the release's tag. The attestation also records the commit and the workflow run that
built the file: add `--format json` to see them.

It doesn't tell you the code is free of bugs or safe to run, only where the file came from. You
still trust GitHub, and the people who can change this repository. The `sha256sums` file next to
the tarballs only shows that a download isn't damaged, not where it came from.
