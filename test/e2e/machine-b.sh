#!/bin/bash
# Machine B ("laptop"): a fresh install with the same users, connected to A's repository. Restores
# everything and checks it matches A, and that the restore's safety rails held.
. /src/test/e2e/lib.sh
. /src/test/e2e/fixtures.sh

step "Setting up machine B, a fresh install"
run_logged pacman-setup pacman_quiet -Syu --needed $TOOL_PACKAGES
# The same users, made in the other order so their user IDs differ from A's: the restore must
# hand files over by name.
useradd -m bob
useradd -m alice
id alice bob | sed 's/^/    | /'
fstab_before=$(sha256sum </etc/fstab)
/src/test/e2e/facts.sh b-fresh
/src/test/e2e/install.sh

step "carryon init --connect"
run_logged connect carryon init --connect "$REPO" --password-file "$E2E/secret/password" --settings-from desktop --defaults
expect_file /var/lib/carryon/restoring
expect_eq "status says restoring" true "$(carryon status --json | jq -r .restoring || true)"

step "carryon restore --defaults (without --yes) applies nothing"
rc=0
run_status restore-defaults carryon restore --defaults || rc=$?
/src/test/e2e/facts.sh b-after-defaults
if ! diff -u "$E2E/facts-b-fresh" "$E2E/facts-b-after-defaults"; then
	fail "restore --defaults without --yes changes nothing" "the facts above unchanged" "they changed (diff above)"
fi
ok "nothing changed (exit status $rc)"
expect_file /var/lib/carryon/restoring

step "carryon restore --plan"
run_logged restore-plan carryon restore --plan
/src/test/e2e/facts.sh b-after-plan
diff -u "$E2E/facts-b-fresh" "$E2E/facts-b-after-plan" >/dev/null ||
	fail "restore --plan changes nothing" "the same facts as before" "$(diff "$E2E/facts-b-fresh" "$E2E/facts-b-after-plan" | head -5)"
ok "nothing changed"
expect_file /var/lib/carryon/restore-plan.txt
grep -q '/etc/fstab' /var/lib/carryon/restore-plan.txt ||
	fail "the plan shows /etc/fstab as review-only" "/etc/fstab in restore-plan.txt" "not mentioned"
ok "the plan mentions /etc/fstab"

step "carryon restore --defaults --yes"
# The restore enables user units through the user's own systemd, which runs only while they're
# signed in or lingering. Lingering is in the backup too, but it starts the user manager without
# waiting for it, so start it first, as signing in would.
loginctl enable-linger "$UNIT_USER"
bus=/run/user/$(id -u "$UNIT_USER")/bus
for _ in $(seq 30); do [ -S "$bus" ] && break; sleep 1; done
[ -S "$bus" ] || fail "$UNIT_USER's user manager starts" "$bus" "no such socket after 30 seconds"
run_logged restore carryon restore --defaults --yes

step "Comparing machine B with machine A"
/src/test/e2e/facts.sh b
if ! diff -u "$E2E/facts-a" "$E2E/facts-b"; then
	fail "B matches A after the restore" "the same facts as A" "the differences above (- is A, + is B)"
fi
ok "packages, the service, system files, users, groups, shells, lingering, the user unit and home files match A"
expect_eq "$UNIT_USER's systemd has $USER_UNIT enabled" enabled \
	"$(systemctl --user -M "$UNIT_USER@" is-enabled "$USER_UNIT" 2>&1 || true)"

step "Safety rails"
expect_eq "/etc/fstab untouched" "$fstab_before" "$(sha256sum </etc/fstab)"
expect_no_file /etc/fstab.pre-restore
expect_no_file /var/lib/carryon/restoring
expect_eq "status says restoring" false "$(carryon status --json | jq -r .restoring || true)"

step "Comparing B's system record with A's"
rm -rf "$E2E/record-b" && install -d "$E2E/record-b"
run_logged capture-b carryon capture --out "$E2E/record-b"
/src/test/e2e/compare-records.sh "$E2E/record-a" "$E2E/record-b"
