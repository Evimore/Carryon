# End-to-end tests

These tests run Carryon on whole Arch Linux machines: containers from `archlinux:latest`, booted
with systemd as PID 1. Run them with `test/e2e/run.sh` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).
CI runs them in the "End-to-end" workflow.

| File | Runs | What it does |
|---|---|---|
| `run.sh` | on the host | builds Carryon, boots the machines, runs the scripts below in them |
| `machine-a.sh` | in machine A, "desktop" | sets up packages, files, a service, users and home files; `init`, `backup`, `verify --deep`, `status --json`, `history`; a backup to an unmounted volume and to a different repository writes nothing |
| `machine-b.sh` | in machine B, "laptop" | a fresh machine with the same users: `init --connect`, `restore --defaults` (changes nothing), `restore --plan` (changes nothing), `restore --defaults --yes`, then compares B with A |
| `systemd.sh` | in its own machine | units parse, timers are on, the backup service records a run, the pacman hook, the settle service's condition, a broken config, the restoring flag |
| `fixtures.sh` | | what A is set up with, and so what B must have |
| `facts.sh` | in A and B | writes what is compared (packages, the service, files, users, home files) one per line |
| `compare-records.sh` | in B | compares the records, allowing only the differences it lists, each with its reason |
| `install.sh` | in each machine | installs the binary, units, hook, catalog and default excludes where the install layout says |
| `lib.sh` | | the checks: each failure prints what was expected, what happened and the logs |

The machines share two volumes: `/backup` holds the restic repository, `/e2e` the password, the
facts, A's record and the logs of every command (`/e2e/log/HOST-STEP.log`).

**No AUR packages.** paru and yay install as the user through `sudo`, and a fresh machine first has
to build an AUR helper from the AUR. That would need passwordless `sudo` for the test user, a
compiler toolchain and the AUR being up, adding minutes and a network dependency to every run for a
path the unit tests already cover with recorded output. Machine A only installs packages from the
Arch repositories.

**Same hardware.** Both machines run on the same runner, so B's hardware fingerprint matches A's
and Carryon treats B as A reinstalled: it keeps the machine name `desktop` and ticks the machine
identity bundles (`/etc/hostname`, `/etc/hosts`), as it should for a reinstall. A restore onto
different hardware isn't covered yet.

**A failure** names the check, what was expected and what happened, then shows Carryon's run
history, status, the restore plan, the end of the logs and the journal of Carryon's units.
`KEEP=1` keeps the machines to look around in: `docker exec -it carryon-e2e-b bash`.
