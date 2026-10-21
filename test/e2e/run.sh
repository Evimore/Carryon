#!/bin/bash
# Runs Carryon's end-to-end tests in Arch Linux containers booted with systemd.
#
#   test/e2e/run.sh restore   back up machine A, restore onto a fresh machine B, compare
#   test/e2e/run.sh asyougo   back up machine A, restore it as you go onto a fresh machine C
#   test/e2e/run.sh systemd   the units, the timers' services, the pacman hook and polkit
#   test/e2e/run.sh all       all three (the default)
#
# Needs Docker (or Podman: ENGINE=podman) able to run privileged containers, and Go to build
# Carryon. KEEP=1 leaves the containers and volumes behind to look at; run.sh clean removes them.
set -euo pipefail

ENGINE=${ENGINE:-docker}
IMAGE=${IMAGE:-archlinux:latest}
PREFIX=carryon-e2e
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

say() { printf '\n\033[1m### %s\033[0m\n' "$*"; }

clean() {
	$ENGINE rm -f "$PREFIX-a" "$PREFIX-b" "$PREFIX-c" "$PREFIX-systemd" >/dev/null 2>&1 || true
	$ENGINE volume rm -f "$PREFIX-backup" "$PREFIX-shared" >/dev/null 2>&1 || true
}

build() {
	say "Building Carryon"
	(cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$here/bin/carryon" ./cmd/carryon)
	"$here/bin/carryon" version
}

# boot NAME HOSTNAME: starts a machine with systemd as PID 1, the backup volume at /backup, the
# volume the machines share notes on at /e2e and the checkout at /src, and waits for it to be up.
boot() {
	say "Booting machine $1 ($2)"
	$ENGINE run -d --name "$PREFIX-$1" --hostname "$2" \
		--privileged --cgroupns=private --tmpfs /run --tmpfs /tmp --stop-signal SIGRTMIN+3 \
		-v "$PREFIX-backup:/backup" -v "$PREFIX-shared:/e2e" -v "$root:/src:ro" \
		"$IMAGE" /usr/lib/systemd/systemd >/dev/null
	local state=""
	for _ in $(seq 60); do
		state=$($ENGINE exec "$PREFIX-$1" systemctl is-system-running 2>/dev/null || true)
		case $state in running | degraded) break ;; esac
		sleep 1
	done
	echo "systemd is $state"
	# The container engine bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf; on a real
	# machine they are plain files, which a restore can replace. Make them plain files.
	$ENGINE exec "$PREFIX-$1" sh -c 'for f in /etc/hostname /etc/hosts /etc/resolv.conf; do
		cp "$f" "$f.engine" && umount "$f" && mv "$f.engine" "$f" || exit 1
	done'
	case $state in
	running) ;;
	degraded) $ENGINE exec "$PREFIX-$1" systemctl --failed --no-pager || true ;;
	*)
		echo "FAILED: machine $1 didn't boot (systemd says: ${state:-nothing})"
		$ENGINE logs "$PREFIX-$1" | tail -n 50
		exit 1
		;;
	esac
}

# on NAME SCRIPT: runs one of the scripts here inside machine NAME.
on() {
	say "Machine $1: $2"
	$ENGINE exec -e CI="${CI:-}" "$PREFIX-$1" bash "/src/test/e2e/$2"
}

restore_test() {
	clean
	boot a desktop
	on a machine-a.sh
	$ENGINE rm -f "$PREFIX-a" >/dev/null
	boot b laptop
	on b machine-b.sh
}

asyougo_test() {
	clean
	boot a desktop
	on a machine-a.sh
	$ENGINE rm -f "$PREFIX-a" >/dev/null
	boot c laptop
	on c machine-c.sh
}

systemd_test() {
	clean
	boot systemd desktop
	on systemd systemd.sh
}

what=${1:-all}
if [ "$what" = clean ]; then
	clean
	exit 0
fi
[ "${KEEP:-}" = 1 ] || trap clean EXIT
start=$SECONDS
build
$ENGINE pull -q "$IMAGE" >/dev/null
case $what in
restore) restore_test ;;
asyougo) asyougo_test ;;
systemd) systemd_test ;;
all) restore_test && asyougo_test && systemd_test ;;
*)
	echo "usage: $0 [restore|asyougo|systemd|all|clean]" >&2
	exit 2
	;;
esac
say "Passed in $((SECONDS - start)) seconds"
