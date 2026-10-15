#!/usr/bin/env bash
# install.sh: set sysbackup up on this machine (or install your edits to it). Run it as yourself:
#     bash /path/to/Toolkit/install.sh
# It asks for your password itself. Safe to re-run: it updates the scripts but never overwrites
# this machine's settings, bundle rules or password.
set -euo pipefail

say() { printf '\e[1;34m::\e[0m %s\n' "$*"; }
die() { printf '\e[1;31mxx\e[0m %s\n' "$*" >&2; exit 1; }

src=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
if [[ $EUID -ne 0 ]]; then
  # root can't read a user's FUSE mount (the Toolkit lived on one), so continue as root from a temporary copy
  tmp=$(mktemp -d /tmp/sysbackup-install.XXXXXX)
  cp -r "$src"/. "$tmp"/
  chmod -R go+rX "$tmp"
  exec sudo bash "$tmp/install.sh" "$@"
fi
[[ $src == /tmp/sysbackup-install.* ]] && trap 'rm -rf "$src"' EXIT
user=${SUDO_USER:-}
[[ -n $user && $user != root ]] || die "run it as yourself (not as root): bash install.sh"
home=$(getent passwd "$user" | cut -d: -f6)
LIB=/usr/local/lib/sysbackup CONF=/etc/sysbackup STATE=/var/lib/sysbackup

need=$(pacman -T restic rclone python fzf libnewt git || true)
if [[ -n $need ]]; then
  say "installing what's missing: $(echo $need)"
  pacman -S --needed $need || die "couldn't install them. Bring the system up to date first (sudo pacman -Syu), then run this again"
fi

say "installing the toolkit to $LIB"
if [[ $src != "$LIB" ]]; then
  install -d -m 755 "$LIB/systemd" "$LIB/pacman"
  install -m 755 "$src/sysbackup" "$src/rclone-as-user" "$src/install.sh" "$LIB/"
  install -m 644 "$src/README.md" "$src/excludes.txt" "$src/bundles.local" "$src/sysbackup.conf" "$LIB/"
  if [[ -f $src/AGENT-NOTES.md ]]; then install -m 644 "$src/AGENT-NOTES.md" "$LIB/"; fi   # notes for AI agents travel with the tool
  install -m 644 "$src"/systemd/* "$LIB/systemd/"
  install -m 644 "$src"/pacman/* "$LIB/pacman/"
fi
ln -sf "$LIB/sysbackup" /usr/local/bin/sysbackup

say "config in $CONF"
install -d -m 755 "$CONF"
install -d -m 700 "$STATE"
# every backup saves the machine's settings to machines/<host>/: reuse them on a reinstall
cfg=$src
host=$(cat /etc/hostname 2>/dev/null || hostname)
if [[ ! -f $CONF/sysbackup.conf ]]; then
  if [[ -d $src/machines/$host ]]; then
    cfg=$src/machines/$host
    say "reusing the saved settings of $host"
  elif compgen -G "$src/machines/*/sysbackup.conf" >/dev/null; then
    echo "Saved settings exist for: $(ls "$src/machines" | tr '\n' ' ')"
    read -rp "Start from one of them (type its name), or press Enter for the defaults: " m
    [[ -n $m && -d $src/machines/$m ]] && cfg=$src/machines/$m
  fi
fi
for f in sysbackup.conf excludes.txt bundles.local; do
  [[ -f $CONF/$f ]] && continue
  if [[ -f $cfg/$f ]]; then install -m 644 "$cfg/$f" "$CONF/$f"; else install -m 644 "$src/$f" "$CONF/$f"; fi
done
# new default excludes from this Toolkit version are added to your list; your own lines are never touched
new=$(grep -vxF -f <(sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$CONF/excludes.txt") \
        <(sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$src/excludes.txt") || true)
if [[ -n $new ]]; then
  printf '\n# added by sysbackup %s\n%s\n' "$(date -I)" "$new" >> "$CONF/excludes.txt"
  say "added $(wc -l <<<"$new") new default exclude(s) to $CONF/excludes.txt"
fi
sed -i "s/^SYSBACKUP_USER=.*/SYSBACKUP_USER=$user/" "$CONF/sysbackup.conf"
set -a; source "$CONF/sysbackup.conf"; set +a

remote=$(sed -n 's/^rclone:\([^:]*\):.*/\1/p' <<<"$RESTIC_REPOSITORY")
if [[ -n $remote ]] && ! runuser -u "$user" -- env HOME="$home" rclone listremotes | grep -qx "$remote:"; then
  die "rclone has no '$remote:' remote for $user yet. As $user (not root) run: rclone config
   n (new remote) -> name: $remote -> pick the storage type and sign in
   then run bash install.sh again"
