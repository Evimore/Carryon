#!/bin/bash
# Writes what the test compares between machines A and B, one fact per line, to $E2E/facts-NAME.
# Usage: facts.sh NAME
. /src/test/e2e/lib.sh
. /src/test/e2e/fixtures.sh
out="$E2E/facts-$1"

{
	for p in $APP_PACKAGES $TOOL_PACKAGES; do
		if pacman -Qi "$p" >/dev/null 2>&1; then
			echo "package $p installed $(pacman -Qi "$p" | sed -n 's/^Install Reason *: //p')"
		else
			echo "package $p missing"
		fi
	done
	echo "unit $SERVICE $(systemctl is-enabled "$SERVICE" 2>&1 || true)"
	for f in $SYSTEM_FILES; do
		if [ -e "$f" ]; then
			echo "file $f $(stat -c '%a %U:%G' "$f") $(sha256sum <"$f" | cut -c1-16)"
		else
			echo "file $f missing"
		fi
	done
	for u in $USERS; do
		home=$(getent passwd "$u" | cut -d: -f6)
		echo "user $u shell $(getent passwd "$u" | cut -d: -f7)"
		echo "user $u groups $(id -nG "$u" | tr ' ' '\n' | grep -vx "$u" | sort | paste -sd, -)"
		for f in $HOME_FILES; do
			if [ -e "$home/$f" ]; then
				echo "home $u ~/$f $(stat -c '%a %U:%G' "$home/$f") $(sha256sum <"$home/$f" | cut -c1-16)"
			else
				echo "home $u ~/$f missing"
			fi
		done
	done
} >"$out"
echo "    facts for $1 ($out):"
sed 's/^/    | /' "$out"
