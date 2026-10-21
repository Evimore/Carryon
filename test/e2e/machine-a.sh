#!/bin/bash
# Machine A ("desktop"): set up packages, settings, users and their files, install Carryon, back up
# to the repository on the shared volume, check the backup, follow a new data folder from the
# inbox into the backup, and check that a backup aimed at the wrong destination writes nothing.
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
	install -d -o "$u" -g "$u" "$home/notes" "$home/.config" "$home/.config/exampleapp" "$home/.config/exampleapp/themes" "$home/.config/$APP"
	echo "# $u's zsh settings" >"$home/.zshrc"
	printf 'buy milk\nwrite tests for %s\n' "$u" >"$home/notes/todo.txt"
	printf '[general]\nuser = %s\nvolume = 7\n' "$u" >"$home/.config/exampleapp/settings.ini"
	printf '[colors]\nbackground = #101010\n' >"$home/.config/exampleapp/themes/dark.ini"
	# The app's settings folder, named like its package: its bundle is offered as you go.
	printf 'set tabsize 4\n# %s likes short tabs\n' "$u" >"$home/.config/$APP/nanorc"
	chown "$u:$u" "$home/.zshrc" "$home/notes/todo.txt" "$home/.config/exampleapp/settings.ini" "$home/.config/exampleapp/themes/dark.ini" "$home/.config/$APP/nanorc"
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
# A data folder no package owns, like a server's database: not backed up until accepted.
install -d "$DATA_DIR"
printf 'example database\n' >"$DATA_DIR/data.db"
# A user unit alice made and enabled (the symlink systemctl --user enable makes), running while
# she's signed out.
units=/home/$UNIT_USER/.config/systemd/user
install -d -o "$UNIT_USER" -g "$UNIT_USER" "$(dirname "$units")" "$units" "$units/default.target.wants"
printf '[Unit]\nDescription=Example sync (end-to-end test)\n\n[Service]\nType=oneshot\nExecStart=/usr/bin/true\nRemainAfterExit=yes\n\n[Install]\nWantedBy=default.target\n' >"$units/$USER_UNIT"
ln -s "$units/$USER_UNIT" "$units/default.target.wants/$USER_UNIT"
chown -h "$UNIT_USER:$UNIT_USER" "$units/$USER_UNIT" "$units/default.target.wants/$USER_UNIT"
loginctl enable-linger "$UNIT_USER"
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

step "A new data folder goes from the inbox into the backup"
# The backup above looked for new data folders (detect.new_paths = "ask" by default).
run_logged inbox carryon inbox
data_id=$(jq -r --arg p "$DATA_DIR" '.entries[] | select(.kind == "path" and .path == $p) | .id' /var/lib/carryon/inbox.json)
[ -n "$data_id" ] || fail "the backup suggests $DATA_DIR" "an inbox entry for $DATA_DIR" "$(jq -c '[.entries[] | {kind, path, bundle}]' /var/lib/carryon/inbox.json)"
ok "the inbox suggests $DATA_DIR as $data_id"
expect_eq "carryon status counts the suggestion" true "$(carryon status --json | jq --argjson n 1 '.inbox >= $n' || true)"
run_logged why-before carryon why "$DATA_DIR/data.db"
expect_eq "carryon why says it's not backed up" false "$(carryon why --json "$DATA_DIR/data.db" | jq .backed_up)"
expect_eq "carryon why names the waiting suggestion" "$data_id" "$(carryon why --json "$DATA_DIR/data.db" | jq -r .waiting.id)"
grep -q "sudo carryon add $data_id" "$E2E/log/$(uname -n)-why-before.log" ||
	fail "carryon why says how to back it up" "\"sudo carryon add $data_id\" in its output" "not there"
ok "carryon why says how to back it up"
run_logged add carryon add "$data_id"
grep -q "\"$DATA_DIR\"" /etc/carryon/config.toml || fail "backup.extra_paths has $DATA_DIR" "$DATA_DIR in config.toml" "$(grep -n extra_paths /etc/carryon/config.toml)"
ok "backup.extra_paths has $DATA_DIR"
expect_eq "the suggestion is settled" "" "$(jq -r --arg id "$data_id" '.entries[] | select(.id == $id) | .id' /var/lib/carryon/inbox.json)"
run_logged backup-data carryon backup
run_logged ls-data carryon restic ls latest "$DATA_DIR"
grep -q "^$DATA_DIR/data.db\$" "$E2E/log/$(uname -n)-ls-data.log" ||
	fail "the next snapshot holds $DATA_DIR" "$DATA_DIR/data.db listed" "not listed (output above)"
ok "the next snapshot holds $DATA_DIR/data.db"
run_logged why-after carryon why "$DATA_DIR/data.db"
why=$(carryon why --json "$DATA_DIR/data.db")
expect_eq "carryon why says it's backed up" true "$(jq .backed_up <<<"$why")"
expect_eq "carryon why names the folder" "$DATA_DIR extra_paths" "$(jq -r '.folder | "\(.path) \(.from)"' <<<"$why")"
grep -q "backup.extra_paths" "$E2E/log/$(uname -n)-why-after.log" ||
	fail "carryon why explains it" "backup.extra_paths named in its output" "not there"
ok "carryon why explains it with backup.extra_paths"
expect_eq "nothing waits for $DATA_DIR" null "$(jq .waiting <<<"$why")"

step "The app's settings form a bundle to offer as you go"
run_logged bundles carryon bundles
jq -e --arg b "$APP_BUNDLE" --arg h ".config/$APP" 'select(.bundle == $b and .home == $h)' /var/lib/carryon/record/items.jsonl >/dev/null ||
	fail "$APP's settings are in $APP_BUNDLE" "items ~/.config/$APP in bundle $APP_BUNDLE" "$(jq -c --arg h ".config/$APP" 'select(.home == $h) | {type, id, user, bundle}' /var/lib/carryon/record/items.jsonl)"
ok "the users' .config/$APP is in $APP_BUNDLE"
jq -e --arg u "$UNIT_USER" --arg n "$USER_UNIT" 'select(.type == "usvc" and .id == $n and .user == $u)' /var/lib/carryon/record/items.jsonl >/dev/null ||
	fail "the record holds $UNIT_USER's enabled $USER_UNIT" "a usvc item $USER_UNIT for $UNIT_USER" "$(jq -c 'select(.user != null and (.type | test("^u"))) | {type, id, user}' /var/lib/carryon/record/items.jsonl)"
ok "the record holds $UNIT_USER's $USER_UNIT"

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
expect_eq "snapshots after the refused backups" 2 "$(carryon restic snapshots --json | jq length)"

step "Saving A's record for the comparison"
rm -rf "$E2E/record-a"
cp -a /var/lib/carryon/record "$E2E/record-a"
rm -rf "$E2E/record-a/.git"
ok "copied to $E2E/record-a"
