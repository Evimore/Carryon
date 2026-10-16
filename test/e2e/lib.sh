# Helpers for the scripts that run inside the test machines. Sourced, not run.
#
# Every check prints what it expected and what happened when it fails, then the logs that help
# (see show_logs), and stops the script.

set -euo pipefail

E2E=/e2e          # shared between the machines: notes, facts, copies of records
REPO=/backup/repo # the restic repository, on its own volume

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
ok() { printf '    \033[32mok\033[0m  %s\n' "$*"; }

# fail WHAT EXPECTED ACTUAL: says what went wrong, shows the logs and stops.
fail() {
	printf '\n\033[1;31mFAILED: %s\033[0m\n' "$1" >&2
	printf '  expected: %s\n' "$2" >&2
	printf '  got:      %s\n' "$3" >&2
	show_logs >&2
	exit 1
}

# show_logs prints Carryon's run history, status and the end of the journal, whatever exists.
show_logs() {
	echo "---- logs on $(uname -n) ----"
	for f in /var/lib/carryon/runs.jsonl /var/lib/carryon/status.json /var/lib/carryon/restore-plan.txt; do
		[ -e "$f" ] || continue
		echo "-- $f (last 40 lines)"
		tail -n 40 "$f"
	done
	if [ -d "$E2E/log" ]; then
		for f in "$E2E"/log/"$(uname -n)"-*.log; do
			[ -e "$f" ] || continue
			echo "-- $f (last 40 lines)"
			tail -n 40 "$f"
		done
	fi
	if command -v journalctl >/dev/null && [ -d /run/systemd/system ]; then
		echo "-- journal (carryon units, last 60 lines)"
		journalctl --no-pager -n 60 -u 'carryon*' 2>/dev/null || true
	fi
	echo "---- end of logs ----"
}

# expect_eq WHAT EXPECTED ACTUAL
expect_eq() {
	[ "$2" = "$3" ] || fail "$1" "$2" "$3"
	ok "$1"
}

# expect_file PATH / expect_no_file PATH
expect_file() { [ -e "$1" ] || fail "$1 exists" "the file is there" "it is missing"; ok "$1 exists"; }
expect_no_file() { [ ! -e "$1" ] || fail "$1 is gone" "no such file" "it exists"; ok "$1 is absent"; }

# run_logged NAME CMD...: runs a command with its output shown and kept in $E2E/log, and stops
# with the end of that output when the command fails.
run_logged() {
	local name=$1
	shift
	mkdir -p "$E2E/log"
	local log="$E2E/log/$(uname -n)-$name.log"
	echo "    \$ $*"
	local rc=0
	"$@" >"$log" 2>&1 || rc=$?
	sed 's/^/    | /' "$log"
	if [ "$rc" -ne 0 ]; then
		fail "$*" "exit status 0" "exit status $rc (output above, also in $log)"
	fi
}

# run_status NAME CMD...: like run_logged but returns the exit status instead of failing.
run_status() {
	local name=$1
	shift
	mkdir -p "$E2E/log"
	local log="$E2E/log/$(uname -n)-$name.log"
	echo "    \$ $*"
	local rc=0
	"$@" >"$log" 2>&1 || rc=$?
	sed 's/^/    | /' "$log"
	echo "    (exit status $rc)"
	return "$rc"
}

# tree_sum DIR: one checksum over every file's path, mode and content, to see whether anything
# under DIR changed.
tree_sum() {
	(cd "$1" && find . -printf '%p %m %s\n' | sort && find . -type f -print0 | sort -z | xargs -0r sha256sum) | sha256sum | cut -d' ' -f1
}

# pacman_quiet ARGS...: pacman without questions or progress bars.
pacman_quiet() { pacman --noconfirm --noprogressbar "$@"; }