fi
if [[ -n $remote ]] && ! err=$(runuser -u "$user" -- env HOME="$home" rclone lsf --max-depth 1 "$remote:" 2>&1 >/dev/null); then
  die "rclone can't reach $remote: ($(tail -n1 <<<"$err"))
   If the login expired, as $user run: rclone reconnect $remote:   then run bash install.sh again"
fi

pw=$RESTIC_PASSWORD_FILE
if [[ ! -s $pw ]]; then
  read -rp "Is there already a backup at $RESTIC_REPOSITORY (from another machine, or from this one before a reinstall)? [y/N] " a
  if [[ $a == [yY]* ]]; then
    read -rsp "Paste the backup password: " p; echo
    (umask 077; printf '%s\n' "$p" > "$pw")
  else
    (umask 077; head -c 48 /dev/urandom | base64 | tr -d '/+=\n' | head -c 40 > "$pw"; echo >> "$pw")
    echo
    echo "  Your backup password. Without it nobody can open the backup, including you:"
    echo
    echo "      $(cat "$pw")"
    echo
    echo "  Save it in your password manager now (e.g. as 'sysbackup restic password')."
  fi
fi
chmod 600 "$pw"
if [[ ! -f $STATE/password-confirmed ]]; then
  read -rp "Type 'saved' once the password is in your password manager (Enter to do it later; verify will remind you): " a
  [[ $a == saved ]] && date -Is > "$STATE/password-confirmed"
fi

say "checking the repository"
export RESTIC_REPOSITORY RESTIC_PASSWORD_FILE
ropt=(-o "rclone.program=$LIB/rclone-as-user")
if out=$(restic "${ropt[@]}" cat config 2>&1); then
  say "repository exists and the password opens it"
elif grep -qiE "wrong password|no key found" <<<"$out"; then
  die "that password doesn't open the existing repository at $RESTIC_REPOSITORY"
else
  say "creating the encrypted repository at $RESTIC_REPOSITORY"
  restic "${ropt[@]}" init
fi

fresh=n
if [[ ! -f $STATE/manifest/items.jsonl ]]; then
  read -rp "Is this a fresh install you're about to restore into from the backup? [y/N] " fresh
  # pause backups so the empty system never becomes the newest snapshot; restore resumes them
  [[ $fresh == [yY]* ]] && date -Is > "$STATE/restoring"
fi

say "installing timers and the pacman hook"
install -m 644 "$LIB"/systemd/* /etc/systemd/system/
install -d -m 755 /etc/pacman.d/hooks
install -m 644 "$LIB/pacman/zz-sysbackup.hook" /etc/pacman.d/hooks/
systemctl daemon-reload
systemctl enable --now sysbackup.timer sysbackup-verify.timer

if [[ $fresh == [yY]* ]]; then
  echo
  echo "Backups are paused until the restore is done. Next, ideally from a TTY (Ctrl+Alt+F3) while"
  echo "logged out of the desktop:   sudo sysbackup restore"
  exit 0
fi
read -rp "Run a backup now? [Y/n] " a
if [[ $a != [nN]* ]]; then
  # through the real services, so this also proves the nightly/weekly automatic runs work
  journalctl -f -n0 -o cat -u sysbackup.service -u sysbackup-verify.service &
  jpid=$!
  trap 'kill $jpid 2>/dev/null; [[ $src == /tmp/sysbackup-install.* ]] && rm -rf "$src"' EXIT
  ok=1
  say "backing up through sysbackup.service (the same way the automatic backups run)"
  systemctl start --wait sysbackup.service || ok=0
  if [[ $ok == 1 ]]; then
    say "deep check through sysbackup-verify.service (the weekly check)"
    systemctl start --wait sysbackup-verify.service || ok=0
  fi
  sleep 2; kill $jpid 2>/dev/null
  echo
  if [[ $ok == 1 ]]; then
    say "the automatic backup and check both work"
  else
    printf '\e[1;31mxx\e[0m %s\n' "an automatic run failed (see the lines above): sysbackup status shows the error" >&2
  fi
fi
cat <<EOF

Done. Backups run daily and an hour after your last package install/removal; a weekly deep check
notifies you if anything is wrong.
  sysbackup status            what was backed up when, recent system changes
  sudo sysbackup verify       quick health check (--deep: also test-restores from the repository)
  sysbackup bundles           how your setup is grouped for restore
  sudo sysbackup restore      pick & choose what to put back (on a fresh install or another machine)
EOF
