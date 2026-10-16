# Carryon design

Carryon backs up what makes a Linux machine yours: the packages you chose, the system settings you
changed, your services and your home folder. From that backup it rebuilds a fresh install, all at once
or one app at a time, and it notices new things to back up by itself.

This document is the plan for the Go rewrite of the [prototype](../legacy/README.md). Where the
prototype already made a decision that worked, Carryon keeps it; those parts say so. Sections marked
**New** didn't exist in the prototype. The task breakdown is in [roadmap.md](roadmap.md).

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
  monthly (`restic forget --host <machine name> --group-by host`). Pruning runs every
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
| `carryon-agent.service` (user unit) | **New:** the user's notifications, and later the tray icon. |
| `/usr/share/libalpm/hooks/carryon.hook` | After every install, upgrade or removal: quick capture, write the pending marker, print hints. Never fails or blocks the transaction. |

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
- **Failure alerts:** `carryon-notify@.service` runs `carryon notify UNIT`, which sends the unit's last
  error lines through every channel in `[notify]`. **Generalized:** the prototype alerted one user;
  Carryon alerts every user with an open graphical session who is in `backup.users` and hasn't
  switched desktop notifications off in their own settings, running `notify-send` (or `busctl` without
  libnotify) as that user. **Generalized:** the prototype's "iCloud signed you out" check works for any
  `rclone:` remote and names it with the reconnect command, without probing a fixed website first.
  **New:** ntfy and webhook alerts, with passwords, keys and `user:password@` hidden before anything
  leaves the machine, and `carryon notify --test` to try every channel. With a missing or broken
  config, it still alerts with the default settings.
- **Paused while restoring:** a `restoring` flag stops automatic backups on a fresh install, so a
  half-restored system never becomes the newest snapshot. Kept from the prototype. **Change:** the
  pacman hook does nothing at all while it is there; the prototype still refreshed the record.
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
2. **What it looks for:** folders in `/var/lib`, `/var/opt` and `/srv` that hold data no package
   owns (for example `/var/lib/jellyfin` filling up after Jellyfin was installed). Known system
   folders (`pacman`, `systemd`, `dbus`...) are ignored.
3. **Who owns it:** the package that created the folder, matched by name, file owner or the service
   user that owns it.
4. **Suggestions:** each finding becomes an inbox entry with its size and probable app. Accepting it
   adds the folder to `extra_paths` and to that app's bundle. Big folders (over 5 GB by default) are
   marked so the user can decide.
5. **Telling the user:** a desktop notification with **Back it up** and **Ignore** buttons, a line in
   pacman's output in a terminal, `carryon inbox`, and a count in `carryon status`.

`detect.new_paths = "ask" | "auto" | "off"` in the config controls this (default `ask`).

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
  their settings at logout. **Generalized** from a Plasma-only check to any graphical session.
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

### As you go

**New.** On a fresh machine connected to an existing repository, nothing has to be restored up front.
Carryon keeps the source machine's record. Whenever the pacman hook sees a package installed whose
bundle has settings, files or services in the backup:

- in a terminal, pacman's output says `carryon: saved settings for obs-studio (9 Oct) — restore with:
  carryon restore --bundle app-obs-studio`;
- on a desktop, the agent shows **Restore** and **Not now**. **Restore** starts
  `carryon-restore@<bundle>.service` through polkit, which applies only that bundle's non-package
  items, after warning if the app is running.

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

[notify]
desktop = true
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
| `carryon restore` | Pick and apply. `--plan` only shows the plan; `--bundle ID` restores one bundle. |
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

- `--json` on read commands, so scripts, status bars and agents can use them.
- **Terminal UI** (Bubble Tea) for `init`, the restore checklist and the inbox. Every prompt has a
  flag, so everything also works without a terminal.
- **Notifications** come from the user agent over D-Bus, with action buttons. When no agent runs, the
  root service falls back to a plain notification in the user's session.
- **Tray icon** (v0.3), a StatusNotifierItem over D-Bus. KDE and most bars show it; GNOME needs the
  AppIndicator extension.
- **No desktop:** hints in pacman's output, `carryon status`, an optional one-line login message
  when something needs attention, and alerts through ntfy or a webhook.

## Who runs what

| Part | Runs as | Reads | Writes |
|---|---|---|---|
| Backup, verify, settle, restore | root (systemd or `sudo`) | everything backed up | record, state, repository |
| pacman hook | root, inside pacman | package lists | pending marker, inbox, hints on stdout |
| Agent | the user | `status.json`, inbox | nothing outside the user's home |
| Actions from the agent (Back up now, Restore bundle) | root, via polkit-allowed `systemctl start` of specific units | | |

- `/etc/carryon/repository-password` and `/var/lib/carryon/record/` are root-only.
- `status.json` and the inbox are world-readable and hold no secrets.
- A user's own settings can only add or remove paths in their own home.
- The agent never runs as root.

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
  the password; backups pause until `sudo carryon restore` finishes.
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
dist/                 systemd units, pacman hook, polkit rules, completions
packaging/aur/        PKGBUILDs
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
| `/usr/lib/systemd/system/carryon-*.{service,timer}` | system units |
| `/usr/lib/systemd/user/carryon-agent.service` | user agent |
| `/usr/share/libalpm/hooks/carryon.hook` | pacman hook |
| `/usr/share/polkit-1/rules.d/carryon.rules` | who may start backups and bundle restores |
| `/usr/share/carryon/` | default excludes, catalog |
| `/etc/carryon/` | config, excludes, bundle rules, password (created by `init`) |
| `/var/lib/carryon/` | record, history, inbox, flags |
| `/var/cache/carryon/restic/` | restic cache |

## Testing

- **Unit tests** use fakes: a fake package manager, a fake program runner, and temporary folders via
  `CARRYON_CONF_DIR`, `CARRYON_STATE_DIR` and `CARRYON_ROOT` (a fake filesystem root for capture).
- **Golden tests** pin the record format and prove prototype records (format 1) still load.
- **End to end in CI:** in an Arch container, install a few packages and settings, back up to a
  local restic repository, then restore into a fresh container and compare the records. The prototype
  never tested a full restore; this does, on every change.
- **systemd and the hook** are tested in a VM job.
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
