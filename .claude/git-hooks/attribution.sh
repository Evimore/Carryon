#!/bin/sh
# Keeps AI attribution out of the project (AGENTS.md, "Pull requests"): Co-authored-by lines that
# name Claude, Claude-Session links, "Generated with Claude Code" lines, commits authored or
# committed as Claude's address, and branches named claude/....
#
#   attribution.sh strip FILE      remove such lines from a commit message (the commit-msg hook)
#   attribution.sh commits RANGE   check the commits in a git range, or --all for every commit
#   attribution.sh text            check standard input, such as a pull request's description
#   attribution.sh branch NAME     check a branch name
set -eu

pattern='^[[:space:]]*(co-authored-by:.*(claude|anthropic)|claude-session:)|generated (with|by) \[?claude code|claude\.ai/code/session_|claude\.com/claude-code'
address='noreply@anthropic\.com'

case ${1:-} in
strip)
	msg=$2
	if grep -qiE "$pattern" "$msg"; then
		# Drop the lines, and the blank lines they leave at the end.
		grep -viE "$pattern" "$msg" | awk 'NF { for (; n > 0; n--) print ""; print; next } { n++ }' >"$msg.tmp" || true
		mv "$msg.tmp" "$msg"
	fi
	for who in AUTHOR COMMITTER; do
		if git var "GIT_${who}_IDENT" | grep -qiE "$address"; then
			echo "This commit would be made as Claude ($(git var "GIT_${who}_IDENT" | sed 's/ [0-9]* [-+][0-9]*$//'))." >&2
			echo "Set GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL to the owner's" >&2
			echo "name and email (in a cloud session: the environment's variables), then commit again." >&2
			exit 1
		fi
	done
	;;
commits)
	range=$2
	[ "$range" = --all ] && range=HEAD
	status=0
	for c in $(git rev-list "$range"); do
		who=$(git log -1 --format='%an <%ae>, committed by %cn <%ce>' "$c")
		if echo "$who" | grep -qiE "$address"; then
			echo "$(git log -1 --format='%h %s' "$c"): made as Claude ($who)"
			status=1
		fi
		lines=$(git log -1 --format=%B "$c" | grep -iE "$pattern" || true)
		if [ -n "$lines" ]; then
			echo "$(git log -1 --format='%h %s' "$c"): attribution in the message:"
			echo "$lines" | sed 's/^/    /'
			status=1
		fi
	done
	if [ $status -ne 0 ]; then
		echo "Remove these lines and author the commits as the owner (see AGENTS.md, \"Pull requests\")."
	fi
	exit $status
	;;
text)
	lines=$(grep -iE "$pattern" || true)
	if [ -n "$lines" ]; then
		echo "AI attribution found; remove these lines (see AGENTS.md, \"Pull requests\"):"
		echo "$lines" | sed 's/^/    /'
		exit 1
	fi
	;;
branch)
	if echo "$2" | grep -qiE '^claude/'; then
		echo "The branch $2 is named claude/...: push the commits to a branch named after what they" \
			"build (such as inbox or fix-hook-timeout) and open the pull request from it (see AGENTS.md)."
		exit 1
	fi
	;;
*)
	echo "usage: $0 strip FILE | commits RANGE|--all | text | branch NAME" >&2
	exit 2
	;;
esac
