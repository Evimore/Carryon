# End-to-end tests

These tests run Carryon on whole Arch Linux machines: containers from `archlinux:latest`, booted
with systemd as PID 1. Run them with `test/e2e/run.sh` (see [CONTRIBUTING.md](../../CONTRIBUTING.md)).
CI runs them in the "End-to-end" workflow, one job per `run.sh` scenario, in parallel. Each job must stay
under 15 minutes: add a job for a new scenario rather than slow one down.

| File | Runs | What it does |
|---|---|---|
| `run.sh` | on the host | builds Carryon, boots the machines, runs the scripts below in them: `restore` (A then B), `asyougo` (A then C), `systemd`, or `all` |
| `machine-a.sh` | in machine A, "desktop" | sets up packages, files, a service, users, home files, an app's settings, a user unit and an unowned data folder; `init`, `backup`; the data folder goes from the inbox into the next backup (`inbox`, `why`, `add`); `verify --deep`, `status --json`, `history`; a backup to an unmounted volume and to a different repository writes nothing |
| `machine-b.sh` | in machine B, "laptop" | a fresh machine with the same users: `init --connect`, `restore --defaults` (changes nothing), `restore --plan` (changes nothing), `restore --defaults --yes`, then compares B with A, the user unit included |
| `machine-c.sh` | in machine C, "laptop" | a fresh machine with alice and carol: `init --connect --restore as-you-go --machine-name laptop`; installing the app prints the hint once and adds a `restore` inbox entry; `carryon-restore@` refuses an unknown bundle and applies the offer; `restore --bundle --user bob=carol` gives bob's settings to carol; nothing else comes back; C's backup leaves A's alone |
| `systemd.sh` | in its own machine | units parse, timers are on, the backup service records a run, the pacman hook, the settle service's condition, a broken config, the restoring flag, the login message after a failed backup, the add and ignore units, polkit for signed-in users who aren't root |
| `fixtures.sh` | | what A is set up with, and so what B must have |
| `facts.sh` | in A and B | writes what is compared (packages, the service, files, users, lingering, the user unit, home files) one per line |
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

**Same hardware.** The machines run on the same runner, so B's hardware fingerprint matches A's
and Carryon treats B as A reinstalled: it keeps the machine name `desktop` and ticks the machine
identity bundles (`/etc/hostname`, `/etc/hosts`), as it should for a reinstall. C is named
`laptop` with `--machine-name`, which is how a second machine on the same hardware looks to
Carryon; a restore onto different hardware isn't covered yet.

**polkit and sessions.** The rules let a user start a backup from a local, active session, and
polkit counts a session as local only when it is on a seat. The systemd machine opens a real logind
session through PAM (`systemd-run --uid=USER -p PAMName=login`, as a console login does with
`pam_systemd`) on a spare virtual console, `/dev/tty63`, which it switches to the front with
`chvt`: a session on seat0, local and active. That switches the console of the computer running the
containers, so this part only runs on CI (`CI=true`); locally it says it was skipped. Sessions
without a seat, and no session at all, are checked to be refused. Not covered: several sessions on
one seat, a graphical session, and the password prompt itself (no agent runs).

**A failure** names the check, what was expected and what happened, then shows Carryon's run
history, status, the restore plan, the end of the logs and the journal of Carryon's units.
`KEEP=1` keeps the machines to look around in: `docker exec -it carryon-e2e-b bash`.
