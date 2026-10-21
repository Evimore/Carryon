# Roadmap

How Carryon gets from the prototype to a public release. Each task below is sized for one pull
request, so a person or an agent can pick it up on its own. Read [design.md](design.md) first, then
the legacy functions the task names in [legacy/sysbackup.py](../legacy/sysbackup.py).

**Status key:** ✅ done · 🔨 in progress · ⬜ not started

## Milestones

| Milestone | Goal |
|---|---|
| **v0.0 Foundation** ✅ | Design, legacy reference, Go skeleton with the record model, when-rules, bundle rules and config, CI. |
| **v0.1 Parity** | Everything the prototype does, generalized: any destination, any Arch-based distro, several users. AUR package. End-to-end restore test in CI. |
| **v0.2 Awareness** | Detection and inbox, the user agent with notifications and actions, restore as you go, `why`, a login message. |
| **v0.3 Comfort** | Tray icon, consistent btrfs backups, battery and metered-network awareness. |
| **v0.4+ Reach** | Debian/Ubuntu and Fedora backends, catalog import from Mackup and xdg-ninja, game saves through Ludusavi, file-manager menus. |
| **1.0** | Stable record format and docs; requests to Chaotic-AUR and CachyOS. |

## v0.1 Parity

All tasks are done. What's left is the release itself: a tag, then the upload to the AUR, both
maintainer steps in [packaging.md](packaging.md).

Tasks in the same row of the dependency list can be done in parallel.

```
T01 sysexec ─┬─► T05 facts ──────────┐
T02 config ──┤   T06 pacman ─────────┼─► T07 capture ─► T08 bundle ─► T09 record store ─┐
T03 engine ──┤   T10 catalog ────────┘                                                  ├─► T11 backup ─► T12 status
T04 dest ────┘                                                                          │   T13 verify
                                                                                        └─► T14 restore plan ─► T15 restore apply
T16 terminal UI (any time) ─► T17 init + recovery kit + migration
T18 notify ─► T19 packaging ─► T20 end-to-end tests
```

### T01 Program runner ✅
- **Package:** `internal/sysexec`.
- **Scope:** run a program and capture its output (with a timeout), run it attached to the terminal,
  run it as another user with that user's `HOME`, `XDG_RUNTIME_DIR` and session bus (like
  `runuser`), check whether a program exists. An interface with a fake for tests that records calls
  and returns canned output.
- **Legacy:** `user_cmd`, `sh`, `call`, `have`.
- **Done when:** other packages can be tested without running real programs.

