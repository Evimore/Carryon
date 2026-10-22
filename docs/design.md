# Carryon design

Carryon backs up what makes a Linux machine yours: the packages you chose, the system settings you
changed, your services and your home folder. From that backup it rebuilds a fresh install, all at once
or one app at a time, and it notices new things to back up by itself.

This document is the plan for the Go rewrite of the [prototype](../legacy/README.md). Where the
prototype already made a decision that worked, Carryon keeps it; those parts say so. Sections marked
**New** didn't exist in the prototype. The versions are in [roadmap.md](roadmap.md), and the tasks
are GitHub issues.

**Contents:** [Goals](#goals) · [Words used here](#words-used-here) · [Overview](#overview) ·
[The system record](#the-system-record) · [What gets captured](#what-gets-captured) ·
[Bundles and when-rules](#bundles-and-when-rules) · [Storage](#storage) ·
[Destinations and mount guards](#destinations-and-mount-guards) · [Running by itself](#running-by-itself) ·
[Noticing new things](#noticing-new-things) · [Restoring](#restoring) · [Configuration](#configuration) ·
[Interfaces](#interfaces) · [Who runs what](#who-runs-what) · [Getting back in](#getting-back-in) ·
[Code layout and libraries](#code-layout-and-libraries) · [Install layout](#install-layout) ·
[Testing](#testing) · [Moving over from the prototype](#moving-over-from-the-prototype) ·
[Decisions](#decisions) · [Open questions](#open-questions)

## Goals

- **Back up broadly, understand deeply.** Capture everything personal by default, so nothing is lost
  because nobody described an app. Use what Carryon knows about packages and apps to group the backup,
  restore it selectively and skip what can be downloaded again.
- **Runs by itself.** After `carryon init`, backups, checks and retention are automatic. Problems
  always reach the user; nothing fails silently.
- **Restore is pick and choose.** Machines differ. Hardware-specific parts start unticked where the
  hardware is missing, and nothing is applied before the user has seen the plan.
- **No lock-in.** The data is an ordinary restic repository and the system record is JSON. Both stay
  readable without Carryon.
- **Arch-based distributions first.** Package-manager details sit behind one interface so other
  distributions can follow.

**Not goals:** disk images, local rollback (snapper and Timeshift do that well), live sync between
machines, Windows or macOS.

## Words used here

| Word | Meaning |
|---|---|
| **Record** | Carryon's description of a machine: its items, bundles and facts. Stored with every snapshot. |
| **Item** | One thing in the record: a package, an edited file, an enabled service, a backed-up folder... |
| **Bundle** | Items that belong together and are restored together, such as an app with its settings and service. |
| **When-rule** | Decides whether a bundle starts ticked on the machine being restored, such as `samehost` or `hw:usb:1234:5678`. |
| **Repository** | The restic repository the backups go to. One repository can hold several machines. |
| **Snapshot** | One backup run in the repository. |
| **Machine name** | The name a machine's snapshots are stored under. Unique per repository. |
| **Inbox** | Suggestions waiting for the user, such as "back up `/var/lib/jellyfin`?" |

## Overview

```
 This machine                                                   Repository (restic)
┌──────────────────────────────────────────┐                  ┌──────────────────────────┐
│ capture ──► record (items, bundles, facts)│                  │ snapshot per run:        │
│   packages · repos · edited/added files   │   carryon backup │   record                 │
│   services · kernel params · users        │ ───────────────► │   /etc, /root, extra     │
│   home folders · extra system paths       │                  │   paths, home folders    │
│                                           │                  │   (minus re-downloadable)│
│ pacman hook · timers · scans ──► inbox    │                  └────────────┬─────────────┘
└──────────────────────────────────────────┘                               │ carryon restore
                                                                            ▼
                                                 Fresh install: pick bundles ─► plan ─► apply
```

## The system record

The record lives in `/var/lib/carryon/record/` (root only, `0700`) and inside every snapshot:

| File | Contents |
|---|---|
| `meta.json` | Machine facts at capture time: machine name, hostname, machine fingerprint, users, kernel, chassis, distribution, Carryon version, record format, complete or not. |
| `items.jsonl` | One item per line, keys sorted. |
| `bundles.json` | Bundle id → title and when-rule, in restore order. |
| `files/` | Copies of edited and added system files (mode, owner and checksum are in the item). |
| `user-units/<user>/` | Each user's systemd unit files and drop-ins. |
| `repo-keys.asc` | Signing keys of extra package repositories. |
| `versions.txt` | Every installed package with its version, for reference. |
| `not-backed-up.md` | What was deliberately left out and how it comes back (Steam games, toolchains...). |
| `SUMMARY.md` | A readable summary of all bundles. |

A local git history of the record answers "what did I change last month?"
(`carryon history`). **Fix:** a commit is made only when an item actually changed, so there are no
empty "0 change(s)" commits.

Capture writes the new record into a staging folder next to the record; publishing adds
`SUMMARY.md`, `.gitignore` and `last-capture`, keeps the git history, and keeps the last
`not-backed-up.md` when a quick capture (the pacman hook) made none. **Changes from the prototype:**

- **Atomic publish.** The prototype emptied the record folder and moved the new files in, so a crash
  could leave half a record. Carryon swaps whole folders, and the next publish finishes or undoes
  an interrupted one.
- **git runs on its own settings.** Commits use the identity `carryon <carryon@localhost>` (the
  prototype put the hostname in it), and root's own git settings (signing, hooks, pager) are
  ignored. Without git there's no history, and nothing fails.
- The record folder is always `0700`, not only when captured as root.

### Item format

Items are JSON objects with a `type`, an `id`, a `bundle` and type-specific fields. **Carryon keeps the
prototype's field names**, so it can read records made by the prototype (format 1) and restore from
the snapshots it made. Carryon writes format 2, which adds the `user` field for multi-user machines.

| Type | id | Notable fields |
|---|---|---|
| `pkg` | package name | `repo`, `asdeps` |
| `aur` | package name | `asdeps` |
| `repo` | repository name | `conf`, `section`, `keys`, `key_names` |
| `file` | absolute path | `kind` (`modified`, `edited`, `unowned`), `mode`, `owner`, `pkg`, `sha`, `link`, `too_big` |
| `svc`, `mask` | unit name | `unit_file`, `pkg` |
| `usvc`, `uunit`, `umask` | unit name or relative path | `user`, `sha` |
| `linger` | user name | |
| `mkdir` | absolute path | `rel`, `for_unit`, `user` |
| `kparam` | kernel parameter | `source` |
| `group`, `shell` | group name or shell path | `user` |
| `cargo`, `pipx`, `npm`, `flatpak` | package or app id | `origin`, `scope`, `user` |
| `path` | absolute path | `home` (path relative to the user's home), `extra`, `owner`, `uid`, `gid`, `user` |

`kind: edited` is **new**: a package-owned file that differs from the package but isn't in its
`backup=` list (see [files](#what-gets-captured)).

## What gets captured

Kept from the prototype, with generalizations marked.

- **Packages.** Explicitly installed packages from the repositories (`pkg`, with the repository they
  come from) and foreign ones (`aur`). Packages installed as dependencies but only optionally needed
  are kept with `asdeps`; true orphans aren't.
- **Extra repositories** that aren't part of the distribution, with their `pacman.conf` section and any
  signing keys added by hand (the pacman keyring minus the distribution keyrings).
  **Generalized:** which repositories count as the distribution's is a per-distribution list
  (Arch, CachyOS, EndeavourOS, Manjaro, Garuda...) that can be extended in the config.
- **System files.** In `/etc`, `/usr` and `/opt`:
  - `modified`: config files in a package's `backup=` list that differ from the package
    (`pacman -Qii`);
  - **New:** `edited`: other package files that differ (`pacman -Qkk`). This check reads every
    package file, so it runs in the nightly capture only, not in the pacman hook;
  - `unowned`: files no package owns.

  Machine-specific and generated files are never captured: machine-id, passwd/shadow, CA bundles,
  the pacman keyring, `*.pacnew`, caches and Carryon's own files. Secrets in `/etc` (SSH host keys,
  NetworkManager connections) are captured as backed-up paths instead, so they only live in the
  encrypted repository. Files over 100 MiB are flagged instead of copied.
- **Services.** Enabled and masked system units, with the package that owns each. Each user's enabled
  and masked user units, plus their unit files and drop-ins, and lingering. For units nobody's package
  provides, the packages their `Exec` lines need and the folders they use (mount points, log folders)
  are added too.
- **Kernel parameters** from the bootloader config (Limine, systemd-boot, GRUB or
  `/etc/kernel/cmdline`), minus root and boot parameters that differ per install.
- **Users.** **Generalized:** every human user (UID 1000 and up with a login shell) unless the config
  limits it. For each, their groups and login shell. Passwords are never stored.
- **Language tools:** cargo, pipx, global npm packages and Flatpak apps (system-wide and per user).
- **Backed-up paths** (restored from the repository rather than the record): NetworkManager
  connections, Bluetooth pairings, Tailscale state, enrolled fingerprints, SSH host keys, `/root`,
  `/srv` and the configured extra paths. For each user, every top-level entry of the home folder and
  every entry of `~/.config`, `~/.local` and `~/.local/share`, so apps can be restored one by one.
- **Hardware facts** for when-rules: a machine fingerprint (hash of the DMI product UUID, board serial
  and board name), chassis, PCI and USB device IDs, Bluetooth adapters, kernel modules, root
  filesystem and bootloader.
- **The not-backed-up list:** installed Steam games by name and size, toolchains, and app files that
  download themselves again, each with how it comes back. **Generalized:** these hints come from the
  [catalog](#configuration), not from code.

Where capture differs from the prototype on purpose:

- **Several users:** groups, shells, user units, language tools and home entries are captured for
  each user and carry `user`; user units go to `user-units/<user>/`. System-wide Flatpak apps are
  recorded once (`scope: system`, no user), and each user's own apps with `scope: user`. The
  not-backed-up list has one part per user.
- **A user's folders don't widen what root reads:** a user's unit folder that leads out of their
  home through a symlink is skipped, and so are unit files someone else owns.
- `%h` in a system unit's `Exec` line means `/root`, as systemd reads it; the prototype used the
  user's home.
- Kernel parameters read from the running kernel (no bootloader config found) have
  `source: /proc/cmdline`; the prototype wrote `None`.
- `edited` files are only taken from `/etc`, `/usr` and `/opt`, like `unowned` ones.
- Folders the capture couldn't read (without root) are listed in `skipped.txt` too.

## Bundles and when-rules

Kept from the prototype. Items are grouped into bundles in this order of precedence:

1. The user's own rules (`put` in `bundles.conf`).
2. App bundles named after units the user made, extra repositories and AUR packages: everything
   sharing their name (binaries, configs, udev rules, settings folders) travels together.
3. Well-known places: locale, identity, login screen, fingerprint login, bootloader tools, snapper,
   initramfs, keyboard tweaks, SSH and GPG keys, game saves, personal files, monitor layout, disk
   layout.
4. A package's edited configs together with its service.
5. Settings folders named like an installed package (`~/.config/OpenRGB` ↔ `openrgb`), autostart
   entries matched by the package owning their program, and extra system folders named like a package
   (`/var/lib/foo` ↔ `foo`).
6. Hardware: kernel parameters for a driver or device, firmware files, apps whose udev rules name only
   a few device makers.
7. Everything else into the built-in catch-all bundles.

A unit's needs (the packages its `Exec` lines run, the folders it uses) go wherever the unit went.
Capture groups the items itself before writing the record, so every capture (the nightly one, the
backup run, the pacman hook) is grouped the same way, with the user's `bundles.conf` applied.

**Several users (generalized).** Bundles are about things, not people: one bundle holds every user's
items for it, and each item keeps its `user`. Alice's and Bob's `~/.config/htop` both go into
`app-htop`, both users' SSH keys into `home-keys`, both users' groups into `account`. Rules about
home folders (well-known places, settings folders, autostart entries, `~/...` globs in
`bundles.conf`) match the path inside each user's home. A unit's needs follow that same user's
unit. So a restore ticks bundles as before; which user receives a user's items is decided per item
(see [Several users](#several-users) under restoring).

Where grouping differs from the prototype on purpose:

- **Files with disk UUIDs** (fstab, crypttab, bootloader configs) always go to `disk-layout`, the
  bundle whose title says they're shown as a diff and never copied. In the prototype an app bundle
  or a `put` rule could take them first (`/etc/default/limine` joined a `limine` app bundle).
  Restore still refuses to copy them wherever they are.
- `edited` package files (new, see [files](#what-gets-captured)) fall back to `configs` with the
  other package tweaks.
- A firmware file right in `/usr/lib/firmware` goes to `firmware-files`; the prototype made a
  bundle called `firmware--usr-lib-firmware`.
- The `packages` and `aur` titles no longer name one distribution or one AUR helper.
- When two app names fit an item equally well, the alphabetically first wins; the prototype's choice
  depended on Python's set order.

When-rules: `always`, `never`, `samehost` (same machine fingerprint, not same hostname),
`chassis:laptop|desktop|...`, `hw:pci:VVVV:DDDD,usb:VVVV:PPPP,module:NAME,bt:MAC` (any present),
`rootfs:btrfs`, `bootloader:limine|grub|systemd-boot|refind`, joined with `+` when all must hold.

`bundles.conf` keeps the prototype's format:

```
title <bundle> <text>          name shown in the restore checklist
when  <bundle> <rule>          when the bundle starts ticked
put   <bundle> <type> <glob>   move matching items into a bundle
drop  <type> <glob>            never record these
```

## Storage

- **restic** stores everything. Carryon requires restic 0.17 or newer, runs it with
  `--json` where it parses output, and always sets `HOME` and `RESTIC_CACHE_DIR`
  (`/var/cache/carryon/restic`), because systemd services start without `HOME`.
- **Any restic repository URL works:** a local path, `sftp:`, `rest:`, `s3:`, `b2:`, `azure:`, `gs:`,
  `swift:` or `rclone:`. For `rclone:` repositories, rclone runs as the user whose rclone config
  holds the login (`rclone_user`), through Carryon's own shim (`carryon rclone-shim`, passed to restic
  as `-o rclone.program=...`).
- **One repository, many machines.** Each machine stores snapshots under its **machine name**.
  **Fix:** the prototype used the hostname, so two machines with the same hostname would share a
  retention group. Carryon stores a machine name in its config at `init`, and `init` refuses a name
  that another machine (different fingerprint) already uses in the repository.
- **Snapshot layout.** Every backup is one snapshot, made with `restic backup --host <machine name>
  --tag carryon --one-file-system --exclude-caches` and the effective exclude file. It holds, at
  their places on the machine: `/etc` (so `/etc/carryon` travels with it), `/root`, `/srv`, the
  record's backed-up `path` items, `backup.extra_paths`, the included users' homes, nested btrfs
  subvolumes inside any of these, and the record itself at `/var/lib/carryon/record`. Restore finds
  the record there. **Changes from the prototype:** the tag is `carryon` (the prototype's was
  `auto`), and only the record is taken from `/var/lib/carryon`, not the whole state folder.
- **Retention** per machine, by default everything from the last 2 days, then 7 daily, 4 weekly and 6
  monthly (`restic forget --host <machine name> --group-by host`), plus the snapshot restoring as you
  go works from (`--keep-tag carryon-source`, see [as you go](#as-you-go)). Pruning runs every
  `prune_every_days` (weekly), counted from the first backup. A failed `forget` or `prune` is a
  warning and is tried again next time; the snapshot is already saved.
- **Password:** generated at `init` (40 random characters) or entered when connecting to an existing
  repository. Stored root-only in `/etc/carryon/repository-password`, or encrypted with
  `systemd-creds` (bound to the TPM when there is one), or fetched with a command
  (`RESTIC_PASSWORD_COMMAND`).
- **Errors** are classified into a short cause plus what to do: wrong password, repository locked,
  login expired (with the exact `rclone reconnect <remote>:` for rclone), destination missing, disk
  full. **Generalized:** the prototype assumed iCloud in every message.

## Destinations and mount guards

**New.** The user chooses the destination at `init`; Carryon guards against the ways a destination
silently goes wrong:

| Situation | What Carryon does |
|---|---|
| A local destination's drive isn't mounted | At `init` it records the filesystem UUID under the path and the repository ID. Every run checks both first. If either is missing or different it writes nothing, because writing into an empty mount point fills the system disk. It records the run as "waiting for <drive>" and backs up when the drive appears (a udev rule or path unit starts the pending backup). |
| A different disk is mounted at the same path | The repository ID doesn't match: Carryon stops and asks. Only `init` ever creates a repository. |
| The destination is on a FUSE or network mount (rclone mount, sshfs, GVFS, SMB, NFS) | Root usually can't read a user's FUSE mount, and rclone's VFS cache can report a write as done before it's uploaded. `init` detects these and offers the direct backend instead (`rclone:remote:path`, `sftp:`), so no mount is involved. |
| Carryon or its config sits on such a mount | The packaged install lives in `/usr` and `/etc`. A tarball install copies itself to `/usr/local/bin` first, because the root service starts at boot before any user mount exists. |
| Mounts inside a home folder | Cloud folders and network shares are left out of the backup by default, and so is the repository's own folder. |
| Offline, or the destination is unreachable | The run is recorded as pending and retried by the settle timer; the user is alerted only when the newest backup is older than `max_age`. |
| Full destination | Free space is checked before writing where the backend allows it. |
| A login expired | The error is recognised and the alert carries the exact fix command. |

## Running by itself

| Unit | What it does |
|---|---|
| `carryon-backup.timer` / `.service` | Daily backup (`Persistent=true`, randomized delay, `Nice=19`, idle I/O). |
| `carryon-verify.timer` / `.service` | Weekly deep check. |
| `carryon-settle.timer` / `.service` | **Fix** for the lost transient timer: every 10 minutes, but the service only starts when `/var/lib/carryon/pending` exists (`ConditionPathExists=`). It backs up once the last change is older than `after_change_delay` (default 1 hour). Survives reboots. |
| `carryon-notify@.service` | `OnFailure=` of the others: alerts the user. |
| `carryon-add@.service` / `carryon-ignore@.service` | **New:** the agent's **Back it up** and **Ignore** buttons: settle one inbox entry, as `carryon add ID` and `carryon ignore ID` do. See [who runs what](#who-runs-what). |
| `carryon-restore@.service` | **New:** restores one app's offered settings (`carryon restore --offered --bundle %i`), see [as you go](#as-you-go). |
| `carryon-flatpak.path` / `.service` | **New:** when system-wide Flatpak apps change (`/var/lib/flatpak/app`), refreshes the record with `carryon capture --quick`, as the hook does for packages. |
| `carryon-agent.service` (user unit) | **New:** the user's notifications with buttons, and later the tray icon. Runs in each desktop session (`graphical-session.target`). |
| `/usr/share/libalpm/hooks/carryon.hook` | After every install, upgrade or removal: quick capture, write the pending marker, offer saved settings of apps just installed, print hints. Never fails or blocks the transaction. |

- **One backup at a time:** a lock in the state folder. A scheduled or settle run that finds
  another backup running does nothing; `carryon backup` by hand says so.
- **Order of a run:** **Changes from the prototype.** The destination is checked first, before the
  capture, so a run that can't write does nothing at all (the pacman hook still keeps the record
  fresh). When the capture fails (say the package database is locked), the backup goes on with the
  last record and ends with a warning: the prototype stopped, which could leave the files without
  any backup for days.
- **Settle after a failure:** when a settle run fails, the pending marker gets the current time, so
  the next try, and the next alert, waits another `after_change_delay` instead of repeating every
  10 minutes.
- **Run history:** **Fix.** The prototype read errors back from the journal, which some
  distributions keep for about a day. Carryon keeps its own history in `/var/lib/carryon/runs.jsonl`
  and writes a summary, `/run/carryon/status.json` mirrored to `/var/lib/carryon/status.json`, that is
  world-readable and holds no secrets: last runs, results, short error text, next runs, inbox count.
  **Fix:** so `carryon status` works without root and after reboots.
- **`carryon status`:** reads `status.json` (the state folder's copy after a reboot), plus the
  pending and restoring flags, which can be newer: the pacman hook marks a backup owed without
  rewriting the summary. It says what needs attention and what to do: a failed backup or check, a
  backup with warnings, the newest backup older than `verify.max_age_hours`, backups paused by a
  restore, and a summary older than that, which means the timer may not be running. A backup waiting
  for its destination needs attention only once the newest backup is too old, like the alerts.
  **Changes from the prototype:** it reads Carryon's own history instead of asking systemd, so it
  works after the journal is gone; as root it shows the record's latest changes and, with
  `--snapshots`, the snapshots per machine, but only after checking the destination like a backup
  does, and with restic's `--no-lock`, so a status check never writes into the repository. `--json`
  exits with 2 when something needs attention, for status bars.
- **Login message:** **New.** Whenever `status.json` is written (after a backup, check, capture or
  pacman hook run), Carryon also writes one line to `/run/motd.d/carryon.motd` when something needs
  attention, by the same rules as `carryon status`, and removes it when all is well:
  `Carryon: the last backup failed (Oct 18 14:02). See: carryon status`. `pam_motd`, which Arch's
  `system-login` PAM stack includes, shows every file in that folder at console and SSH logins, so
  no program runs at login. The line gives dates, not "2 days ago", so it stays true until it is
  next written. `notify.login_message = false` removes it and stops writing it. Failing to write it
  is a warning in the run's log; it never fails a backup or the status write. `/run` is emptied at
  boot, and nothing writes the line again then: it comes back with the next run that writes the
  status (a scheduled backup missed while the machine was off runs soon after boot, and the pacman
  hook runs on the next package change), and `carryon status` always has the full picture. A
  problem that only appears with time, like the newest backup getting too old because no backup
  runs at all, shows in the line only once something writes the status again.
- **Failure alerts:** `carryon-notify@.service` runs `carryon notify UNIT`, which sends the unit's last
  error lines through every channel in `[notify]`. **Generalized:** the prototype alerted one user;
  Carryon alerts every user with an open graphical session who is in `backup.users` and hasn't
  switched desktop notifications off in their own settings, running `notify-send` (or `busctl` without
  libnotify) as that user. A user whose agent runs gets the agent's notification instead, with
  buttons (see [interfaces](#interfaces)). **Generalized:** the prototype's "iCloud signed you out" check works for any
  `rclone:` remote and names it with the reconnect command, without probing a fixed website first.
  **New:** ntfy and webhook alerts, with passwords, keys and `user:password@` hidden before anything
  leaves the machine, and `carryon notify --test` to try every channel. With a missing or broken
  config, it still alerts with the default settings.
- **Verify:** `carryon verify` checks the record, whether everything in it can be reinstalled,
  whether what the machine runs is backed up, and the repository; `--deep` also lists the newest
  snapshot, runs `restic check --read-data-subset` and restores the record into a temporary folder.
  Like a backup, it checks the destination first and runs no restic command on one that doesn't
  match; read-only restic commands use `--no-lock`. Each run goes into the run history (kind
  `verify`). **Changes from the prototype:** an unconfirmed recovery kit is a warning, not a
  failure; the AUR or an extra repository that can't be reached while the machine is offline is a
  warning; an extra repository's `Include`d mirror list counts as its servers; the drift check uses
  the quick capture, so files only the nightly capture finds (`edited`) aren't compared; a program
  started by a unit counts as backed up when it is in any backed-up folder, not only a home; when
  the destination isn't there, the age of the last backup comes from the run history; and the
  prototype's Toolkit comparison is gone with the Toolkit.
- **Paused while restoring:** a `restoring` flag stops automatic backups on a fresh install, so a
  half-restored system never becomes the newest snapshot. Kept from the prototype. **Change:** the
  pacman hook does nothing at all while it is there; the prototype still refreshed the record.
  Restoring as you go sets no flag: backups start at once (see [as you go](#as-you-go)).
- **Sleep:** a running backup holds a logind inhibitor so the machine doesn't suspend mid-upload.
  **New.**
- **Consistent copies:** on btrfs, a later version backs up from read-only snapshots of the
  subvolumes, so databases aren't read while they change. **New, v0.3.**
- **Nested subvolumes and mounts:** **Fix.** The prototype's `--one-file-system` silently skipped
  them. Carryon lists the filesystems inside each backed-up folder, includes nested btrfs subvolumes of
  the same device, and reports other mounts it leaves out. Mounted subvolumes come from the mount
  table; unmounted ones (made with `btrfs subvolume create`) show up as a change of device number,
  so on btrfs Carryon walks the backed-up folders, skipping what the exclude list leaves out.
  Snapper's `.snapshots` subvolumes are left out and reported, since they hold copies of everything
  else. What is left out is listed in the run history and `status.json` as notes; adding a path to
  `backup.extra_paths` takes it in.

## Noticing new things

**New.** Most of the machine is captured automatically, as in the prototype: home folders, `/etc`,
`/usr`, `/opt`, packages and services. Detection covers what that misses, and labels what it finds.

1. **Triggers:** the pacman hook (with the changed package names), the nightly capture, and a
   Flatpak watcher.
2. **What it looks for:** folders in `/var/lib`, `/var/opt` and `/srv` (and the subfolders of
   `/var/lib/private`, where units with `DynamicUser=yes` keep their state) that hold at least one
   file no package owns (for example `/var/lib/jellyfin` filling up after Jellyfin was installed).
   Empty folders and folders of package files only are left alone, and so are: known system folders
   (`pacman`, `systemd`, `dbus`...), folders already backed up or left out by the excludes and the
   catalog, ignored ones, symlinks, and other filesystems mounted there. The known system folders
   ship as data, `/usr/share/carryon/system-folders.txt` (`catalog/system-folders.txt` here), so
   the list can grow without a new binary; without it nothing is scanned, since the system's own
   state would fill the inbox, and the backup warns.
3. **Who owns it:** the probable app is, in this order, the package owning the folder itself, the
   package of an enabled unit whose `StateDirectory=` it is, a package named like it
   (`/var/lib/jellyfin` ↔ `jellyfin-server`, packages installed on purpose first), or the package of
   an enabled unit whose `User=` owns it (never root). The suggestion's bundle is the app bundle the
   record already puts that package in, else `app-<package>`.
4. **Suggestions:** each finding becomes an inbox entry with its size and probable app. Accepting it
   adds the folder to `extra_paths` and to that app's bundle. Big folders (over 5 GiB by default) are
   marked so the user can decide. Sizes don't count what is mounted inside the folder.
5. **Telling the user:** a desktop notification with **Back it up** and **Ignore** buttons, a line in
   pacman's output in a terminal, `carryon inbox`, and a count in `carryon status`.

`detect.new_paths = "ask" | "auto" | "off"` in the config controls this (default `ask`). With
`auto`, a found folder is backed up exactly as `carryon add` would (from the next backup on), and
the run history notes it. With `off`, nothing is scanned.

- **Every backup scans everything** (the nightly one, and settle and manual runs too), after the
  capture and before restic. A suggestion it doesn't find again (backed up, ignored, owned or gone
  since) is taken out of the inbox. What it found goes in the run's notes.
- **The pacman hook** looks only at the folders of the packages pacman gave it that are still
  installed: the folders they own files in or that are named like them. It asks pacman about at
  most 200 of a folder's files, waits for the inbox lock for 2 seconds at most and skips the
  suggestion when it's busy, and stops a little before the hook's time limit, so the hook's run is
  still recorded. Each new entry prints one line in pacman's output, with the command that backs it
  up. An app's data usually appears later, when it first runs; the nightly scan finds it then.

**The inbox** is `/var/lib/carryon/inbox.json`, `{"format": 1, "entries": [...]}`, world-readable
and free of secrets: paths, sizes, app and bundle names, never file contents. Each entry asks one
thing: `path` (back up this folder?) or `restore` (restore this app's saved settings? See
[as you go](#as-you-go)). It has its size, probable app and bundle, whether it's large, and when and
by what it was first found. Its ID is the kind and ten hex digits of a hash of the path or bundle
(`path-3fa9c2d1e0`): the same each time the same thing is found, and safe as a systemd instance
name, so the agent's buttons never pass a path.

- **Writers** (the pacman hook, the nightly backup, the commands) change it under a lock of its
  own, and each says how long it waits: the hook gives up after a moment instead of blocking
  pacman. Readers need no lock; the file is always replaced in one step. A damaged file is set
  aside as `inbox.json.damaged` and the inbox starts again empty.
- **Settling:** `carryon add PATH|ID` adds the folder to `backup.extra_paths`, and to its app's
  bundle with a `put` line in `bundles.conf` when grouping wouldn't find it by name.
  `carryon ignore PATH|ID` adds it to `detect.ignore` in `config.toml`, so the choice travels with
  `/etc/carryon` in the backup; a folder inside an ignored one isn't suggested either. Either
  command settles the suggestions for that folder and the folders inside it. Ignoring a restore
  offer keeps its bundle in the inbox's `ignored_bundles`, so it isn't offered again.
  `carryon inbox` lists the entries (`--json`) and, as root in a terminal, goes through them: back
  it up, ignore, or later.
- `carryon status` counts the entries from the inbox itself, since settling one doesn't rewrite
  `status.json`.

## Restoring

### Everything, from a plan

Kept from the prototype, with a terminal UI instead of whiptail and fzf:

1. Choose the machine and snapshot to restore from.
2. Tick bundles. Each starts ticked or not by its when-rule, and the reason is shown.
3. Optionally open bundles to untick single items. Choices are remembered per source machine.
4. Read the plan: only items that differ from this machine, with full diffs in
   `/var/lib/carryon/restore-plan.txt`. Confirm.
5. Apply, in this order: extra repositories and keys; one full system update plus packages (falling
   back to one package at a time); an AUR helper (paru or yay) and AUR packages as the user; system
   files (old versions kept as `*.pre-restore`); data from the repository; groups and shells; services;
   folders; user units; kernel parameters and initramfs; language tools. Everything that failed is
   listed at the end; the rest still applies.

Safety rails, all kept:

- Files with disk UUIDs (fstab, crypttab, bootloader configs, `/etc/kernel/cmdline`) are shown as a
  diff and never copied; kernel parameters are added separately.
- An initramfs config is never copied if it lacks storage hooks this machine needs (`encrypt`,
  `sd-encrypt`, `lvm2`, `mdadm`, `zfs`, `resume`, `btrfs`, `bcache`).
- Machine identity, Bluetooth pairings, login and initramfs settings start unticked unless it's the
  same machine.
- When the username or home path differs, paths inside restored text files (up to 2 MiB) and
  symlinks are rewritten, and `User=` lines are adapted.
- Folders owned by service users are handed back to that user by name when the numeric IDs differ.
- The rclone config being used for this restore is kept; other remotes from the backup are merged in.
- Home files aren't restored while the user's desktop session is running, because desktops rewrite
  their settings at logout. **Generalized** from a Plasma-only check to any graphical session. For
  one app's bundle restored on its own, the app must be closed instead (see [as you go](#as-you-go)).
- App notes after restore (such as "KDE Wallet opens with the old login password") come from the
  catalog. **Generalized.**

Where planning differs from the prototype on purpose:

- **Records from both tools.** Only the record is fetched from the snapshot, from
  `/var/lib/carryon/record` or, for the prototype's snapshots, `/var/lib/sysbackup/manifest`.
- **One checklist.** Single items are unticked inside the bundle checklist, instead of a second
  "fine-tune" list. Remembered choices live in `/var/lib/carryon/selections/<machine>.json` (the
  prototype kept them in `/etc/sysbackup/selections/`), and are saved only when the user actually
  picked; `--defaults` and runs without a terminal take the default ticks and remember nothing.
- **Several machines without a terminal:** when this machine's own name is among them, it's taken;
  otherwise `--from` is required, as before.
- **The plan file says more:** diffs are made by Carryon itself (no `diff` needed), user unit files
  get diffs too, review-only files say why they're never copied, and per-user steps say which user
  here they're for and where they go.

Where applying differs from the prototype on purpose:

- **Asking first.** Applying needs a yes to "Apply this plan?" after the plan is shown, or `--yes`
  without a terminal; `--defaults` alone shows the plan and stops. The prototype's `--yes` only
  stopped pacman from asking.
- **Backups stay paused after a failure.** The prototype started automatic backups again even when
  steps had failed. Carryon keeps the restoring flag until a full restore applies cleanly, says how
  to finish (run it again: only what's still missing is done) or how to start backups anyway.
  Nothing to do counts as finished.
- **The first `*.pre-restore` copy is kept.** Running the restore again replaces what the first run
  wrote, not the original copy.
- **Rails checked again while applying.** Review-only and boot-risk files are checked on the path a
  file would really be written to, after following symlinks on this machine, so neither a changed
  plan nor a link can copy them. Data from the backup is never restored over them either. Writes
  into a home never follow a symlink out of it.
- **Several users.** Each user's home files are checked for a desktop session and handed to that
  user; folders made on the way (such as `~/.config`) are theirs too, not root's. Without a
  terminal, home files of a signed-in user are skipped and listed.
- **AUR helper.** paru or yay if installed, else one from the configured repositories, else
  `paru-bin` built from the AUR, always as the user. The prototype only tried `pacman -S paru`.
- **A missing initramfs tool is reported** instead of skipped, since the machine may not boot
  without it.

### As you go

**New.** On a fresh machine connected to an existing repository, nothing has to be restored up front.
Carryon keeps the source machine's record, and offers each app's saved settings when the app gets
installed.

- **Choosing it.** `carryon init --connect` asks whether to restore everything now (the restoring
  flag pauses backups until `sudo carryon restore` is done, as before) or as you go: no flag, backups
  start at once, and the record of the machine whose settings are reused is kept. Without a
  terminal, `--restore now|as-you-go` chooses, and the default is now, as before. As you go needs a
  machine to restore from, so with `--settings-from none` it's now. The pacman hook does nothing
  while the restoring flag is there, so restoring everything now makes no offers until the restore
  is done; after it, the bundles that weren't picked are offered as their apps get installed.
- **The kept source.** `/var/lib/carryon/source/` holds a copy of the source machine's record
  (without its git history) and `source.json`: the machine, the snapshot, whether Carryon tagged it,
  and the bundles restored from it. It's root-only, like the record. `init --connect` keeps one when
  restoring as you go, and every `carryon restore` from the backup keeps the one it used, replacing
  one from another snapshot and its offers. A record on disk (`--record`) isn't kept: there's no
  snapshot to restore data from.
- **Retention.** When this machine backs up under the source machine's name, its own retention
  would one day forget the snapshot offers restore from. Carryon tags that snapshot
  `carryon-source`, and every `forget` keeps snapshots with that tag (`--keep-tag`). restic gives a
  snapshot a new ID when its tags change, so the kept record follows it. Tagging writes into the
  repository, so it happens only after the destination check. Under another name there's nothing to
  tag: a machine's retention only forgets its own snapshots. `sudo carryon restore --stop-offers`
  takes the tag off, drops the kept record and closes the offers.
- **Offering.** Whenever the pacman hook sees a package installed whose app's bundle in the kept
  record has something to change here, it offers the bundle once:
  - in a terminal, pacman's output says `carryon: saved settings for obs-studio (9 Oct) — restore
    with: carryon restore --bundle app-obs-studio`;
  - the offer is an inbox entry (`restore`) whose `changes` say in short lines what restoring would
    change: places and counts, never file contents, since the inbox is world-readable ("2 system
    files in /etc/obs", "restores ~/.config/obs-studio (alice)", "enables obs-relay.service").
    `carryon inbox` lists it, and on a desktop the agent shows it with **Restore** and **Not now**.

  pacman tells its hooks every package it changed without saying which were installed, and an
  upgrade is no reason to offer anything, so "installed" means: pacman named it, it's installed now,
  and the record didn't hold it before the hook refreshed it (without a record yet, every installed
  package pacman names counts). Only apps' bundles are offered, not the built-in or hardware ones,
  whose settings belong to the whole machine, and only when their when-rule ticks them here. A
  bundle that was restored or ignored, or whose offer is waiting, isn't offered again. An offer
  covers what isn't software: system files, data from the backup, groups and shells, services,
  folders, user units and kernel parameters. Packages, AUR packages and language tools are the
  user's to install, and files with disk IDs are never copied, as always. The offer runs after the
  record refresh, inside the hook's time limit, waits at most two seconds for the inbox lock, and
  what goes wrong becomes a hint.
- **Applying an offer.** **Restore** starts `carryon-restore@<bundle>.service` through polkit,
  which runs `carryon restore --offered --bundle <bundle>` as root, without a terminal. It refuses an
  instance name that isn't a bundle ID (letters, digits and `@._+-`, never a path or an option;
  systemd's `\x2b`-style escapes are read back), a bundle the kept record doesn't have, and a bundle
  that wasn't offered. Restore never applies anything the user hasn't seen, so it applies only the
  plan that was offered: a fingerprint of that plan (each step, where it goes, and the diff of each
  file) is kept in `/var/lib/carryon/source/offers.json`, root-only since it's made from file
  contents. When the plan is different now, nothing is applied: the offer is renewed with the new
  changes and the unit fails, so the user sees the new plan before choosing again. Users are matched
  as without a terminal: by name, else the only user here.
- **One app's home files.** Home files aren't restored while their owner's desktop session runs,
  because desktops write their settings back at logout. That's about the desktop's settings: one
  app's own folder is safe while the app isn't running, and restoring it mid-session is the point of
  restoring as you go. **Change:** for an app's bundle restored on its own (by the unit or with
  `--bundle`), Carryon checks instead that none of the user's processes runs a program of the app's
  packages (from `pacman -Ql`, against each process's program and first argument in `/proc`), and
  asks, or without a terminal skips them and says to close the app. When it can't tell, the desktop
  check applies.

`carryon restore --bundle ID` does the same by hand, from the kept record unless `--from`,
`--snapshot` or `--record` say otherwise, with the same plan, confirmation and safety rails,
packages included. It leaves the restoring flag alone. A bundle restored cleanly isn't offered
again.

### Several users

Users are matched by name. If a source user doesn't exist on the new machine, the restore asks which
user receives their files (by default, with one user in the backup, whoever runs `sudo carryon
restore`), or `--user OLD=NEW` says it. Paths and `User=` lines are adapted for every user whose
name or home differs, in one pass, so `alice→bob` and `bob→carol` don't chain.

## Configuration

Files in `/etc/carryon/`, written by `carryon init` with comments:

```toml
# /etc/carryon/config.toml
config_version = 1
machine_name = "desktop"

[repository]
url = "/mnt/backup/carryon"          # any restic repository URL
password_file = "/etc/carryon/repository-password"
# rclone_user = "alice"              # for rclone: URLs, whose rclone login to use

[backup]
schedule = "daily"
after_change_delay = "1h"
extra_paths = ["/root", "/srv", "/usr/local"]
users = ["*"]

[retention]
keep_within = "2d"
keep_daily = 7
keep_weekly = 4
keep_monthly = 6
prune_every_days = 7

[verify]
schedule = "weekly"
max_age_hours = 48
read_subset = "2%"

[detect]
new_paths = "ask"                    # ask | auto | off
large_path_gb = 5
# ignore = ["/srv/scratch"]         # never suggest these folders

[notify]
desktop = true
login_message = true                 # a line at console and SSH logins while something needs attention
# ntfy_url = "https://ntfy.sh/my-topic"

[pacman]
# extra_standard_repos = ["myrepo"]
```

- `excludes.txt`: the user's restic exclude patterns. Defaults ship in
  `/usr/share/carryon/excludes.default` and update with the package; the user can switch single
  defaults off. Carryon builds one effective exclude file per run and uses it **both** for restic and
  for its own matching, with restic's pattern rules. **Fix** for the prototype's two matchers
  disagreeing. The file holds, in order: the defaults and the catalog's patterns (minus the ones
  switched off), mount points and the repository's own folder (matched literally), then the user's
  patterns, last so that a `!` pattern can take back a default.
- `bundles.conf`: bundle rules, as above.
- **Per-user settings**, `~/.config/carryon/config.toml`: excludes and notification preferences for
  that user's home. The root service never follows a user's setting outside that user's home.
- **Catalog**, `/usr/share/carryon/catalog/*.toml`: what Carryon knows about apps: extra excludes,
  how excluded data comes back, post-restore notes. Shipped with the package; contributions welcome.
  One file per app, described in [catalog/README.md](../catalog/README.md). Its patterns apply
  whether or not the app is installed, and can be switched off like the defaults.
- `carryon config check|get|set|edit` validates and changes settings. Unknown keys are reported.
  Settings from older versions are migrated (`config_version`). **Fix:** the prototype never updated
  an existing config.

## Interfaces

| Command | Does |
|---|---|
| `carryon init` | Wizard: destination, password, schedule, users. `--connect URL` joins an existing repository. |
| `carryon status` | Last runs, next runs, problems and inbox count. Works without root. |
| `carryon backup` | Capture and back up now. |
| `carryon capture` | Refresh the record only. |
| `carryon verify [--deep\|--full]` | Is the backup current, complete and restorable? |
| `carryon restore` | Pick and apply. `--plan` only shows the plan; `--bundle ID` restores one bundle; `--stop-offers` stops restoring as you go. |
| `carryon bundles` | How this machine is grouped, ticked as it would be here. |
| `carryon inbox` / `add` / `ignore` | Review suggestions; add or ignore paths. |
| `carryon exclude PATH` | Leave something out. |
| `carryon why PATH` | Which item, bundle and rule cover a path, or which exclude leaves it out. |
| `carryon history` | Changes to the system over time, from the record's history. |
| `carryon config ...` | Show, check and change settings. |
| `carryon recovery-kit` | Show what's needed to get back in on a new machine. |
| `carryon restic ARGS...` | Any restic command on the repository, with password and backend handled. |
| `carryon version` | Version and build info. |

Hidden, used by units and hooks: `hook`, `settle`, `notify`, `agent`, `rclone-shim`.

- **`carryon why PATH`** (**New**) reads the same settings a backup does and answers from them: the
  backed-up folder holding the path, the first thing that leaves it out on the way down from that
  folder (an exclude pattern and where it comes from, a `CACHEDIR.TAG` folder, or another
  filesystem), a `!` pattern taking it back, the nested subvolume it's on, and how a package file
  you haven't changed or data that downloads itself comes back, and a folder in `detect.ignore` or
  waiting in the inbox. As root it also reads the record:
  the items holding the path, their bundle and when-rule, whether that starts ticked here, and the
  grouping step that put each item there. The record doesn't store the step: `why` groups the
  record's items again with today's `bundles.conf`, and says so when that would move an item.

- `--json` on read commands, so scripts and status bars can use them.
- **Terminal UI** (Bubble Tea) for `init`, the restore checklist and the inbox. Every prompt has a
  flag, so everything also works without a terminal.
- **Notifications** come from the user agent over D-Bus, with action buttons. When no agent runs, the
  root service falls back to a plain notification in the user's session. **New**, and below.

**The user agent** is `carryon agent`, run by `carryon-agent.service`, a user unit `carryon init`
switches on for every user (`systemctl --global enable`). It starts with the desktop session
(`graphical-session.target`), so a console or SSH login doesn't start it, and a window manager that
doesn't reach that target simply keeps root's plain notifications. It refuses to run as root.

- **What it shows,** each once, through `org.freedesktop.Notifications`:

  | Event | Buttons | Starts |
  |---|---|---|
  | The last backup failed | **Back up now** | `carryon-backup.service` |
  | The newest backup is older than `verify.max_age_hours` (not while a failure says it already) | **Back up now** | `carryon-backup.service` |
  | The last check found problems | none: the body says what to do | |
  | A suggested folder (inbox `path`) | **Back it up**, **Ignore** | `carryon-add@<entry>.service`, `carryon-ignore@<entry>.service` |
  | Saved settings to restore (inbox `restore`) | **Restore**, **Not now** | `carryon-restore@<bundle>.service` |

  Problems come from the same rules as `carryon status` (`internal/status`), so the two never
  disagree. Backups with warnings, an old summary and a paused backup show in `carryon status` and
  the login message only: they don't need a decision now. An instance name is an inbox entry ID or a
  bundle ID, checked before it is used, never a path; a bundle ID's `+` or `@` is escaped as
  `systemd-escape` does (`carryon-restore@app-gtk\x2b.service`). A service without buttons gets the command to
  run instead (`sudo carryon add path-3fa9c2d1e0`), and a body that may hold markup is escaped.
- **Buttons** run `systemctl start --no-block` as the user, which polkit allows or asks a password
  for. When that fails, a notification says why and what to run by hand. **Not now** only
  closes the offer: it stays in `carryon inbox`, and isn't shown again.
- **What a restore changes:** the offer lists its `changes` lines from the inbox (places and
  counts, never file contents). When the plan changed and the offer was renewed with new lines,
  it is a new event and shows again, so the user sees the plan **Restore** will apply.
- **Restoring an app that runs:** the agent runs as the user, so it sees their processes: the
  programs the app's package installs (`pacman -Qlq`) or one named like it. The offer says to close
  the app first, and **Restore** checks again: while the app runs, it shows a warning with
  **Restore** and **Not now** instead of starting anything.
- **Once, across restarts:** what was shown is kept in `~/.local/state/carryon/agent.json`
  (`$XDG_STATE_HOME/carryon` when that is inside the home), readable by the user only. An event is
  a run's end time or an entry's ID (with its changes, for a restore offer), so the next night's
  failure shows again; events that are over
  are forgotten, but only when both files could be read. Nothing counts as shown until the
  notification service took it, so a desktop that isn't ready yet gets it a minute later.
- **Settings:** the user's own `notify.desktop = false` (`~/.config/carryon/config.toml`) shows
  nothing and remembers nothing, so switching it back on shows what still needs attention; users
  `backup.users` leaves out get nothing, as from root. Both are read again whenever they change.
- **Watching:** it looks at `status.json` (both copies), the inbox, the flags and both settings
  files every 5 seconds, and decides again at least every 10 minutes, since a backup grows too old
  without any file changing. A few `stat` calls every 5 seconds cost nothing, need no inotify, and
  find files that don't exist yet, like `/run/carryon` after a reboot.
- **Not twice:** `carryon notify` asks each user's own service manager, as that user, whether their
  agent runs (`systemctl --user is-active carryon-agent.service`), and leaves those users' desktop
  notification to it, but only for a failure in the run history, since that is in `status.json`,
  where the agent finds it. A run that crashed before recording itself still gets root's plain
  notification. ntfy and webhooks are unchanged. The answer only decides who shows a user their
  own notification: faking it can only silence root's notification to that same user.
- **Tray icon** (v0.3), a StatusNotifierItem over D-Bus. KDE and most bars show it; GNOME needs the
  AppIndicator extension.
- **No desktop:** hints in pacman's output, `carryon status`, a one-line login message when
  something needs attention (on by default, see "Running by itself"), and alerts through ntfy or a webhook.

## Who runs what

| Part | Runs as | Reads | Writes |
|---|---|---|---|
| Backup, verify, settle, restore | root (systemd or `sudo`) | everything backed up | record, state, repository |
| pacman hook | root, inside pacman | package lists, the kept record | pending marker, inbox, offers' fingerprints, hints on stdout |
| Agent | the user | `status.json`, inbox, flags, `config.toml`, the user's own settings, the user's own processes | `~/.local/state/carryon/agent.json`, nothing outside the user's home |
| Actions from the agent (Back up now, Back it up, Ignore, Restore) | root, via polkit-allowed `systemctl start` of specific units, named by an entry or bundle ID | the inbox, to check the ID | settings, inbox; a restore writes what it restores |

- `/etc/carryon/repository-password`, `/var/lib/carryon/record/` and `/var/lib/carryon/source/` (the
  kept record and the offers' fingerprints) are root-only.
- `status.json` and the inbox are world-readable and hold no secrets.
- A user's own settings can only add or remove paths in their own home.
- The agent never runs as root.

**The agent's buttons** start root's units with `systemctl start`, and
`/usr/share/polkit-1/rules.d/carryon.rules` says who may, without sudo:

| Unit | Button | Who may start it |
|---|---|---|
| `carryon-backup.service` | Back up now | anyone in a local, active session |
| `carryon-restore@BUNDLE.service` | Restore | members of `wheel`; others type an administrator's password (`auth_admin_keep`) |
| `carryon-add@ID.service`, `carryon-ignore@ID.service` | Back it up, Ignore | the same as Restore |

A backup only does what the timer does anyway, so anyone at the machine may start one. Restoring a
bundle or settling a suggestion changes what root writes or backs up, so it needs an administrator.
The rules match only the `start` verb of these units in a local, active session (polkit counts a
user service, like the agent, as part of the user's graphical session), and only instance names
shaped like an inbox entry ID (`path-3fa9c2d1e0`) or a bundle ID (a letter or digit, then letters,
digits and `@._+-`, with `+` and `@` written `\x2b` and `\x40` as the agent and `systemd-escape`
write them, and `-` also as `\x2d`; no other escape, so never `\x2f` for `/`). Anything else
(another verb or unit, a remote or inactive session, a name like a path) falls through to polkit's
other rules and systemd's defaults, which ask for an administrator's password.

Each unit checks its instance name again, since polkit's rules can be changed and root can start
any unit: the instance is passed as written (`%i`; `%I` would turn `-` into `/`), glued to a hidden
flag (`carryon add --entry=%i`, `carryon restore --offered --bundle=%i`) so it can't become an
option, and refused unless it is the ID of an entry in the inbox right now, or for a restore the
bundle of an offer. `--entry` takes no path, unlike `carryon add PATH|ID`, so a user can never pass
a path or an option to root. A refused or failed unit alerts the user like the others
(`carryon-notify@`). The add and ignore units are sandboxed: they can write only `/etc/carryon` and
`/var/lib/carryon`, with no network.

InputLine uses the same split: a root service plus a helper for each signed-in user.

## Getting back in

Restoring needs the password, the repository URL, and Carryon. **Replaces** the prototype's
unencrypted Toolkit copy next to the repository:

- **Recovery kit:** `init` shows the repository URL, machine name, password and the restore steps,
  and asks the user to confirm they saved them. `verify` keeps warning until they do. `carryon
  recovery-kit` shows it again.
- **Settings travel inside the snapshots:** `/etc/carryon` is part of every backup. On a new machine,
  `carryon init --connect URL` lists the machines in the repository and offers to reuse one's
  settings.
- **On a new machine:** install Carryon from the AUR, run `sudo carryon init --connect URL` and enter
  the password. Restoring everything now pauses backups until `sudo carryon restore` finishes;
  restoring as you go starts them at once (see [as you go](#as-you-go)).
- **Without Carryon:** the kit explains how to get files back with plain restic.
- **What `init` checks:** before anything is written, a local folder on a FUSE or network mount gets
  the direct backend offered, and a folder under `/mnt`, `/media` or `/run/media` that is really on the
  system disk (an empty mount point) needs confirming (`--yes` without a terminal). It records the
  destination in `/var/lib/carryon/destination.json` for every backup to check, and asks before
  recording a different repository at the place backups went before. The password is saved before
  `restic init` runs, so it can't be lost in between; a replaced password file is kept as `.old`.
- **Changed from the prototype:** `install.sh` asked "is this a fresh install?" and had the user type
  `saved` for the password. Carryon uses `--connect` for a fresh install, and a yes/no question (or
  `--kit-saved`) for the kit. `--connect` counts typing the password in as having the kit. It never
  takes another machine's password file or destination record from a backup, drops a
  `password_command` (it may only work on that machine), and keeps replaced files as
  `<file>.before-<machine>`. The new machine keeps the old name only when its hardware fingerprint
  matches the reused machine's record; otherwise the default is its hostname.

## Code layout and libraries

Go 1.24 or newer, `CGO_ENABLED=0`, one static binary.

```
cmd/carryon/          main
internal/cli/         commands (cobra)
internal/paths/       standard locations, overridable for tests
internal/config/      config.toml: defaults, loading, validation, migration
internal/ui/          terminal output with journal levels; prompts and the terminal UI
internal/sysexec/     running programs, as root or as a user (fakeable in tests)
internal/record/      items, meta, bundles: reading, writing, diffing, summary, history
internal/rules/       when-rules and bundles.conf
internal/facts/       hardware and machine facts
internal/pkgmgr/      package-manager interface; pacman/ implementation
internal/capture/     collectors that build the record
internal/bundle/      grouping items into bundles
internal/catalog/     app knowledge: excludes, re-download hints, notes
internal/engine/      restic: running, parsing, errors; the rclone shim
internal/dest/        destination guards
internal/state/       run history, status.json, inbox, flags
internal/backup/      the backup run
internal/verify/      checks
internal/restore/     source, plan, apply
internal/detect/      hook handling and scans
internal/notify/      desktop and push notifications
internal/agent/       the per-user agent
catalog/              app definitions (TOML)
internal/tools/gendocs/  man page and shell completions, generated at build time
dist/                 systemd units (system, and the user agent's), pacman hook, polkit rules
packaging/            PKGBUILDs (aur/), the install layout check
```

Libraries: `spf13/cobra` (commands), `BurntSushi/toml` (config), `charmbracelet/bubbletea`, `bubbles`,
`lipgloss` and `huh` (terminal UI), `godbus/dbus/v5` (notifications, systemd, logind, polkit, tray).
Programs it runs: `restic` (required), `pacman` (on Arch), `git` (optional, for history), `rclone`
(optional), `paru` or `yay` (restore of AUR packages).

**Fix:** the prototype's `sh` returned whatever a program printed even when it failed, and an empty
string when the program was missing or timed out, so a broken call could look like "nothing found".
Carryon runs every program through `internal/sysexec`, which returns the output **and** an error
naming the command, its exit status and the end of its error output. Each caller decides whether a
failure matters: `pacman -Qkk` exiting non-zero because it found changed files is a result, a missing
`pacman` is not.

## Install layout

| Path | What |
|---|---|
| `/usr/bin/carryon` | the binary |
| `/usr/lib/systemd/system/carryon-*.{service,timer,path}` | system units |
| `/usr/lib/systemd/user/carryon-agent.service` | user agent |
| `/usr/share/libalpm/hooks/carryon.hook` | pacman hook |
| `/usr/share/polkit-1/rules.d/carryon.rules` | who may start the agent's units: backups, bundle restores, settling suggestions |
| `/usr/share/carryon/` | default excludes, known system folders, catalog |
| `/usr/share/man/man1/carryon.1` | the man page, generated from the commands |
| `/usr/share/{bash-completion/completions,zsh/site-functions,fish/vendor_completions.d}/` | shell completions |
| `/usr/share/doc/carryon/`, `/usr/share/licenses/carryon/` | README, this design, the catalog's README; the licence |
| `/etc/carryon/` | config, excludes, bundle rules, password (created by `init`) |
| `/var/lib/carryon/` | record, history, inbox, flags |
| `/var/cache/carryon/restic/` | restic cache |

`make install` lays out the `/usr` part; packages install nothing under `/etc` or `/var` and enable
no units, `carryon init` does. `packaging/layout.txt` pins the list and CI checks it, on the release
tarballs and on the installed pacman package too. The user agent's unit lives in `dist/systemd-user/`,
apart from the system units in `dist/systemd/`. The polkit rules folder is 755 root:root, as the
polkit package has it since polkit 124; `polkit` is an optional dependency, since without it only
root starts the units.
The binary is static and not a PIE, against the Arch Go guidelines: without cgo, a Go PIE needs the
system's loader, and the same binary must run from the recovery kit anywhere. See
[packaging.md](packaging.md).

## Testing

- **Unit tests** use fakes: a fake package manager, a fake program runner, and temporary folders via
  `CARRYON_CONF_DIR`, `CARRYON_STATE_DIR` and `CARRYON_ROOT` (a fake filesystem root for capture).
- **The user agent** is tested against a fake notification service on a private `dbus-daemon`
  (skipped where `dbus-daemon` isn't installed): each event shows once, with the right buttons, also
  after a restart, and each button starts the right unit through a fake `systemctl`. Its rules
  (settings, retries, the app check on a fake `/proc`) are tested with a notification service in
  memory. A real desktop session, polkit and the units behind the buttons are left for the end-to-end
  tests and real machines.
- **Golden tests** pin the record format and prove prototype records (format 1) still load.
- **End to end in CI** (`test/e2e/`, the "End-to-end" workflow): machine A, an Arch container, gets a
  few packages, changed and added system files, a service, two users with home files and an extra
  path; it backs up to a restic repository on a shared volume and passes `verify --deep`. Machine B,
  a fresh container with the same users (other user IDs), runs `init --connect` and `restore
  --defaults --yes`. Then packages, files with their modes and owners, the service, groups, shells
  and home files must match A, and B's record must match A's except for differences the test lists
  with their reasons. It also checks the safety rails for real: `--defaults` without `--yes` and
  `--plan` change nothing, `/etc/fstab` is never copied, the restoring flag is there from
  `--connect` until a clean restore, and a backup to an unmounted volume or a different repository
  writes nothing. The prototype never tested a full restore; this does, on every change.
  A also leaves a data folder in `/var/lib` that no package owns: the backup suggests it in the
  inbox, `carryon why` explains it, and once `carryon add` accepts it the next snapshot holds it.
  A user unit with lingering comes back on B, enabled in the user's own systemd.
- **Restoring as you go** runs on machine C, a fresh container named `laptop` with alice and carol
  instead of alice and bob: `init --connect --restore as-you-go --machine-name laptop`, then
  installing an app whose bundle has saved settings prints the hint once (not on a reinstall) and
  adds a `restore` inbox entry. `carryon-restore@` refuses an unknown bundle and applies the offer,
  `restore --bundle --user bob=carol` gives bob's settings to carol, nothing outside the bundle
  comes back, and C's backups go under its own name without touching A's.
- **systemd and the hook** are tested in a container booted with systemd as PID 1 (rather than a VM:
  it boots in seconds on CI runners): the units parse (`systemd-analyze verify`), the timers are on,
  `carryon-backup.service` records a run, `pacman -S` runs the hook, which refreshes the record and
  marks a backup owed, and never fails pacman, even with a broken config; `carryon-settle.service`
  only starts while a backup is owed; and while restoring the hook and the backup service do
  nothing. The machines of the restore test boot with systemd too, so services are captured and
  enabled as on a real machine. The same machine checks the login message after a failed backup,
  the add and ignore units' checks of their instance names, and polkit for users who aren't root, in
  a logind session opened through PAM on a virtual console in the foreground (on CI only, since it
  switches the console): anyone at the console may start a backup, and a user outside `wheel` is
  asked for a password to settle a suggestion. A session without a seat, which polkit doesn't count
  as local, and no session at all are refused.
- **Not covered end to end yet:** AUR packages (an AUR helper would have to be built from the AUR
  in every run), rclone and other remote backends, a graphical session and polkit's password prompt,
  the user agent's notifications (no desktop runs), and real hardware (the containers share the
  runner's).
- **Real machines:** before a release, a maintainer runs the backup and the deep check through the
  real systemd services, as the prototype's notes require.

## Moving over from the prototype

`carryon init --from-sysbackup`:

1. Reads `/etc/sysbackup/sysbackup.conf`, `excludes.txt` and `bundles.local`, and writes the Carryon
   equivalents.
2. Reuses the same restic repository and password, with the old hostname as the machine name, so
   snapshot history continues.
3. Restores from the prototype's snapshots work, because format 1 records can be read.
4. After the first Carryon backup (a snapshot tagged `carryon` under the machine name), running it
   again offers to disable the prototype's timers and hook; until then it prints the command. The hook
   is renamed to `zz-sysbackup.hook.disabled` rather than deleted.

Details: the prototype backed up one user's home (`SYSBACKUP_USER`), so `backup.users` becomes that
user rather than `"*"`. `KEEP` is read option by option; `--keep-*` options Carryon has no setting
for, and `TOOLKIT_MIRROR`, are reported and left out. Only the user's own lines of `excludes.txt` are
carried over (the rest are Carryon defaults). If the prototype recorded that the password was saved,
the recovery kit counts as saved.

## Decisions

| Decision | Choice |
|---|---|
| Name | Carryon |
| Language | Go (a rewrite, using the prototype as reference) |
| Licence | GPL-3.0-or-later |
| First release | Arch-based distributions |
| Storage | restic, any destination the user chooses, with mount guards |
| Desktop integration | Notifications first, tray icon in v0.3 |
| New system data | Ask by default |
| Binary | Static, no cgo, not a PIE |

## Open questions

- The tray library: a small StatusNotifierItem implementation over godbus keeps the binary free of
  cgo; `fyne.io/systray` needs cgo.
- The exact btrfs snapshot procedure for consistent backups, and what to do on other filesystems.
- Whether to import Mackup's app definitions (GPL-3.0) and xdg-ninja's (MIT). **The catalog format is
  settled:** one TOML file per app with an id, a name, how to recognize it (packages, Flatpak ids,
  settings folders), excludes, re-download hints and post-restore notes; see
  [catalog/README.md](../catalog/README.md). Mackup's lists of settings files could fill in
  `recognize.paths` for many apps (its licence matches ours); xdg-ninja's describe where apps
  *should* keep files, which says less about what to back up.
