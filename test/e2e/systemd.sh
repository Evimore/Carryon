#!/bin/bash
# Runs in a machine where systemd is PID 1: Carryon's units load and parse, the backup service
# records a run, the pacman hook refreshes the record and marks a backup owed without ever failing
# pacman, and the settle service only starts while a backup is owed.
. /src/test/e2e/lib.sh

# unit_result UNIT: how the unit's last start went: "success", "failed", or "skipped" when its
# conditions said no.
unit_result() {
	if [ "$(systemctl show -p ConditionResult --value "$1")" = no ]; then
		echo skipped
	else
		systemctl show -p Result --value "$1"
	fi
}
runs() { [ -e /var/lib/carryon/runs.jsonl ] && wc -l </var/lib/carryon/runs.jsonl || echo 0; }

step "Setting up"
run_logged pacman-setup pacman_quiet -Syu --needed restic git jq diffutils
/src/test/e2e/install.sh
install -d -m700 "$E2E/secret"
printf 'example-password-for-the-e2e-test\n' >"$E2E/secret/password"
chmod 600 "$E2E/secret/password"
run_logged init carryon init --repo "$REPO" --password-file "$E2E/secret/password" --defaults --kit-saved

step "The units load and parse"
run_logged analyze systemd-analyze verify /usr/lib/systemd/system/carryon-*.service /usr/lib/systemd/system/carryon-*.timer
for t in carryon-backup.timer carryon-verify.timer carryon-settle.timer; do
	expect_eq "$t enabled" enabled "$(systemctl is-enabled "$t" 2>&1 || true)"
	expect_eq "$t active" active "$(systemctl is-active "$t" 2>&1 || true)"
done
systemctl list-timers --all --no-pager 'carryon-*' | sed 's/^/    | /'

step "carryon-backup.service backs up and records the run"
before=$(runs)
systemctl start carryon-backup.service || true
expect_eq "carryon-backup.service result" success "$(unit_result carryon-backup.service)"
expect_eq "runs recorded" "$((before + 1))" "$(runs)"
expect_eq "the run's trigger" timer "$(tail -n1 /var/lib/carryon/runs.jsonl | jq -r .trigger)"
expect_eq "the run's result" ok "$(tail -n1 /var/lib/carryon/runs.jsonl | jq -r .result)"
expect_file /run/carryon/status.json

step "carryon-settle.service doesn't start while no backup is owed"
expect_no_file /var/lib/carryon/pending
before=$(runs)
systemctl start carryon-settle.service || true
expect_eq "carryon-settle.service" skipped "$(unit_result carryon-settle.service)"
expect_eq "runs recorded" "$before" "$(runs)"

step "pacman -S runs the hook, which refreshes the record and marks a backup owed"
commits=$(git -C /var/lib/carryon/record rev-list --count HEAD)
run_logged pacman-tree pacman_quiet -S tree
grep -q "Updating Carryon's system record" "$E2E/log/$(uname -n)-pacman-tree.log" ||
	fail "pacman runs the hook" "\"Updating Carryon's system record...\" in pacman's output" "not there"
ok "pacman ran the hook"
jq -e 'select(.type == "pkg" and .id == "tree")' /var/lib/carryon/record/items.jsonl >/dev/null ||
	fail "the record lists the new package" "an item pkg:tree in items.jsonl" "none"
ok "the record lists tree"
expect_eq "record commits" "$((commits + 1))" "$(git -C /var/lib/carryon/record rev-list --count HEAD)"
expect_file /var/lib/carryon/pending

step "carryon-settle.service starts once a backup is owed, and backs up once changes settled"
run_logged settle-delay carryon config set backup.after_change_delay 1s
sleep 2
before=$(runs)
systemctl start carryon-settle.service || true
expect_eq "carryon-settle.service result" success "$(unit_result carryon-settle.service)"
expect_eq "runs recorded" "$((before + 1))" "$(runs)"
expect_eq "the run's trigger" settle "$(tail -n1 /var/lib/carryon/runs.jsonl | jq -r .trigger)"
expect_no_file /var/lib/carryon/pending

step "The hook never fails pacman, even with a broken config"
cp /etc/carryon/config.toml "$E2E/config.toml.good"
printf 'this is [not valid toml\n' >/etc/carryon/config.toml
run_logged pacman-broken pacman_quiet -S htop
expect_eq "htop installed" htop "$(pacman -Qq htop 2>&1)"
grep -qi "settings can't be read" "$E2E/log/$(uname -n)-pacman-broken.log" ||
	fail "the hook says the settings are broken" "a hint in pacman's output" "none"
ok "the hook said the settings can't be read"
run_logged pacman-remove-broken pacman_quiet -R htop
cp "$E2E/config.toml.good" /etc/carryon/config.toml

step "While restoring, the hook does nothing and the timers' services don't start"
touch /var/lib/carryon/restoring
rm -f /var/lib/carryon/pending
commits=$(git -C /var/lib/carryon/record rev-list --count HEAD)
run_logged pacman-restoring pacman_quiet -S htop
expect_eq "record commits" "$commits" "$(git -C /var/lib/carryon/record rev-list --count HEAD)"
expect_no_file /var/lib/carryon/pending
systemctl start carryon-backup.service || true
expect_eq "carryon-backup.service while restoring" skipped "$(unit_result carryon-backup.service)"
rm /var/lib/carryon/restoring

step "A failed backup leaves a login message, and the next good one removes it"
expect_no_file /run/motd.d/carryon.motd
# A different repository at the same place: the backup refuses it and fails. (A wrong password
# only makes it wait for the destination.)
mv "$REPO" "$REPO.real"
restic init --repo "$REPO" --password-file "$E2E/secret/password" >/dev/null
systemctl start carryon-backup.service || true
expect_eq "carryon-backup.service with another repository" exit-code "$(unit_result carryon-backup.service)"
expect_eq "the run's result" failed "$(tail -n1 /var/lib/carryon/runs.jsonl | jq -r .result)"
expect_file /run/motd.d/carryon.motd
sed 's/^/    | /' /run/motd.d/carryon.motd
grep -q '^Carryon: the last backup failed .*See: carryon status$' /run/motd.d/carryon.motd ||
	fail "the login message names the failed backup" "Carryon: the last backup failed ..." "$(cat /run/motd.d/carryon.motd)"
ok "the login message names the failed backup"
rm -rf "$REPO"
mv "$REPO.real" "$REPO"
systemctl reset-failed carryon-backup.service
systemctl start carryon-backup.service || true
expect_eq "carryon-backup.service result" success "$(unit_result carryon-backup.service)"
expect_no_file /run/motd.d/carryon.motd
