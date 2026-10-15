# Carryon

**Back up what makes your Linux machine yours, and get it back on any fresh install.**
New Game+ for your Linux install.

---

Reinstalling Linux or setting up a new computer usually means hours of putting things back:
reinstalling packages, re-enabling services, copying config files and remembering what you changed in
`/etc`. Backup tools save your files, but not the knowledge of what your system *is*.

**Carryon records your system and backs it up**, then rebuilds a fresh install from that backup:

- **Everything that makes it yours:** the packages you chose (including AUR), extra repositories and
  their keys, the system files you changed or added, your services, kernel parameters, users,
  groups and home folders.
- **Pick and choose on restore:** your setup is grouped into bundles, such as an app with its
  settings, service and autostart entry. Hardware-specific ones start unticked on a machine without
  that hardware. You see the full plan before anything changes.
- **Runs by itself:** a daily backup, another one after you install or remove packages, a weekly
  check that the backup can really be restored, and a notification when something's wrong.
- **Your storage, encrypted:** backups go to a [restic](https://restic.net) repository on a USB disk,
  a NAS, SFTP, S3, B2 or anything rclone can reach. Your data stays readable with plain restic.

> **Status: early development.** Carryon is being rewritten in Go from a working prototype. Nothing is
> usable yet. See the [roadmap](docs/roadmap.md) for what's done and what's next.

## What you'll need

- An Arch-based distribution: Arch Linux, CachyOS, EndeavourOS, Manjaro... Others are planned.
- A place for backups: a USB disk, a NAS, an SFTP server or a cloud storage account.

## Documentation

- [Design](docs/design.md): how Carryon works and why
- [Roadmap](docs/roadmap.md): milestones and the task list
- [The prototype](legacy/README.md): the Python tool Carryon grows out of

## For developers

```sh
go build ./cmd/carryon
go test ./...
```

Carryon is written in Go and builds to a single static binary. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Carryon is licensed under the [GNU General Public License v3.0 or later](LICENSE).
