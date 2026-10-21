# Legacy prototype (`sysbackup`)

This folder holds the Python prototype that Carryon grows out of. It ran on one CachyOS desktop for
months, backing up to a restic repository through rclone. Carryon is a rewrite in Go. **This code is
the reference for that rewrite:** when porting a feature, read the matching function here first,
because most of its regular expressions, special cases and orderings exist for a reason.

Nothing here is built, installed or tested by this repository. It will be deleted once Carryon reaches
feature parity (see [docs/roadmap.md](../docs/roadmap.md)).

## What changed from the original

The prototype was written for its author's machine. Before it was added here:

- The default repository and Toolkit mirror (both pointed at the author's rclone remote) were blanked
  in `sysbackup.py` and `sysbackup.conf`.
- Mentions of a specific password manager became "your password manager".
- Personal excludes and re-download hints (a manual AUR build folder, a cloud-drive mount, one app's
  log files) were removed from `excludes.txt` and `sysbackup.py`.
- The author's own bundle rules were replaced by examples in `bundles.local.example`.
- `install.sh` messages no longer name a specific cloud provider.

Comments that explain *why* the prototype talks to rclone the way it does still mention iCloud Drive,
because that's the backend it was built and tested against.

## Files

| File | What it is |
|---|---|
| `sysbackup.py` | The whole tool: capture, backup, verify, restore, bundles, status (stdlib-only Python, ~2,700 lines). |
| `install.sh` | Installer: copies the tool to `/usr/local`, creates config, the password and the repository, enables the timers. |
| `rclone-as-user` | Shim that lets root's restic use the user's rclone login (`-o rclone.program=...`). |
| `sysbackup.conf` | Config template (`KEY=VALUE`). |
| `excludes.txt` | Default restic excludes (caches, Steam games, toolchains, re-downloadable app files). |
| `bundles.local.example` | The bundle-rule format: `title`, `when`, `put`, `drop`. |
| `pacman/zz-sysbackup.hook` | Post-transaction hook: refresh the record, schedule a backup an hour after the last change. |
| `systemd/` | Daily backup, weekly deep verify, failure notification units. |

## Where each part goes in Carryon

| Prototype function(s) | Carryon package |
|---|---|
| `DEFAULTS`, `load_conf` | `internal/config` |
| `paint`, `_stderr_is_journal`, `_emit`, `log`, `warn`, `die` | `internal/ui` |
| `user_cmd`, `sh`, `call`, `have` | `internal/sysexec` |
| `restic_env`, `restic_cmd`, `restic`, `restic_error`, `cloud_problem`, `restic_snapshots` | `internal/engine` |
| `online`, the rclone pre-check | `internal/dest` (destination guards) |
| `hw_ids`, `devices_for_module`, `device_name`, `pretty_id`, `detect_bootloader`, `target_facts`, `machine_fingerprint`, `usb_present`, `fingerprint_readers`, `app_devices` | `internal/facts` |
| `when_applies`, `_when_one`, `read_local_rules`, `glob_hit` | `internal/rules` |
| `capture_packages`, `parse_gpg_keys`, `capture_repos` | `internal/pkgmgr/pacman` |
| `capture_files`, `unit_props`, `capture_services`, `unit_needs`, `cmdline_config`, `capture_kernel`, `capture_account`, `capture_tools`, `load_excludes`, `excluded`, `sys_path_item`, `capture_paths`, `capture` | `internal/capture` |
| `BUILTIN`, `ORDER`, `rank`, `early_rules`, `FALLBACK`, `stem_of`, `stem_matches`, `firmware_when`, `assign_bundles`, `app_key`, `autostart_owner` | `internal/bundle` |
| `item`, `key`, `describe`, `read_items`, `load_manifest`, `fingerprint`, `diff_items`, `change_lines`, `summary_md`, `publish` | `internal/record` |
| `REDOWNLOAD_HINTS`, `steam_games`, `redownload_list`, `disk_usage`, `human` | `internal/catalog` |
| `cmd_backup` | `internal/backup` |
| `cmd_pacman_hook` | `internal/detect` |
| `cmd_notify` | `internal/notify`; `internal/agent` shows it with buttons |
| `cmd_status`, stamps | `internal/state` |
| `Report`, `aur_info`, `repo_reachable`, `covered`, `cmd_verify`, `verify_cloud`, `finish` | `internal/verify` |
| `checklist`, `item_picker`, `menu`, `confirm` | `internal/ui` (terminal UI) |
| `get_source`, `pick`, `adapt*`, `file_state`, `boot_risk`, `make_plan`, `show_plan`, `install_one_by_one`, `apply_plan`, `remap_owner`, `merge_rclone`, `restore_data`, `cmd_restore` | `internal/restore` |
| `install.sh` | `carryon init` plus the package's install files |
| `toolkit_digest`, `mirror_toolkit`, `verify_toolkit` | Dropped: replaced by the recovery kit and settings stored inside each snapshot ([design](../docs/design.md#getting-back-in)) |

## Lessons the prototype learned the hard way

- **systemd services have no `HOME`.** restic then can't find its cache, and every automatic run
  failed until `HOME` and `RESTIC_CACHE_DIR` were set explicitly. Always test through the real
  service, not only with `sudo`.
- **Root can't read a user's FUSE mount** (no `allow_other`). The prototype's installer copied itself
  to `/tmp` before elevating for this reason.
- **Never walk or `du` a cloud mount.** It crawls the whole remote drive.
- **Cloud backends through rclone are slow** (5 to 40 seconds per request on iCloud Drive). Keep round
  trips few.
- **A wrapper around rclone must not die on a missing config** (`set -e` turned that into a cryptic
  "error talking to rclone").
- **Only add syslog level prefixes (`<3>`) when stderr really is the journal**
  (`JOURNAL_STREAM` matches stderr's device and inode).
- **Restore stays pick-and-choose.** Two machines are never identical; hardware-specific bundles
  start unticked where the hardware is missing.
- **Files with disk UUIDs are shown as a diff, never copied** (fstab, crypttab, bootloader configs),
  and an initramfs config is never copied if it lacks storage hooks the target needs.

## Problems found in the prototype

A survey of the running prototype found these. Carryon's design addresses each one.

- `status` without root printed "never" for every timestamp (the state folder is root-only).
- After a reboot, `status` said "hasn't run yet", because systemd resets a unit's exit timestamp.
- Error details were read back from the journal, which some distributions keep for only a day.
- The "back up an hour after the last install" timer was transient and lost on reboot.
- The system record's git history got empty "0 change(s)" commits.
- Only `backup=` config files were detected as edited; other edited package files were missed.
- Capture matched excludes with Python's `fnmatch`, restic with its own rules; the two can disagree.
- `--one-file-system` silently skipped nested subvolumes and mounts inside backed-up folders.
- Retention was grouped by hostname, so two machines with the same name would share one group.
- A real restore was never run end to end.
