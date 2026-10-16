#!/bin/bash
# Compares two system records, A's (from its backup) and B's (captured after the restore).
# Usage: compare-records.sh RECORD_A RECORD_B
#
# After a full restore the records must be the same, except for the differences listed below, each
# with the reason it is expected. Anything else fails the test.
. /src/test/e2e/lib.sh
a=$1 b=$2

# Items allowed to differ, as "type:id" regular expressions.
ALLOWED_ITEMS=(
	'file:/etc/fstab'       # review-only: shown in the plan, never copied
	'file:/etc/resolv.conf' # written by the container engine for each container
	'file:/etc/machine-id'  # each machine's own ID, made at first boot
	# the restore saves the backup's re-download list in the main user's home, and B's capture
	# then finds it there like any other home file
	'path:/home/alice/not-backed-up\.md'
)
# Fields of meta.json allowed to differ.
ALLOWED_META=(
	host         # the hostname
	machine_name # B's machine name depends on whether its hardware fingerprint matches A's
	machine      # the hardware fingerprint (the same here: both containers run on one host)
	user         # the main user is the one with the lowest user ID, and B makes its users in
	home         # the other order on purpose (see machine-b.sh); users are compared as a set
)

items() { jq -c '{key: (.type + ":" + .id), item: .}' "$1/items.jsonl" | sort; }
items "$a" >"$E2E/items-a"
items "$b" >"$E2E/items-b"

allowed_re=$(printf '%s|' "${ALLOWED_ITEMS[@]}")
allowed_re="^\{\"key\":\"(${allowed_re%|})\""
# Print every difference, marked expected or not, so a failure shows the whole picture.
unexpected=0
echo "    items only in A (<) or only in B (>), or different:"
while IFS= read -r line; do
	if printf '%s' "${line:2}" | grep -Eq "$allowed_re"; then
		echo "    expected   $line"
	else
		echo "    UNEXPECTED $line"
		unexpected=$((unexpected + 1))
	fi
done < <(diff "$E2E/items-a" "$E2E/items-b" | grep '^[<>]' || true)
[ "$unexpected" -eq 0 ] || fail "B's record matches A's apart from the expected differences" \
	"only items matching: ${ALLOWED_ITEMS[*]}" "the items marked UNEXPECTED above"
ok "items match apart from the expected differences ($(wc -l <"$E2E/items-a") items in A)"

meta_filter="del($(printf '.%s, ' "${ALLOWED_META[@]}" | sed 's/, $//')) | .users |= sort_by(.name)"
ma=$(jq -S "$meta_filter" "$a/meta.json")
mb=$(jq -S "$meta_filter" "$b/meta.json")
if [ "$ma" != "$mb" ]; then
	diff -u <(echo "$ma") <(echo "$mb") || true
	fail "meta.json matches apart from ${ALLOWED_META[*]}" "the same machine facts" "the differences above"
fi
ok "meta.json matches apart from: ${ALLOWED_META[*]}"
