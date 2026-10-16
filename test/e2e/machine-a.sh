#!/bin/bash
# Machine A ("desktop"): set up packages, settings, users and their files, install Carryon, back up
# to the repository on the shared volume, check the backup, and check that a backup aimed at the
# wrong destination writes nothing.
. /src/test/e2e/lib.sh
. /src/test/e2e/fixtures.sh

step "Setting up machine A"
# Packages first, before Carryon and its pacman hook are installed.
run_logged pacman-setup pacman_quiet -Syu --needed $TOOL_PACKAGES $APP_PACKAGES
for u in $USERS; do
	useradd -m -G "${GROUPS_OF[$u]}" -s "${SHELL_OF[$u]}" "$u"
done
for u in $USERS; do
	home=/home/$u
	install -d -o "$u" -g "$u" "$home/notes" "$home/.config" "$home/.config/exampleapp" "$home/.config/exampleapp/themes"
	echo "# $u's zsh settings" >"$home/.zshrc"
	printf 'buy milk\nwrite tests for %s\n' "$u" >"$home/notes/todo.txt"
	printf '[general]\nuser = %s\nvolume = 7\n' "$u" >"$home/.config/exampleapp/settings.ini"
	printf '[colors]\nbackground = #101010\n' >"$home/.config/exampleapp/themes/dark.ini"
	chown "$u:$u" "$home/.zshrc" "$home/notes/todo.txt" "$home/.config/exampleapp/settings.ini" "$home/.config/exampleapp/themes/dark.ini"
	chmod 600 "$home/notes/todo.txt"
done
# A package's backup= file, changed.
printf '\n# Changed by the end-to-end test\nMaxAuthTries 3\n' >>/etc/ssh/sshd_config
# Files no package owns, one only root and wheel may read.
install -d /etc/exampled
printf 'listen = 127.0.0.1:8080\n' >/etc/exampled/exampled.conf
printf 'not-a-real-key\n' >/etc/exampled/secret.key
chown root:wheel /etc/exampled/secret.key
chmod 640 /etc/exampled/secret.key
# An extra path outside the folders Carryon always backs up.
install -d -o alice -g alice /opt/exampledata
printf 'example data\n' >/opt/exampledata/data.txt
chown alice:alice /opt/exampledata/data.txt
# A review-only file: the restore shows it but must never copy it. The entries also tell
# carryon verify how the container's mounts come back, as a real machine's fstab would for its
# backup drive; noauto, since the container engine mounts them.
cat >>/etc/fstab <<'FSTAB'

# Changed by the end-to-end test on machine A: a restore must not copy these lines.
/dev/disk/by-label/backup /backup ext4 noauto,nofail 0 0
/dev/disk/by-label/e2e    /e2e    ext4 noauto,nofail 0 0
/dev/disk/by-label/src    /src    ext4 noauto,nofail,ro 0 0
FSTAB
systemctl enable "$SERVICE"
/src/test/e2e/facts.sh a

/src/test/e2e/install.sh

step "carryon init"
install -d -m700 "$E2E/secret"
printf 'example-password-for-the-e2e-test\n' >"$E2E/secret/password"
chmod 600 "$E2E/secret/password"
run_logged init carryon init --repo "$REPO" --password-file "$E2E/secret/password" --users '*' --defaults --kit-saved
run_logged config-extra carryon config set backup.extra_paths "/root, /srv, /usr/local, /opt/exampledata"
expect_eq "machine name" "desktop" "$(carryon config get machine_name)"

step "carryon backup"
run_logged backup carryon backup
expect_file /var/lib/carryon/record/items.jsonl
snapshots=$(carryon restic snapshots --json | jq length)
expect_eq "snapshots in the repository" 1 "$snapshots"
run_logged ls-home carryon restic ls latest /home/alice/notes/todo.txt /opt/exampledata/data.txt
grep -q '/home/alice/notes/todo.txt' "$E2E/log/$(uname -n)-ls-home.log" || fail "the snapshot holds alice's notes" "/home/alice/notes/todo.txt listed" "not listed"
grep -q '/opt/exampledata/data.txt' "$E2E/log/$(uname -n)-ls-home.log" || fail "the snapshot holds the extra path" "/opt/exampledata/data.txt listed" "not listed"
ok "the snapshot holds home files and the extra path"

step "carryon verify --deep"
run_logged verify carryon verify --deep

step "carryon status --json"
rc=0
carryon status --json >"$E2E/status-a.json" || rc=$?
jq . "$E2E/status-a.json" | sed 's/^/    | /'
expect_eq "last backup result" ok "$(jq -r .last_backup.result "$E2E/status-a.json")"
expect_eq "last check result" ok "$(jq -r '.last_check.result' "$E2E/status-a.json")"
expect_eq "machine in status" desktop "$(jq -r .machine "$E2E/status-a.json")"
expect_eq "restoring in status" false "$(jq -r .restoring "$E2E/status-a.json")"
expect_eq "status --json exit status" 0 "$rc"

step "carryon history"
run_logged history carryon history --changes

step "A backup to the wrong destination writes nothing"
# 1. The backup volume isn't mounted: /backup is an empty folder on the system disk.
install -d /mnt/keep
mount --bind /backup /mnt/keep
umount /backup
before=$(tree_sum /backup)
rc=0
run_status backup-unmounted carryon backup || rc=$?
[ "$rc" -ne 0 ] || grep -qi 'wait\|wrote nothing' "$E2E/log/$(uname -n)-backup-unmounted.log" ||
	fail "backup with the volume unmounted" "a non-zero exit or a 'waiting' message" "exit 0 without either"
expect_eq "files under the unmounted /backup" "$before" "$(tree_sum /backup)"
mount --bind /mnt/keep /backup
umount /mnt/keep
# 2. A different repository at the same place.
mv "$REPO" "$REPO.real"
# The same password, so only the repository ID tells them apart.
restic init --repo "$REPO" --password-file "$E2E/secret/password" >/dev/null
before=$(tree_sum "$REPO")
rc=0
run_status backup-other-repo carryon backup || rc=$?
[ "$rc" -ne 0 ] || fail "backup into a different repository" "a non-zero exit" "exit 0"
expect_eq "files in the other repository" "$before" "$(tree_sum "$REPO")"
rm -rf "$REPO"
mv "$REPO.real" "$REPO"
expect_eq "snapshots after the refused backups" 1 "$(carryon restic snapshots --json | jq length)"

step "Saving A's record for the comparison"
rm -rf "$E2E/record-a"
cp -a /var/lib/carryon/record "$E2E/record-a"
rm -rf "$E2E/record-a/.git"
ok "copied to $E2E/record-a"
