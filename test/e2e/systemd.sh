#!/bin/bash
# Runs in a machine where systemd is PID 1: Carryon's units load and parse, the backup service
# records a run, the pacman hook refreshes the record and marks a backup owed without ever failing
# pacman, the settle service only starts while a backup is owed, the units behind the agent's
# buttons settle an inbox entry and refuse any other instance name, and polkit loads the rules.
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
run_logged pacman-setup pacman_quiet -Syu --needed restic git jq diffutils polkit
/src/test/e2e/install.sh
install -d -m700 "$E2E/secret"
printf 'example-password-for-the-e2e-test\n' >"$E2E/secret/password"
chmod 600 "$E2E/secret/password"
run_logged init carryon init --repo "$REPO" --password-file "$E2E/secret/password" --defaults --kit-saved

step "The units load and parse"
run_logged analyze systemd-analyze verify /usr/lib/systemd/system/carryon-*.service /usr/lib/systemd/system/carryon-*.timer \
	/usr/lib/systemd/system/carryon-*.path
for t in carryon-backup.timer carryon-verify.timer carryon-settle.timer carryon-flatpak.path; do
	expect_eq "$t enabled" enabled "$(systemctl is-enabled "$t" 2>&1 || true)"
	expect_eq "$t active" active "$(systemctl is-active "$t" 2>&1 || true)"
done
systemctl list-timers --all --no-pager 'carryon-*' | sed 's/^/    | /'
# The user agent is a user unit: no user manager runs here, so systemd-analyze gets a runtime folder
# of its own.
run_logged analyze-user env XDG_RUNTIME_DIR="$(mktemp -d)" systemd-analyze --user verify /usr/lib/systemd/user/carryon-agent.service
expect_eq "carryon-agent.service enabled for every user" enabled "$(systemctl --global is-enabled carryon-agent.service 2>&1 || true)"

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

step "polkit loads Carryon's rules and runs them without errors"
# polkitd compiles every rules file when it starts and logs any that fail. Asking it about
# starting a unit runs the rules; nobody here has a session, so Carryon's leave the answer to
# the defaults. T28 checks the answers for a signed-in user.
systemctl restart polkit.service
pkcheck --action-id org.freedesktop.systemd1.manage-units --process $$ \
	--detail unit carryon-backup.service --detail verb start >/dev/null 2>&1 || true
pkcheck --action-id org.freedesktop.systemd1.manage-units --process $$ \
	--detail unit carryon-add@-etc-shadow.service --detail verb start >/dev/null 2>&1 || true
sleep 1
journalctl --no-pager -b -u polkit.service | sed 's/^/    | /'
expect_eq "polkit.service active" active "$(systemctl is-active polkit.service 2>&1 || true)"
if journalctl --no-pager -b -u polkit.service | grep -iE 'carryon\.rules|error|exception'; then
	fail "polkit runs carryon.rules without errors" "no errors in polkit's journal" "errors above"
fi
ok "polkit loaded and ran the rules without errors"

# entry_id KIND PATH: the inbox entry ID of a path, as state.EntryID makes it.
entry_id() { printf '%s-%s' "$1" "$(printf '%s\0%s' "$1" "$2" | sha256sum | cut -c1-10)"; }

# started UNIT: starts a oneshot unit and says how it went, from systemctl's exit status, since
# systemctl start waits for it to finish. A unit that ran cleanly is unloaded right away, so
# systemctl show would only give defaults.
started() { if systemctl start "$1"; then echo success; else echo failed; fi; }

step "carryon-add@ and carryon-ignore@ settle an inbox entry and refuse anything else"
data=/var/lib/e2e-data scratch=/srv/e2e-scratch
add_id=$(entry_id path "$data") ignore_id=$(entry_id path "$scratch")
jq -n --arg a "$add_id" --arg ap "$data" --arg i "$ignore_id" --arg ip "$scratch" '{format: 1, entries: [
	{found: "2026-10-01T03:00:00Z", id: $a, kind: "path", path: $ap},
	{found: "2026-10-01T03:00:00Z", id: $i, kind: "path", path: $ip}]}' >/var/lib/carryon/inbox.json
chmod 644 /var/lib/carryon/inbox.json
config=$(sha256sum </etc/carryon/config.toml)
for unit in 'carryon-add@-etc-shadow.service' 'carryon-ignore@-etc.service' \
	'carryon-add@path-0123456789.service' 'carryon-add@--yes.service' "carryon-add@${add_id^^}.service"; do
	expect_eq "$unit refused" failed "$(started "$unit")"
	journalctl --no-pager -n 3 -o cat -u "$unit" | sed 's/^/    | /'
done
expect_eq "the refused units left the config alone" "$config" "$(sha256sum </etc/carryon/config.toml)"
expect_eq "the refused units left the inbox alone" 2 "$(jq '.entries | length' /var/lib/carryon/inbox.json)"
expect_eq "carryon-add@$add_id.service" success "$(started "carryon-add@$add_id.service")"
grep -q "\"$data\"" /etc/carryon/config.toml || fail "backup.extra_paths has $data" "$data in config.toml" "$(grep -n extra_paths /etc/carryon/config.toml)"
ok "backup.extra_paths has $data"
expect_eq "carryon-ignore@$ignore_id.service" success "$(started "carryon-ignore@$ignore_id.service")"
grep -q "\"$scratch\"" /etc/carryon/config.toml || fail "detect.ignore has $scratch" "$scratch in config.toml" "$(grep -n ignore /etc/carryon/config.toml)"
ok "detect.ignore has $scratch"
expect_eq "inbox entries left" 0 "$(jq '.entries | length' /var/lib/carryon/inbox.json)"
expect_eq "carryon-add@ of a settled entry" failed "$(started "carryon-add@$add_id.service")"
