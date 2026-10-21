#!/bin/bash
# Machine C ("laptop"): a fresh install connected to A's repository to restore as you go, under a
# machine name of its own, with alice and carol instead of alice and bob. Installing the app offers
# its saved settings once, carryon-restore@ and restore --bundle bring back only that bundle, and
# C's backups don't touch A's.
. /src/test/e2e/lib.sh
. /src/test/e2e/fixtures.sh

inbox_restore() { jq -r --arg b "$1" '.entries[] | select(.kind == "restore" and .bundle == $b) | .id' /var/lib/carryon/inbox.json 2>/dev/null || true; }
snapshots_of() { carryon restic snapshots --json | jq --arg h "$1" '[.[] | select(.hostname == $h)] | length'; }
# home_fact USER FILE: what facts.sh says of a home file, with "file" for the user's name.
home_fact() {
	local home f=$2
	home=$(getent passwd "$1" | cut -d: -f6)
	if [ -e "$home/$f" ]; then echo "file ~/$f $(stat -c '%a %U:%G' "$home/$f") $(sha256sum <"$home/$f" | cut -c1-16)"; else echo "file ~/$f missing"; fi
}

step "Setting up machine C, a fresh install"
run_logged pacman-setup pacman_quiet -Syu --needed $TOOL_PACKAGES
# alice as on A; carol is the one to receive bob's files.
useradd -m alice
useradd -m carol
/src/test/e2e/install.sh

step "carryon init --connect, restoring as you go, as a machine of its own"
run_logged connect carryon init --connect "$REPO" --password-file "$E2E/secret/password" --settings-from desktop \
	--restore as-you-go --machine-name laptop --defaults
expect_eq "machine name" laptop "$(carryon config get machine_name)"
expect_no_file /var/lib/carryon/restoring
expect_eq "status says restoring" false "$(carryon status --json | jq -r .restoring || true)"
expect_file /var/lib/carryon/source
expect_eq "restore offers waiting" "" "$(jq -r '.entries[] | select(.kind == "restore") | .id' /var/lib/carryon/inbox.json 2>/dev/null || true)"
desktop_snapshots=$(snapshots_of desktop)

step "Installing $APP offers its saved settings, once"
run_logged pacman-app pacman_quiet -S "$APP"
hint="carryon: saved settings for $APP (.*) — restore with: carryon restore --bundle $APP_BUNDLE"
expect_eq "hints in pacman's output" 1 "$(grep -c "$hint" "$E2E/log/$(uname -n)-pacman-app.log" || true)"
offer=$(inbox_restore "$APP_BUNDLE")
[ -n "$offer" ] || fail "the inbox offers $APP_BUNDLE" "a restore entry for $APP_BUNDLE" "$(jq -c '[.entries[] | {kind, path, bundle}]' /var/lib/carryon/inbox.json)"
ok "the inbox offers $APP_BUNDLE as $offer"
jq -r --arg id "$offer" '.entries[] | select(.id == $id) | .changes[]' /var/lib/carryon/inbox.json | sed 's/^/    | /'
run_logged inbox carryon inbox
# Reinstalling, and removing and installing again, offer nothing new: it's offered already.
run_logged pacman-app-again pacman_quiet -S "$APP"
run_logged pacman-app-remove pacman_quiet -R "$APP"
run_logged pacman-app-back pacman_quiet -S "$APP"
for log in pacman-app-again pacman-app-remove pacman-app-back; do
	expect_eq "hints in $log" 0 "$(grep -c 'carryon: saved settings' "$E2E/log/$(uname -n)-$log.log" || true)"
done
expect_eq "the offer is still there, once" "$offer" "$(inbox_restore "$APP_BUNDLE")"

step "carryon-restore@ refuses what wasn't offered"
for unit in 'carryon-restore@app-nothing-here.service' 'carryon-restore@-etc-shadow.service'; do
	rc=0
	systemctl start "$unit" || rc=$?
	expect_eq "$unit refused" true "$([ "$rc" -ne 0 ] && echo true || echo "false (exit status 0)")"
	journalctl --no-pager -n 3 -o cat -u "$unit" | sed 's/^/    | /'
done
expect_eq "alice's ~/.config/$APP/nanorc after the refused units" "file ~/.config/$APP/nanorc missing" "$(home_fact alice ".config/$APP/nanorc")"

step "carryon-restore@$APP_BUNDLE.service applies the offer"
systemctl start "carryon-restore@$APP_BUNDLE.service" ||
	fail "carryon-restore@$APP_BUNDLE.service" "it succeeds" "it failed: $(journalctl --no-pager -n 20 -o cat -u "carryon-restore@$APP_BUNDLE.service")"
journalctl --no-pager -o cat -u "carryon-restore@$APP_BUNDLE.service" | sed 's/^/    | /'
expect_eq "alice's ~/.config/$APP/nanorc" "$(sed -n "s|^home alice |file |p" "$E2E/facts-a" | grep "^file ~/.config/$APP/nanorc ")" \
	"$(home_fact alice ".config/$APP/nanorc")"
expect_eq "the offer is settled" "" "$(inbox_restore "$APP_BUNDLE")"
# bob has no account here, so the offer left his settings out.
expect_eq "carol's ~/.config/$APP/nanorc" "file ~/.config/$APP/nanorc missing" "$(home_fact carol ".config/$APP/nanorc")"

step "carryon restore --bundle $APP_BUNDLE --user bob=carol gives bob's settings to carol"
run_logged restore-bundle carryon restore --bundle "$APP_BUNDLE" --user bob=carol --yes
grep -q "kept record of desktop" "$E2E/log/$(uname -n)-restore-bundle.log" ||
	fail "restore --bundle uses the kept record" "\"kept record of desktop\" in its output" "not there"
ok "restore --bundle used the kept record"
expect_eq "carol's ~/.config/$APP/nanorc, bob's on A, now carol's" \
	"$(sed -n "s|^home bob |file |p" "$E2E/facts-a" | grep "^file ~/.config/$APP/nanorc " | sed 's/bob:bob/carol:carol/')" \
	"$(home_fact carol ".config/$APP/nanorc")"

step "Only $APP_BUNDLE came back"
for f in $SYSTEM_FILES; do
	[ ! -e "$f" ] || fail "$f wasn't restored" "no such file: it isn't in $APP_BUNDLE" "it exists"
done
ok "no system file from A is here"
for u in alice carol; do
	for f in $HOME_FILES; do
		[ "$f" = ".config/$APP/nanorc" ] && continue
		expect_eq "$u's ~/$f" "file ~/$f missing" "$(home_fact "$u" "$f")"
	done
done
for p in $APP_PACKAGES; do
	[ "$p" = "$APP" ] && continue
	expect_eq "package $p" missing "$(pacman -Q "$p" >/dev/null 2>&1 && echo installed || echo missing)"
done

step "C backs up under its own name, beside A's backups"
run_logged backup carryon backup
expect_eq "laptop's snapshots" 1 "$(snapshots_of laptop)"
expect_eq "desktop's snapshots, untouched" "$desktop_snapshots" "$(snapshots_of desktop)"
expect_eq "machine in status" laptop "$(carryon status --json | jq -r .machine)"
expect_eq "last backup result" ok "$(carryon status --json | jq -r .last_backup.result)"
