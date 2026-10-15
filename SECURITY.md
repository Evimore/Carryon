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