### T02 Config, complete ✅
- **Package:** `internal/config` (the skeleton already loads and validates the basics).
- **Scope:** writing a commented config file (for `init`), `config check|get|set` support,
  `config_version` migration, per-user config (paths limited to the user's home), the effective
  exclude list (defaults minus switched-off ones, plus the user's, the catalog's and automatic ones).
- **Legacy:** `load_conf`, `load_excludes`; `install.sh` for how excludes are merged.
- **Done when:** a config round-trips with comments, and invalid values give clear errors.

### T03 restic engine ✅
- **Package:** `internal/engine`.
- **Scope:** build restic commands and environment (`HOME`, `RESTIC_CACHE_DIR`, password file or
  command, `--retry-lock`), run quietly or with live output while keeping the last lines, parse
  `snapshots --json`, `ls --json` and backup summaries, classify errors into cause plus fix, the
  hidden `carryon rclone-shim` command that runs rclone as `rclone_user`.
- **Legacy:** `restic_env`, `restic_cmd`, `restic`, `restic_error`, `restic_snapshots`,
  `rclone-as-user`.
- **Done when:** tests run against a real local restic repository (CI installs restic).

### T04 Destination guards ✅
- **Package:** `internal/dest`.
- **Scope:** everything in [design: destinations and mount guards](design.md#destinations-and-mount-guards):
  filesystem UUID and repository ID checks, FUSE and network mount detection, reachability checks per
  backend (replacing the prototype's iCloud probe), free-space check.
- **Legacy:** `online`, `cloud_problem`, `mountpoints`.
- **Done when:** an unmounted destination is never written to, proven by a test with a fake mount
  table.

### T05 Machine facts ✅
- **Package:** `internal/facts`.
- **Scope:** machine fingerprint, chassis, PCI and USB IDs, Bluetooth adapters, kernel modules (from
  modaliases), device names, root filesystem, bootloader, fingerprint readers, apps' USB devices. All
  readers take a root path so tests can use a fake `/sys` and `/proc`.
- **Legacy:** `hw_ids`, `devices_for_module`, `device_name`, `detect_bootloader`, `target_facts`,
  `machine_fingerprint`, `usb_present`, `fingerprint_readers`, `app_devices`.
- **Done when:** `rules.Facts` is filled from a fake system tree in tests.

### T06 pacman backend ✅
- **Package:** `internal/pkgmgr` (interface) and `internal/pkgmgr/pacman`.
- **Scope:** explicit and foreign packages with their repositories, optional-only dependencies, file
  owners in batches, `backup=` files that were modified, edited package files (`-Qkk`, nightly only),
  all owned files, extra repositories and their sections, hand-added signing keys, sync database
  lookups, installing packages for restore. Standard repositories per distribution (Arch, CachyOS,
  EndeavourOS, Manjaro, Garuda) plus `pacman.extra_standard_repos`.
- **Legacy:** `capture_packages`, `parse_gpg_keys`, `capture_repos`, `file_owners`, the `pacman` calls
  in `capture_files` and `make_plan`.
- **Done when:** parsing is tested with recorded pacman output.

### T07 Capture ✅
- **Package:** `internal/capture`.
- **Scope:** files, services, user units for every included user, unit needs, kernel parameters,
  users' groups and shells, language tools, backed-up paths, and the capture run that assembles the
  record. Uses T05 and T06 through interfaces.
- **Legacy:** `capture_files`, `unit_props`, `capture_services`, `unit_needs`, `cmdline_config`,
  `capture_kernel`, `capture_account`, `capture_tools`, `sys_path_item`, `capture_paths`, `capture`.
- **Done when:** a capture of a fake system tree produces the expected items.

### T08 Bundles ✅
- **Package:** `internal/bundle`.
- **Scope:** the built-in bundles and their order, well-known rules, app stems, edited-config
  groups, settings-folder and autostart matching, extra-path matching, hardware bundles, fallbacks,
  unit needs following their unit, titles and when-rules. `bundles.conf` parsing already exists in
  `internal/rules`.
- **Legacy:** `BUILTIN`, `ORDER`, `rank`, `early_rules`, `FALLBACK`, `stem_of`, `stem_matches`,
  `firmware_when`, `assign_bundles`, `app_key`, `autostart_owner`.
- **Done when:** golden tests over a realistic item list give the same bundles as the prototype.

### T09 Record store ✅
- **Package:** `internal/record` (types, reading, writing and diffing already exist).
- **Scope:** publishing a staged record atomically, the git history (commit only when items change),
  `SUMMARY.md`, change lines for commit messages.
- **Legacy:** `summary_md`, `publish`, `change_lines`.
- **Done when:** publishing twice without changes makes no commit.

### T10 Catalog ✅
- **Package:** `internal/catalog`, data in `catalog/`.
- **Scope:** the catalog file format; default excludes; re-download hints (Steam games from their
  manifests, toolchains, app files) and the `not-backed-up.md` list; post-restore notes; an exclude
  matcher with restic's pattern rules; never walking into mounts.
- **Legacy:** `REDOWNLOAD_HINTS`, `steam_games`, `redownload_list`, `disk_usage`, `excluded`,
  `legacy/excludes.txt`.
- **Done when:** the matcher agrees with restic on a table of patterns and paths.

### T11 Backup run ✅
- **Packages:** `internal/backup`, `internal/state`, `internal/detect` (hook part), `dist/`.
- **Scope:** the backup run (guards, capture, restic backup with the effective excludes, retention,
  weekly prune, run history, `status.json`), the restoring flag, the pending marker and
  `carryon settle`, the pacman hook command, systemd units, sleep inhibitor, nested subvolume
  handling.
- **Legacy:** `cmd_backup`, `cmd_pacman_hook`, `legacy/systemd/`, `legacy/pacman/`.
- **Done when:** a backup to a local repository works in a test, and the hook never returns non-zero.

### T12 Status ✅
- **Scope:** `carryon status` from `status.json` without root; with root, optionally the snapshot
  list (`--snapshots`, since it opens the repository).
- **Legacy:** `cmd_status`.
- **Done when:** correct output without root and after a reboot.

### T13 Verify ✅
- **Package:** `internal/verify`.
- **Scope:** record checks (complete, fresh, drift), reinstallability (repository packages, AUR via
  its RPC API, extra repositories reachable, keys saved), coverage (units' programs, DKMS sources,
  kernel parameters, mounts, modules, leftover enable links), repository checks (opens, newest
  snapshot age, recovery kit confirmed) and `--deep` (contents of the latest snapshot, `restic check
  --read-data-subset`, a test restore).
- **Legacy:** `Report`, `aur_info`, `repo_reachable`, `covered`, `cmd_verify`, `verify_cloud`,
  `finish`.

### T14 Restore: source and plan ✅
- **Package:** `internal/restore`.
- **Scope:** choosing machine and snapshot, fetching its record, default ticks from when-rules,
  remembered choices, the plan (only what differs), diffs, boot-risk and review-only files, user
  mapping, `--plan`.
- **Legacy:** `get_source`, `pick`, `adapt`, `file_state`, `boot_risk`, `make_plan`, `show_plan`.
- **Done when:** a plan for a format-1 record from the prototype matches the prototype's dry run.

### T15 Restore: apply ✅
- **Package:** `internal/restore` (`Applier`), `carryon restore`.
- **Scope:** every phase in order, with failures collected, `*.pre-restore` copies, path adaptation,
  owner remapping, rclone config merge, desktop-session check, `--bundle` for a single bundle.
- **Legacy:** `install_one_by_one`, `apply_plan`, `remap_owner`, `merge_rclone`, `restore_data`,
  `cmd_restore`.

### T16 Terminal UI ✅
- **Package:** `internal/ui`.
- **Scope:** checklist with reasons, item picker with search, menus, confirmations, all with a
  non-interactive fallback (flags, `--defaults`).
- **Legacy:** `checklist`, `item_picker`, `menu`, `confirm`.

### T17 init, recovery kit and migration ✅
- **Scope:** `carryon init` (destination, password, machine name, users, schedule; creates or opens
  the repository; enables units), `--connect`, the recovery kit, `--from-sysbackup`.
- **Legacy:** `install.sh`.

### T18 Notifications ✅
- **Package:** `internal/notify`.
- **Scope:** `carryon notify UNIT` for `OnFailure=`, a fallback notification into the user's session,
  ntfy and webhook senders.
- **Legacy:** `cmd_notify`, `last_error`.

### T19 Packaging ✅
- **Scope:** `PKGBUILD`s for `carryon`, `carryon-bin` and `carryon-git` in `packaging/aur/`, a release
  workflow that builds static binaries and a tarball with `dist/`, man page and shell completions.
- **Done:** the `Makefile` (`make`, `make install`, `make dist`, `make check-install`), the man page
  and completions generated by `internal/tools/gendocs`, the "Release" workflow (a dry run on pull
  requests) and the "Package" workflow, which builds `carryon-git` with `makepkg`, runs namcap and
  installs it in an Arch container. See [packaging.md](packaging.md).

### T20 End-to-end tests ✅
- **Scope:** a CI job in an Arch container: set up packages and settings, `carryon init` with a local
  repository, `backup`, then a fresh container, `init --connect`, `restore --defaults`, and compare the
  records. A VM job for systemd units and the hook.

## v0.2 Awareness

Carryon notices what it doesn't back up yet, tells the user wherever they are (desktop, terminal or
login), explains what it does with a path, and gives saved settings back as apps get installed. Most
of this is **New** in [design.md](design.md), so there is little prototype code to port: read the
design sections each task names.

Already done in v0.1: `carryon history` (T09), edited-file detection with `pacman -Qkk` (T06, T07)
and restoring one bundle by hand with `carryon restore --bundle` (T15).

```
T21 inbox ─┬─► T22 detection scans ────────────────────────────┐
           ├─► T23 restore as you go ─► T26 polkit and units ───┤
           └─► T27 user agent ──────────────────────────────────┼─► T28 end-to-end tests
T24 why ────────────────────────────────────────────────────────┤
T25 login message ──────────────────────────────────────────────┘
```

**Names the tasks share**, so they can be built in parallel:

- The inbox is `/var/lib/carryon/inbox.json` (T21).
- The agent's buttons start `carryon-backup.service` (**Back up now**),
  `carryon-restore@<bundle>.service` (**Restore**), and `carryon-add@<entry>.service` and
  `carryon-ignore@<entry>.service` (a suggestion's **Back it up** and **Ignore**). An instance name
  is a bundle ID or an inbox entry ID, never a path (T23 adds the restore unit, T26 the others).

### T21 Inbox ✅
- **Packages:** `internal/state` (the inbox file), `internal/cli` (`inbox`, `ignore`, `add`).
- **Scope:** the inbox file, world-readable and free of secrets, shaped `{"entries": [...]}` as
  `carryon status` already counts it. Each entry has a short, stable ID that is safe as a systemd
  instance name; its kind (`path`: back up this folder? `restore`: restore this app's saved
  settings?); the path or bundle; its size, probable app and bundle, and whether it's large; when
  and by what it was found. Entries are added, replaced and removed under a lock, since the hook,
  the nightly backup and the commands all write the file; a damaged file is set aside, not fatal.
  Ignored paths go in `detect.ignore` in `config.toml`, so they travel with `/etc/carryon` in the
  backup. `carryon inbox` lists the entries (`--json`) and, in a terminal, goes through them with
  the terminal UI: back it up, ignore, or later. `carryon add` and `carryon ignore` also take an
  entry ID, and remove the entry they settle. Accepting a folder adds it to `backup.extra_paths`
  and, when its app's bundle wouldn't find it by name, a `put` line to `bundles.conf`.
- **Legacy:** none; the prototype had no inbox.
- **Done when:** writers running at the same time don't lose entries, `add` and `ignore` settle an
  entry by path or ID, `carryon status` counts it, and all of it works without a terminal.

### T22 Detection scans ✅
- **Package:** `internal/detect`.
- **Scope:** [design: noticing new things](design.md#noticing-new-things).
  - **What:** folders in `/var/lib`, `/var/opt` and `/srv` holding data no package owns. Left out:
    known system folders (shipped as data, not code, so the list can grow), folders already backed
    up or left out by the excludes and the catalog, ignored paths, and other filesystems.
  - **Whose:** the probable app is a package matched by name (`/var/lib/jellyfin` ↔
    `jellyfin-server`), the package owning the folder itself, or the package of the unit whose
    `User=`, `StateDirectory=` or `DynamicUser` folder it is.
  - **How big:** measured without walking into mounts, marked large above `detect.large_path_gb`.
  - **What then:** `detect.new_paths = "ask"` adds an inbox entry; `auto` adds the folder as if
    accepted and notes it in the run history; `off` does nothing.
  - **When:** the nightly backup scans everything. The pacman hook checks only the folders of the
    packages it was given and stays within its time limit (an app's data usually appears later, when
    it first runs, and the nightly scan finds it then). New entries print one line in pacman's
    output. A path unit on `/var/lib/flatpak/app` refreshes the record when system-wide Flatpak apps
    change.
- **Legacy:** none. The bundle rule that puts `/var/lib/foo` with package `foo` is already in
  `internal/bundle` (`extraFolders`).
- **Done when:** a scan of a fake system tree suggests exactly its unowned data folders, each with
  the right app, and the hook still never fails or blocks pacman.

### T23 Restore as you go ✅
- **Packages:** `internal/restore`, `internal/setup`, `internal/detect` (hook part), `dist/`.
- **Scope:** [design: as you go](design.md#as-you-go). Keep the source machine's record and its
  snapshot ID in the state folder (root only, like the record), from `init --connect` and after each
  restore. When the pacman hook sees a package installed whose bundle in that record has settings,
  files or services that differ from this machine, it prints `carryon: saved settings for obs-studio
  (9 Oct) — restore with: carryon restore --bundle app-obs-studio` and adds a `restore` inbox entry,
  once per bundle: a restored or ignored bundle isn't offered again. `carryon restore --bundle`
  uses the kept record and snapshot by default, so it needs no choice of machine.
  `carryon-restore@.service` applies one bundle's items other than packages, without a terminal,
  and refuses an instance name that isn't a bundle of the kept record.
  **Restore never applies anything before the user has seen the plan**, so the offer says what
  would change (places and counts, never file contents, since the inbox is world-readable), and the
  unit applies only that: if the plan changed since the offer, it refuses and the offer is renewed.
  Settle the questions [design: open questions](design.md#open-questions) lists for this task
  (the restoring flag, retention, home files during a session) and write the answers into the
  design.
- **Legacy:** none.
- **Done when:** with a kept record, installing a package offers its bundle once, `restore --bundle`
  applies only that bundle, the unit refuses a plan that changed since the offer, and the hook still
  never fails or blocks pacman.
- **Done:** everything above, with the design's three proposals: `init --connect` asks "everything
  now or as you go" (`--restore`, now without a terminal); the snapshot is tagged `carryon-source`
  and kept out of `forget` when the machine keeps the source's name, and `restore --stop-offers`
  ends that; an app's bundle waits for the app to close instead of the desktop session. Offers are
  made for apps' bundles only, and "installed" is a package the record didn't hold before the hook
  ran, so upgrades offer nothing and no second hook file is needed. `carryon inbox` shows an
  offer's changes.

### T24 `carryon why` ✅
- **Packages:** `internal/bundle`, a new `internal/why`, `internal/cli`.
- **Scope:** `carryon why PATH` says in plain words whether and how a path is backed up:
  - in which backed-up folder (`/etc`, `/root`, `/srv`, `backup.extra_paths`, a user's home);
  - left out by which exclude pattern, and where that comes from (the defaults, a catalog app,
    `excludes.txt`, a mount or the repository's own folder), including a `!` pattern taking it back;
  - left out as another filesystem or mount, and how to take it in;
  - the record item that holds it, its bundle, **the rule that put it there**, the bundle's
    when-rule, and whether it would start ticked here;
  - a package file that is unchanged and comes back with its package, or data that downloads itself
    again (the not-backed-up hints); once T21 is in, an ignored path or one waiting in the inbox.

  `internal/bundle` learns to say which step put each item in its bundle (a `put` line, an app
  named after a unit or package, a well-known place, a settings folder named like a package...).
  The record format doesn't change: `why` groups the record's items again to get the reasons.
  Without root the record can't be read, so `why` says what the settings tell and suggests `sudo`.
  `--json`.
- **Legacy:** none; it reuses `excluded` and the bundle rules already ported.
- **Done when:** a table test covers each answer above, including a default exclude taken back by
  `!` and an item moved by a `put` rule.
- **Done:** every answer above. It also names a
  folder marked as a cache (`CACHEDIR.TAG`, which `--exclude-caches` leaves out), a nested btrfs
  subvolume, a user `backup.users` leaves out, and an item that today's `bundles.conf` would group
  differently (the next capture moves it).

### T25 Login message ✅
- **Packages:** `internal/state`, `internal/status`, `internal/config`.
- **Scope:** the "no desktop" path in [design: interfaces](design.md#interfaces). When something
  needs attention, by the same rules as `carryon status`, one line goes in
  `/run/motd.d/carryon.motd`, which `pam_motd` shows at console and SSH logins: `Carryon: the last
  backup failed 2 days ago. See: carryon status`. It's written whenever `status.json` is and removed
  when all is well, so no program runs at login. On by default; `notify.login_message = false`
  turns it off. The folder can be moved for tests, like the others in `internal/paths`.
- **Legacy:** none.
- **Done when:** a test shows status changes writing and removing the line.

### T26 polkit rules and action units ⬜
- **Packages:** `dist/` (polkit rules and units), `internal/cli`, `packaging/`.
- **Scope:** the `carryon-add@` and `carryon-ignore@` units (names above), and
  `/usr/share/polkit-1/rules.d/carryon.rules`, which lets users start every unit behind the agent's
  buttons with `systemctl start`, without root. Proposed default, to confirm in the pull request:
  anyone with a local, active session may start a backup; restoring a bundle or settling a
  suggestion changes what root writes or backs up, so it needs an administrator (`wheel`), and
  others get polkit's password prompt (`auth_admin_keep`). Each unit checks its instance name
  against the inbox and refuses anything else, so a user can never pass a path or an option to
  root. The install layout (`packaging/layout.txt`, the `Makefile`,
  [design: install layout](design.md#install-layout)) gets the rules and units.
- **Legacy:** none.
- **Done when:** `systemd-analyze verify` passes on the new units in CI, an unknown or path-like
  instance name is refused, and the layout check passes.

### T27 User agent ✅
- **Packages:** a new `internal/agent`, `carryon agent`, `dist/` (the user unit).
- **Scope:** `carryon-agent.service`, a user unit `carryon init` enables for every user, runs
  `carryon agent`, which refuses to run as root. It watches `status.json` and the inbox and shows
  desktop notifications over D-Bus (`org.freedesktop.Notifications`) with buttons:
  - a failed backup or check: **Back up now**, or the fix;
  - a suggestion: **Back it up** and **Ignore**;
  - a restore offer: what would change, with **Restore** and **Not now**, warning first if the app
    is running.

  The buttons start the units T26 adds (`systemctl start`, through polkit); until T26 is in, tests
  use fakes. The agent follows the user's own notification settings, shows each event once (across
  restarts too), and writes nothing outside the user's home. While a user's agent runs, `carryon
  notify` leaves that user's notification to it, so nobody gets two. Adds `godbus/dbus/v5` (pure Go,
  named in the design).
- **Legacy:** `cmd_notify`, the plain notification the agent improves on.
- **Done when:** against a fake notification service on a private bus, each event shows once with
  the right buttons, and each button starts the right unit.
- **Done:** all of it, on a private bus in the tests. The agent runs with the desktop session
  (`graphical-session.target`), polls the files every 5 seconds instead of watching them, and also
  says when the newest backup is too old. A restore offer lists what it would change (T23's
  `state.Entry.Changes`), and a renewed offer, whose plan changed, shows again. Problems carry a `kind` in
  `carryon status --json`, which the agent reads. `carryon notify` leaves a user's desktop
  notification to their agent only for failures in the run history, which the agent can see.

### T28 End-to-end tests for v0.2 ⬜
- **Scope:** extend `test/e2e/`:
  - a data folder in `/var/lib` shows up in the inbox, `carryon add` of its ID makes the next backup
    hold it, and `carryon why` explains it;
  - on machine B, restoring as you go: installing a package with saved settings prints the hint
    once, and `restore --bundle` brings back only that bundle;
  - a user who isn't root starts `carryon-backup.service` through polkit;
  - the login message appears when a backup fails and goes away after the next good one.

  Where it's cheap, also close gaps T20 listed: user units, `--user` mapping, a different machine
  name.
- **Done when:** the "End-to-end" workflow runs these on every pull request and stays under 15
  minutes.

## Later milestones

Broken into tasks when v0.2 is done. In short:

- **v0.3:** tray icon, btrfs snapshots for consistent backups, battery and metered-network checks.
  ntfy and webhook alerts and `status --json` for status bars were planned here but came with v0.1
  (T18, T12).
- **v0.4+:** apt and dnf backends, catalog import, Ludusavi, Dolphin and Nautilus menus.
