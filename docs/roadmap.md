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
| **v0.2 Awareness** | Detection and inbox, the user agent with notifications, restore as you go, `why`, `history`, edited-file detection. |
| **v0.3 Comfort** | Tray icon, consistent btrfs backups, battery and metered-network awareness, ntfy and webhook alerts, status-bar JSON. |
| **v0.4+ Reach** | Debian/Ubuntu and Fedora backends, catalog import from Mackup and xdg-ninja, game saves through Ludusavi, file-manager menus. |
| **1.0** | Stable record format and docs; requests to Chaotic-AUR and CachyOS. |

## v0.1 Parity

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

### T11 Backup run ⬜
- **Packages:** `internal/backup`, `internal/state`, `internal/detect` (hook part), `dist/`.
- **Scope:** the backup run (guards, capture, restic backup with the effective excludes, retention,
  weekly prune, run history, `status.json`), the restoring flag, the pending marker and
  `carryon settle`, the pacman hook command, systemd units, sleep inhibitor, nested subvolume
  handling.
- **Legacy:** `cmd_backup`, `cmd_pacman_hook`, `legacy/systemd/`, `legacy/pacman/`.
- **Done when:** a backup to a local repository works in a test, and the hook never returns non-zero.

### T12 Status ⬜
- **Scope:** `carryon status` from `status.json` without root; with root, optionally the snapshot
  list (`--snapshots`, since it opens the repository).
- **Legacy:** `cmd_status`.
- **Done when:** correct output without root and after a reboot.

### T13 Verify ⬜
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

### T15 Restore: apply ⬜
- **Scope:** every phase in order, with failures collected, `*.pre-restore` copies, path adaptation,
  owner remapping, rclone config merge, desktop-session check, `--bundle` for a single bundle.
- **Legacy:** `install_one_by_one`, `apply_plan`, `remap_owner`, `merge_rclone`, `restore_data`,
  `cmd_restore`.

### T16 Terminal UI ✅
- **Package:** `internal/ui`.
- **Scope:** checklist with reasons, item picker with search, menus, confirmations, all with a
  non-interactive fallback (flags, `--defaults`).
- **Legacy:** `checklist`, `item_picker`, `menu`, `confirm`.

### T17 init, recovery kit and migration ⬜
- **Scope:** `carryon init` (destination, password, machine name, users, schedule; creates or opens
  the repository; enables units), `--connect`, the recovery kit, `--from-sysbackup`.
- **Legacy:** `install.sh`.

### T18 Notifications ✅
- **Package:** `internal/notify`.
- **Scope:** `carryon notify UNIT` for `OnFailure=`, a fallback notification into the user's session,
  ntfy and webhook senders.
- **Legacy:** `cmd_notify`, `last_error`.

### T19 Packaging ⬜
- **Scope:** `PKGBUILD`s for `carryon`, `carryon-bin` and `carryon-git` in `packaging/aur/`, a release
  workflow that builds static binaries and a tarball with `dist/`, man page and shell completions.

### T20 End-to-end tests ⬜
- **Scope:** a CI job in an Arch container: set up packages and settings, `carryon init` with a local
  repository, `backup`, then a fresh container, `init --connect`, `restore --defaults`, and compare the
  records. A VM job for systemd units and the hook.

## Later milestones

Broken into tasks when v0.1 is done. In short:

- **v0.2:** detection scans and inbox (`internal/detect`), the user agent with D-Bus notifications and
  actions, polkit rules, restore as you go, `why`, `history`, edited-file detection with `pacman -Qkk`.
- **v0.3:** tray icon, btrfs snapshots for consistent backups, battery and metered-network checks,
  ntfy and webhook alerts, `status --json` for status bars.
- **v0.4+:** apt and dnf backends, catalog import, Ludusavi, Dolphin and Nautilus menus.
