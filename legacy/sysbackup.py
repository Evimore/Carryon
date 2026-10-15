#!/usr/bin/env python3
"""sysbackup: record, back up, verify and selectively restore an Arch/CachyOS machine.

  sysbackup capture             record packages, configs, services, drivers... (runs automatically)
  sysbackup backup              capture + encrypted restic backup + retention + settings to iCloud
  sysbackup verify [--deep]     check the backup is current, complete and restorable (--deep: also
                                re-download a sample and test a restore; --full: re-download everything)
  sysbackup restore [options]   pick bundles from a backed-up machine and apply them here
  sysbackup bundles             show how the captured items are grouped
  sysbackup status              last backups per machine and recent system changes
  sysbackup restic ARGS...      any restic command on the backup (snapshots, ls, restore --include ...)

Everything is captured; deciding what applies to which machine happens at restore time.
"""
from __future__ import annotations

import argparse
import datetime as dt
import fnmatch
import glob
import grp
import hashlib
import json
import os
import pwd
import re
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from collections import Counter, defaultdict
from pathlib import Path

LIB_DIR = Path(__file__).resolve().parent
CONF_DIR = Path(os.environ.get("SYSBACKUP_CONF_DIR", "/etc/sysbackup"))
STATE_DIR = Path(os.environ.get("SYSBACKUP_STATE_DIR", "/var/lib/sysbackup"))
MANIFEST_DIR = STATE_DIR / "manifest"
RESTORING = STATE_DIR / "restoring"     # backups pause while this exists (fresh install mid-restore)
IS_ROOT = os.geteuid() == 0
os.environ.setdefault("HOME", pwd.getpwuid(os.getuid()).pw_dir)   # systemd services start without HOME
os.environ.setdefault("TERM", "linux")   # whiptail refuses to start without it
HOST = socket.gethostname()

# ------------------------------------------------------------------ config & small helpers

DEFAULTS = {
    "SYSBACKUP_USER": "",
    "RESTIC_REPOSITORY": "",                 # was the author's rclone remote
    "RESTIC_PASSWORD_FILE": str(CONF_DIR / "restic-password"),
    "TOOLKIT_MIRROR": "",                    # was the author's rclone remote
    "EXTRA_PATHS": "/root /srv /usr/local",
    "KEEP": "--keep-within 2d --keep-daily 7 --keep-weekly 4 --keep-monthly 6",
    "PRUNE_EVERY_DAYS": "7",
    "MAX_AGE_HOURS": "48",
    "AFTER_INSTALL_DELAY": "1h",
    "VERIFY_READ_SUBSET": "2%",
}


def load_conf() -> dict[str, str]:
    conf = dict(DEFAULTS)
    for path in (CONF_DIR / "sysbackup.conf", LIB_DIR / "sysbackup.conf"):
        try:
            text = path.read_text()
        except OSError:
            continue
        for line in text.splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            v = v.strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
                v = v[1:-1]
            conf[k.strip()] = v
        break
    return conf


CONF = load_conf()
USER = pwd.getpwnam(CONF["SYSBACKUP_USER"] or os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name)
HOME = Path(USER.pw_dir)

_err_tty, _out_tty = sys.stderr.isatty(), sys.stdout.isatty()


def paint(code: str, s: str, tty: bool = _out_tty) -> str:
    return f"\033[{code}m{s}\033[0m" if tty else s


def _stderr_is_journal() -> bool:
    """True only when stderr really is the journal (systemd sets JOURNAL_STREAM to its device:inode)."""
    try:
        st = os.fstat(2)
    except OSError:
        return False
    return os.environ.get("JOURNAL_STREAM", "") == f"{st.st_dev}:{st.st_ino}"


_journal = _stderr_is_journal()


def _emit(level: int, mark: str, color: str, msg: str) -> None:
    text = f"{paint(color, mark, _err_tty)} {msg}"
    if _journal:                                   # syslog level per line: 3 error, 4 warning, 6 info
        text = "\n".join(f"<{level}>{ln}" for ln in text.splitlines())
    print(text, file=sys.stderr, flush=True)


def log(msg: str) -> None:
    _emit(6, "::", "1;34", msg)


def warn(msg: str) -> None:
    _emit(4, "!!", "1;33", msg)


def die(msg: str) -> None:
    _emit(3, "xx", "1;31", msg)
    sys.exit(1)


def user_cmd(cmd: list[str]) -> list[str]:
    """Wrap cmd so it runs as the backed-up user (we usually run as root)."""
    if not IS_ROOT or USER.pw_uid == 0:
        return list(cmd)
    rt = f"/run/user/{USER.pw_uid}"
    return ["runuser", "-u", USER.pw_name, "--", "env", f"HOME={HOME}", f"USER={USER.pw_name}",
            f"XDG_RUNTIME_DIR={rt}", f"DBUS_SESSION_BUS_ADDRESS=unix:path={rt}/bus",
            f"PATH={HOME}/.cargo/bin:{HOME}/.local/bin:{os.environ.get('PATH', '/usr/bin')}", *cmd]


def sh(cmd: list[str], *, user: bool = False, check: bool = False, input: str | None = None,
       env: dict | None = None, timeout: int = 600) -> str:
    """Run quietly and return stdout (whatever it printed, even on failure, unless check)."""
    if user:
        cmd = user_cmd(cmd)
    try:
        r = subprocess.run(cmd, input=input, capture_output=True, env=env, timeout=timeout,
                           encoding="utf-8", errors="surrogateescape")
    except (FileNotFoundError, subprocess.TimeoutExpired) as e:
        if check:
            die(f"{cmd[0]}: {e}")
        return ""
    if check and r.returncode != 0:
        die(f"{shlex.join(cmd)} failed ({r.returncode}): {r.stderr.strip()[-600:]}")
    return r.stdout


def call(cmd: list[str], *, user: bool = False, env: dict | None = None) -> int:
    """Run attached to the terminal (pacman/paru prompts) and echo the command first."""
    if user:
        cmd = user_cmd(cmd)
    print(paint("2", "$ " + shlex.join(cmd), _err_tty), file=sys.stderr)
    return subprocess.run(cmd, env=env).returncode


def have(cmd: str) -> bool:
    return shutil.which(cmd) is not None


def need_root(what: str) -> None:
    if not IS_ROOT:
        die(f"{what} needs root: sudo sysbackup {what}")


def sha_file(p: Path | str) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def now_iso() -> str:
    return dt.datetime.now().astimezone().isoformat(timespec="seconds")


def parse_time(s: str) -> dt.datetime:
    s = re.sub(r"(\.\d{6})\d+", r"\1", s.strip()).replace("Z", "+00:00")
    return dt.datetime.fromisoformat(s)


def age_hours(t: dt.datetime) -> float:
    return (dt.datetime.now().astimezone() - t).total_seconds() / 3600


def brief(names: list[str], n: int = 8) -> str:
    names = list(names)
    return ", ".join(names[:n]) + (f" ... (+{len(names) - n} more)" if len(names) > n else "")


def item(type_: str, id_: str, **kw) -> dict:
    return {"type": type_, "id": id_, **kw}


def key(it: dict) -> str:
    return f"{it['type']}:{it['id']}"


def mountpoints() -> set[str]:
    pts = set()
    for line in Path("/proc/self/mounts").read_text().splitlines():
        f = line.split()
        if len(f) > 1:
            pts.add(f[1].encode().decode("unicode_escape"))
    return pts


def file_owners(paths) -> dict[str, str]:
    """{path: package} for the paths a package owns (keys are the paths as given)."""
    real = defaultdict(list)                 # pacman answers with resolved paths (/lib -> /usr/lib, dir/)
    for p in paths:
        if p:
            real[os.path.realpath(p)].append(p)
    queries, owners = list(real), {}
    for i in range(0, len(queries), 200):
        for line in sh(["pacman", "-Qo", *queries[i:i + 200]]).splitlines():
            m = re.match(r"(.+) is owned by (\S+) ", line)
            if m:
                for orig in real.get(os.path.realpath(m.group(1).rstrip("/")), []):
                    owners[orig] = m.group(2)
    return owners


# ------------------------------------------------------------------ restic

def restic_env() -> dict:
    if not have("restic"):
        die("restic isn't installed: sudo pacman -S restic")
    # explicit cache folder: restic gives up when it can't find one (e.g. no HOME under systemd)
    cache = os.environ.get("RESTIC_CACHE_DIR") or str(Path(os.environ["HOME"]) / ".cache/restic")
    return dict(os.environ, RESTIC_REPOSITORY=CONF["RESTIC_REPOSITORY"],
                RESTIC_PASSWORD_FILE=CONF["RESTIC_PASSWORD_FILE"], RESTIC_CACHE_DIR=cache)


def restic_cmd(args: list[str]) -> list[str]:
    cmd = ["restic", "--retry-lock", "30m"]
    if CONF["RESTIC_REPOSITORY"].startswith("rclone:"):
        # restic runs as root; rclone must run as you so it uses your iCloud login
        cmd += ["-o", f"rclone.program={LIB_DIR / 'rclone-as-user'}"]
    return cmd + args


def restic(args: list[str], *, quiet: bool = True) -> subprocess.CompletedProcess:
    if quiet:
        return subprocess.run(restic_cmd(args), env=restic_env(), capture_output=True,
                              encoding="utf-8", errors="surrogateescape")
    print(paint("2", "$ restic " + shlex.join(args), _err_tty), file=sys.stderr, flush=True)
    p = subprocess.Popen(restic_cmd(args), env=restic_env(), stderr=subprocess.PIPE, encoding="utf-8", errors="replace")
    tail: list[str] = []
    for line in p.stderr:                          # still shown live (terminal or journal) ...
        sys.stderr.write(line)
        sys.stderr.flush()
        tail = (tail + [line.rstrip("\n")])[-30:]  # ... and kept to explain a failure
    return subprocess.CompletedProcess(args, p.wait(), "", "\n".join(tail))


def restic_error(stderr: str) -> str:
    """The line that says what went wrong, plus what to do about it."""
    lines = [ln.strip() for ln in stderr.splitlines() if ln.strip()]
    cause = next((ln for ln in reversed(lines) if re.search(r"(?i)fatal|error|unable|fail|denied|wrong password|locked", ln)),
                 lines[-1] if lines else "restic gave no error output")
    cause = re.sub(r"^Fatal:\s*", "", cause)
    s = stderr.lower()
    if "wrong password" in s or "no key found" in s:
        hint = "the backup password doesn't match: compare /etc/sysbackup/restic-password with your password manager"
    elif "already locked" in s:
        hint = "another backup or check is still running; stale locks clear themselves (or: sudo sysbackup restic unlock)"
    elif re.search(r"rclone|icloud|unauthori[sz]ed|\b401\b|reauth", s):
        hint = "can't reach iCloud: if it signed you out, run (as yourself): rclone reconnect iCloud:"
    elif "no space left" in s:
        hint = "a disk is full"
    else:
        hint = "full log: journalctl -u sysbackup -e   (sysbackup status shows the last runs)"
    return f"{cause}\n   {hint}"


def last_error(unit: str) -> str:
    """Error lines from the most recent run of a systemd unit ('' if none)."""
    inv = sh(["systemctl", "show", "-p", "InvocationID", "--value", unit]).strip()
    if not inv:
        return ""
    out = sh(["journalctl", f"_SYSTEMD_INVOCATION_ID={inv}", "-p", "err", "-o", "cat", "--no-pager"]).strip()
    if not out:                                    # older runs logged without levels
        out = "\n".join(ln for ln in sh(["journalctl", f"_SYSTEMD_INVOCATION_ID={inv}", "-o", "cat", "--no-pager"]).splitlines()
                        if re.search(r"^xx |Fatal|unable to|error", ln))
    lines = [re.sub(r"^xx\s*", "", ln.strip()) for ln in out.splitlines() if ln.strip()]
    return "\n".join(lines[-3:])


def cloud_problem() -> str:
    """'' if the rclone remote behind the repository answers; otherwise rclone's own error."""
    repo = CONF["RESTIC_REPOSITORY"]
    if not repo.startswith("rclone:"):
        return ""
    remote = repo.split(":")[1] + ":"
    try:
        r = subprocess.run([str(LIB_DIR / "rclone-as-user"), "lsf", "--max-depth", "1", remote],
                           capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.TimeoutExpired) as e:
        return f"rclone couldn't be run: {e}"
    if r.returncode == 0:
        return ""
    lines = [ln for ln in r.stderr.strip().splitlines() if ln.strip()]
    return (lines[-1] if lines else f"rclone exited with {r.returncode}") + f"\n   If iCloud signed you out, run (as yourself): rclone reconnect {remote}"


def restic_snapshots(host: str | None = None) -> list[dict]:
    r = restic(["snapshots", "--json", *(["--host", host] if host else [])])
    if r.returncode != 0:
        die("can't read the backup repository: " + (r.stderr.strip().splitlines() or ["?"])[-1]
            + "\n   If iCloud signed you out, run: rclone reconnect iCloud:")
    snaps = json.loads(r.stdout or "[]")
    for s in snaps:
        s["_t"] = parse_time(s["time"])
    return sorted(snaps, key=lambda s: s["_t"])


# ------------------------------------------------------------------ hardware facts

def hw_ids() -> set[str]:
    """IDs describing this machine's hardware, for bundle `when` rules."""
    ids: set[str] = set()
    for dev in Path("/sys/bus/pci/devices").glob("*"):
        try:
            ids.add(f"pci:{(dev / 'vendor').read_text()[2:6]}:{(dev / 'device').read_text()[2:6]}")
        except OSError:
            pass
    for dev in Path("/sys/bus/usb/devices").glob("*"):
        try:
            ids.add(f"usb:{(dev / 'idVendor').read_text().strip()}:{(dev / 'idProduct').read_text().strip()}")
        except OSError:
            pass
    for line in sh(["bluetoothctl", "list"], timeout=5).splitlines():
        m = re.match(r"Controller ([0-9A-Fa-f:]{17})", line)
        if m:
            ids.add("bt:" + m.group(1).lower())
    mods = {line.split()[0] for line in Path("/proc/modules").read_text().splitlines()}
    for alias in [*Path("/sys/bus/pci/devices").glob("*/modalias"), *Path("/sys/bus/usb/devices").glob("*/modalias")]:
        try:
            mods.update(sh(["modprobe", "-R", alias.read_text().strip()]).split())
        except OSError:
            pass
    ids |= {"module:" + m.replace("-", "_") for m in mods}
    return {i.lower() for i in ids}


def devices_for_module(mod: str) -> set[str]:
    ids = set()
    for drv in (Path("/sys/module") / mod / "drivers").glob("*"):
        bus, _, name = drv.name.partition(":")
        for dev in (Path("/sys/bus") / bus / "drivers" / name).glob("*"):
            real = dev.resolve()
            for d in (real, real.parent):
                v = d / ("vendor" if bus == "pci" else "idVendor")
                p = d / ("device" if bus == "pci" else "idProduct")
                if v.is_file() and p.is_file():
                    ids.add(f"{bus}:{v.read_text().strip().removeprefix('0x')}:{p.read_text().strip().removeprefix('0x')}")
                    break
    return ids


def device_name(hwid: str) -> str:
    bus, _, vd = hwid.partition(":")
    if bus == "pci":
        try:
            f = shlex.split(sh(["lspci", "-mm", "-d", vd]).splitlines()[0])
            return re.sub(r"\s*\[[0-9a-f]{4}\]$", "", f[3])
        except (IndexError, ValueError):
            pass
    if bus == "usb":
        line = sh(["lsusb", "-d", vd]).strip()
        if line:
            return line.split(vd, 1)[-1].strip()
    return hwid


def pretty_id(i: str) -> str:
    kind, _, rest = i.partition(":")
    return {"pci": f"PCI {rest}", "usb": f"USB {rest}", "bt": f"Bluetooth adapter {rest}",
            "module": f"hardware driven by {rest}"}.get(kind, i)


def when_applies(rule: str, facts: dict, src: dict) -> tuple[bool, str]:
    """Does a bundle's when-rule hold on the target machine? `a+b` means both must hold."""
    for part in rule.strip().split("+"):
        ok, why = _when_one(part.strip(), facts, src)
        if not ok:
            return False, why
    return True, ""


def _when_one(rule: str, facts: dict, src: dict) -> tuple[bool, str]:
    if rule == "always":
        return True, ""
    if rule == "never":
        return False, "opt-in"
    if rule == "samehost":
        if src.get("machine") and facts.get("machine"):
            same = src["machine"] == facts["machine"]        # same motherboard, whatever it's called now
        else:
            same = src["host"] == facts["host"]
        return same, f"specific to {src['host']} (this is a different machine)"
    if rule.startswith("chassis:"):
        want = rule.split(":", 1)[1]
        return facts["chassis"] == want, f"{want}s only"
    if rule.startswith("hw:"):
        ids = [x.strip().lower() for x in rule[3:].split(",") if x.strip()]
        return any(x in facts["ids"] for x in ids), "needs " + " or ".join(pretty_id(x) for x in ids[:2]) + ", not found here"
    if rule.startswith("rootfs:"):
        want = rule.split(":", 1)[1]
        return facts["rootfs"] == want, f"needs a {want} root filesystem (this one is {facts['rootfs'] or 'unknown'})"
    if rule.startswith("bootloader:"):
        want = rule.split(":", 1)[1]
        return facts["bootloader"] == want, f"needs the {want} bootloader (this machine uses {facts['bootloader'] or 'unknown'})"
    warn(f"unknown when-rule {rule!r}, treating it as 'always'")
    return True, ""


def detect_bootloader() -> str:
    m = re.search(r"Product:\s*(\S+)", sh(["bootctl", "status", "--no-pager"], timeout=10))
    name = m.group(1).lower() if m else ""
    for b in ("limine", "systemd-boot", "grub", "refind"):
        if b in name:
            return b
    for path, b in (("/etc/default/limine", "limine"), ("/etc/default/grub", "grub"),
                    ("/etc/sdboot-manage.conf", "systemd-boot"), ("/boot/refind_linux.conf", "refind")):
        if Path(path).exists():
            return b
    return ""


def target_facts() -> dict:
    return {"host": HOST, "ids": hw_ids(), "chassis": sh(["hostnamectl", "chassis"]).strip(),
            "rootfs": sh(["findmnt", "-no", "FSTYPE", "/"]).strip(), "bootloader": detect_bootloader(),
            "machine": machine_fingerprint()}


# ------------------------------------------------------------------ capture

STANDARD_REPOS = re.compile(r"^((core|extra|multilib)(-testing)?|(gnome|kde)-unstable|cachyos.*)$")
NOT_CAPTURED = re.compile("|".join([
    r"^/etc/(machine-id|adjtime|ld\.so\.cache|\.pwd\.lock|\.updated|mtab|resolv\.conf)$",
    r"^/etc/(passwd|group|shadow|gshadow|subuid|subgid)-?$",
    r"^/etc/(ca-certificates/extracted|ssl/certs|pacman\.d/gnupg|fonts/conf\.d)/",
    r"^/etc/ssh/ssh_host_[^/]*_key(\.pub)?$",       # identity: restored from the encrypted backup
    r"^/etc/NetworkManager/system-connections/",    # wifi passwords: same
    r"^/etc/(cachyos/installation-snapshot|os-release)$",
    # the toolkit itself: install.sh puts it on each machine (it's still in every backup)
    r"^/etc/sysbackup/", r"^/usr/local/(lib/sysbackup/|bin/sysbackup$)",
    r"^/etc/pacman\.d/hooks/zz-sysbackup\.hook$", r"^/etc/systemd/system/sysbackup",
    r"^/usr/lib/modules/", r"^/usr/share/mime/", r"/__pycache__/", r"\.pyc$",
    r"\.(pacnew|pacsave|pacorig|pre-restore|bak|old|orig)$", r"mirrorlist$", r"\.cache$",
    r"^/usr/lib/locale/locale-archive$", r"^/(etc|usr/lib)/udev/hwdb\.bin$",
    r"^/usr/share/glib-2\.0/schemas/gschemas\.compiled$", r"^/usr/share/info/dir$",
    r"^/usr/share/vim/vimfiles/doc/tags$", r"^/etc/xml/catalog$",
]))
HANDLED_ELSEWHERE = {"/etc/passwd", "/etc/group", "/etc/shadow", "/etc/gshadow", "/etc/subuid",
                     "/etc/subgid", "/etc/shells", "/etc/pacman.conf", "/etc/resolv.conf"}
REVIEW_ONLY = re.compile(r"^/etc/(fstab|crypttab|default/limine|kernel/cmdline|default/grub|sdboot-manage\.conf)$")
ROOTISH = re.compile(r"^(BOOT_IMAGE|initrd|root|rootflags|rootfstype|cryptdevice|cryptkey|resume|"
                     r"resume_offset|rd\.luks\.[a-z.]+|rw|ro)(=|$)")
# always backed up (restore relies on them), whatever EXTRA_PATHS says
ESSENTIAL_PATHS = ("/etc", "/var/lib/bluetooth", "/var/lib/tailscale", "/var/lib/fprint")
UNIT_SUFFIX = re.compile(r"\.(service|socket|timer|path|mount|automount|target|slice)$")
MAX_FILE = 100 * 1024 * 1024


def capture_packages(ctx: dict) -> list[dict]:
    repo_of: dict[str, str] = {}
    for line in sh(["pacman", "-Sl"]).splitlines():
        f = line.split()
        if len(f) >= 2:
            repo_of.setdefault(f[1], f[0])          # first repo listed wins, like pacman
    ctx["repo_of"] = repo_of
    items = [item("pkg", p, repo=repo_of.get(p, "?")) for p in sh(["pacman", "-Qqen"]).split()]
    items += [item("aur", p) for p in sh(["pacman", "-Qqem"]).split()]
    # installed as dependencies but only optionally needed: reinstalling the rest wouldn't bring them back.
    # True leftovers (nothing uses them, e.g. build tools) are not recorded.
    orphans = set(sh(["pacman", "-Qqdt"]).split())
    items += [item("pkg", p, repo=repo_of.get(p, "?"), asdeps=True) for p in sh(["pacman", "-Qqdttn"]).split() if p not in orphans]
    items += [item("aur", p, asdeps=True) for p in sh(["pacman", "-Qqdttm"]).split() if p not in orphans]
    return items


def parse_gpg_keys(listing: str) -> list[tuple[str, str]]:
    keys, state, fpr = [], None, None
    for line in listing.splitlines():
        f = line.split(":")
        if f[0] == "pub":
            state, fpr = "pub", None
        elif f[0] == "fpr" and state == "pub" and fpr is None:
            fpr = f[9]
        elif f[0] == "uid" and state == "pub" and fpr:
            keys.append((fpr, f[9]))
            state = None
    return keys


def capture_repos(ctx: dict, stage: Path, tmp: Path) -> list[dict]:
    custom = [r for r in sh(["pacman-conf", "--repo-list"]).split() if not STANDARD_REPOS.match(r)]
    ctx["repo_files"] = set()
    if not custom:
        return []
    defs: dict[str, tuple[Path, str]] = {}
    for f in [Path("/etc/pacman.conf"), *sorted(Path("/etc/pacman.d").glob("*.conf"))]:
        try:
            text = f.read_text(errors="replace")
        except OSError:
            continue
        sec, buf = None, defaultdict(list)
        for line in text.splitlines():
            m = re.match(r"\s*\[([^\]]+)\]", line)
            if m:
                sec = m.group(1)
            if sec in custom:
                buf[sec].append(line)
        for r, lines in buf.items():
            defs.setdefault(r, (f, "\n".join(lines).strip() + "\n"))
    # signing keys you added by hand = pacman keyring minus the distro keyrings
    gpg = ["gpg", "--homedir", "/etc/pacman.d/gnupg", "--no-permission-warning", "--batch", "--no-auto-check-trustdb"]
    mine = parse_gpg_keys(sh(gpg + ["--with-colons", "--list-keys"]))
    scratch = tmp / "gnupg"
    scratch.mkdir(mode=0o700)
    distro = set()
    for kr in Path("/usr/share/pacman/keyrings").glob("*.gpg"):
        distro |= {f for f, _ in parse_gpg_keys(sh(["gpg", "--homedir", str(scratch), "--batch", "--with-colons", "--show-keys", str(kr)]))}
    extra = [(f, u) for f, u in mine if f not in distro and "pacman@localhost" not in u]
    if extra:
        (stage / "repo-keys.asc").write_text(sh(gpg + ["--armor", "--export", *[f for f, _ in extra]]))
    items = []
    for r in custom:
        f, section = defs.get(r, (Path("/etc/pacman.conf"), ""))
        conf = str(f) if f != Path("/etc/pacman.conf") else ""
        if conf:
            ctx["repo_files"].add(conf)
        items.append(item("repo", r, conf=conf, section=section, keys=[f for f, _ in extra],
                          key_names=[u for _, u in extra]))
    return items


def capture_files(stage: Path, ctx: dict) -> list[dict]:
    owned = {p.rstrip("/") for p in sh(["pacman", "-Qlq"]).splitlines()}
    modified = re.findall(r"(/\S+) \[modified\]$", sh(["pacman", "-Qii"]), re.M)
    owner_of = file_owners(modified)
    found = sh(["find", "/etc", "/usr", "/opt", "-xdev", "(", "-type", "f", "-o", "-type", "l", ")", "-print0"]).split("\0")
    unowned = [p for p in found if p and p not in owned
               and not (os.path.islink(p) and p.startswith(("/etc/systemd/", "/etc/xdg/systemd/")))]
    items, skipped = [], []
    for kind, paths in (("modified", modified), ("unowned", unowned)):
        for p in paths:
            if NOT_CAPTURED.search(p) or p in HANDLED_ELSEWHERE or p in ctx["repo_files"]:
                continue
            try:
                st = os.lstat(p)
            except OSError:
                continue
            it = item("file", p, kind=kind, mode=f"{st.st_mode & 0o7777:o}", owner=f"{_uname(st.st_uid)}:{_gname(st.st_gid)}")
            if kind == "modified":
                it["pkg"] = owner_of.get(p, "")
            dest = stage / "files" / p.lstrip("/")
            dest.parent.mkdir(parents=True, exist_ok=True)
            if stat.S_ISLNK(st.st_mode):
                it["link"] = os.readlink(p)
                os.symlink(it["link"], dest)
            elif st.st_size > MAX_FILE:
                it["too_big"] = True
                warn(f"not copying {p} ({st.st_size >> 20} MiB); restore it by hand if you need it")
            else:
                try:
                    data = Path(p).read_bytes()
                except OSError:
                    skipped.append(p)
                    continue
                it["sha"] = hashlib.sha256(data).hexdigest()
                dest.write_bytes(data)
                os.chmod(dest, st.st_mode & 0o7777)
            items.append(it)
    ctx["skipped"] = skipped
    return items


def _uname(uid: int) -> str:
    try:
        return pwd.getpwuid(uid).pw_name
    except KeyError:
        return str(uid)


def _gname(gid: int) -> str:
    try:
        return grp.getgrgid(gid).gr_name
    except KeyError:
        return str(gid)


def unit_props(units: list[str]) -> dict[str, dict[str, str]]:
    if not units:
        return {}
    blocks = sh(["systemctl", "show", "-p", "LoadState", "-p", "FragmentPath", "--", *units]).strip().split("\n\n")
    return {u: dict(ln.split("=", 1) for ln in b.splitlines() if "=" in ln) for u, b in zip(units, blocks)}


def capture_services(stage: Path) -> list[dict]:
    enabled, masked = [], []
    for line in sh(["systemctl", "list-unit-files", "--no-legend", "--no-pager", "--state=enabled,masked"]).splitlines():
        f = line.split()
        if len(f) < 2 or f[0].startswith("sysbackup") or "@." in f[0]:
            continue
        (masked if f[1] == "masked" else enabled).append(f[0])
    for link in Path("/etc/systemd/system").glob("*.wants/*@?*"):  # enabled template instances
        if "@." not in link.name:
            enabled.append(link.name)
    props = unit_props(sorted(set(enabled)))
    # an enable link whose unit no longer exists (package removed) is a leftover, not something to restore
    enabled = [u for u, p in props.items() if p.get("LoadState") != "not-found"]
    owners = file_owners(props[u].get("FragmentPath", "") for u in enabled)
    items = [item("svc", u, unit_file=props[u].get("FragmentPath", ""), pkg=owners.get(props[u].get("FragmentPath", ""), ""))
             for u in enabled]
    items += [item("mask", u) for u in sorted(set(masked))]

    ud = HOME / ".config/systemd/user"
    if ud.is_dir():
        for name in sorted({p.name for p in [*ud.glob("*.wants/*"), *ud.glob("*.requires/*")]}):
            items.append(item("usvc", name))
        (stage / "user-units").mkdir(exist_ok=True)
        for p in sorted(ud.iterdir()):
            if p.is_symlink() and os.readlink(p) == "/dev/null":
                items.append(item("umask", p.name))
        for p in sorted(ud.rglob("*")):     # unit files and drop-ins (foo.service.d/override.conf)
            if p.is_file() and not p.is_symlink() and not p.parent.name.endswith((".wants", ".requires")):
                rel = str(p.relative_to(ud))
                (stage / "user-units" / rel).parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(p, stage / "user-units" / rel)
                items.append(item("uunit", rel, sha=sha_file(p)))
    if (Path("/var/lib/systemd/linger") / USER.pw_name).exists():
        items.append(item("linger", USER.pw_name))
    return items


CMDLINE_SOURCES = [  # (config file, regex: (prefix)(parameters), command that applies it)
    (Path("/etc/default/limine"), r'^(\s*KERNEL_CMDLINE\[default\]\+?=["\'])([^"\']*)', ["limine-update"]),
    (Path("/etc/sdboot-manage.conf"), r'^(\s*LINUX_OPTIONS=["\'])([^"\']*)', ["sdboot-manage", "gen"]),
    (Path("/etc/kernel/cmdline"), r"^()(.*)$", ["mkinitcpio", "-P"]),
    (Path("/etc/default/grub"), r'^(\s*GRUB_CMDLINE_LINUX(?:_DEFAULT)?=["\'])([^"\']*)', ["grub-mkconfig", "-o", "/boot/grub/grub.cfg"]),
]


def unit_needs(items: list[dict], stage: Path, ctx: dict) -> list[dict]:
    """What your hand-made units need that a reinstall wouldn't bring back by itself: packages they
    run that only came in as dependencies (fuse3, libnotify...) and folders they use (mount points, log dirs)."""
    texts = {}
    for it in items:
        if it["type"] == "svc" and not it.get("pkg") and it.get("unit_file"):
            try:
                texts[it["id"]] = Path(it["unit_file"]).read_text(errors="replace")
            except OSError:
                pass
        elif it["type"] == "uunit":
            texts[it["id"]] = (stage / "user-units" / it["id"]).read_text(errors="replace")
    progs: dict[str, str] = {}
    dirs: dict[str, str] = {}
    for unit, text in texts.items():
        for m in re.finditer(r"^\s*Exec[A-Za-z]*=[-@:+!|]*(.+)$", text, re.M):
            args = m.group(1).split()
            if not args:
                continue
            progs.setdefault(args[0].replace("%h", str(HOME)), unit)
            if Path(args[0]).name == "rclone" and len(args) > 3 and args[1] == "mount":
                dirs.setdefault(args[3], unit)                    # rclone mount REMOTE: MOUNTPOINT
            for n, arg in enumerate(args):
                if arg.startswith("--log-file="):
                    dirs.setdefault(str(Path(arg.split("=", 1)[1]).parent), unit)
                elif arg == "--log-file" and n + 1 < len(args):
                    dirs.setdefault(str(Path(args[n + 1]).parent), unit)
    have_pkgs = {i["id"] for i in items if i["type"] in ("pkg", "aur")}
    owners = file_owners([p for p in progs if p.startswith("/")])
    out, seen = [], set()
    for path, unit in progs.items():
        pkg = owners.get(path)
        if pkg and pkg not in have_pkgs and pkg not in seen:
            seen.add(pkg)
            if pkg in ctx["repo_of"]:
                out.append(item("pkg", pkg, repo=ctx["repo_of"][pkg], asdeps=True, for_unit=unit))
            else:
                out.append(item("aur", pkg, asdeps=True, for_unit=unit))
    for d, unit in dirs.items():
        d = d.replace("%h", str(HOME))
        if d.startswith(str(HOME) + "/"):
            out.append(item("mkdir", d, rel=d.removeprefix(str(HOME) + "/"), for_unit=unit))
        elif d.startswith("/"):
            out.append(item("mkdir", d, for_unit=unit))
    return out


def machine_fingerprint() -> str:
    """Identifies the hardware across reinstalls and renames (needs root; '' otherwise)."""
    parts = []
    for f in ("product_uuid", "board_serial", "board_name"):
        try:
            parts.append((Path("/sys/class/dmi/id") / f).read_text().strip())
        except OSError:
            return ""
    return hashlib.sha256("|".join(parts).encode()).hexdigest()[:16]


def cmdline_config() -> tuple[Path | None, list[str]]:
    """Where the persistent kernel command line lives, and its parameters."""
    for path, rx, _ in CMDLINE_SOURCES:
        if path.exists():
            return path, [x for m in re.finditer(rx, path.read_text(), re.M) for x in m.group(2).split()]
    return None, Path("/proc/cmdline").read_text().split()


def capture_kernel() -> list[dict]:
    src, params = cmdline_config()
    return [item("kparam", p, source=str(src)) for p in dict.fromkeys(params) if not ROOTISH.match(p)]


def capture_account() -> list[dict]:
    items = [item("group", g.gr_name) for g in grp.getgrall() if USER.pw_name in g.gr_mem]
    items.append(item("shell", USER.pw_shell))
    return items


def capture_tools() -> list[dict]:
    items = []
    if have("cargo"):
        for line in sh(["cargo", "install", "--list"], user=True).splitlines():
            m = re.match(r"^(\S+) v\S+", line)
            if m:
                items.append(item("cargo", m.group(1)))
    if have("pipx"):
        items += [item("pipx", line.split()[0]) for line in sh(["pipx", "list", "--short"], user=True).splitlines() if line.strip()]
    if have("npm"):
        for line in sh(["npm", "ls", "-g", "--depth=0", "--parseable"]).splitlines()[1:]:
            name = re.sub(r"^.*/node_modules/", "", line)
            if name not in ("npm", "corepack"):
                items.append(item("npm", name))
    if have("flatpak"):
        for line in sh(["flatpak", "list", "--app", "--columns=application,origin,installation"], user=True).splitlines():
            f = line.split("\t")
            if len(f) >= 2:
                items.append(item("flatpak", f[0], origin=f[1], scope=f[2] if len(f) > 2 else "system"))
    return items


def load_excludes() -> list[str]:
    for p in (CONF_DIR / "excludes.txt", LIB_DIR / "excludes.txt"):
        try:
            return [ln.strip() for ln in p.read_text().splitlines() if ln.strip() and not ln.lstrip().startswith("#")]
        except OSError:
            continue
    return []


def excluded(path: str, pats: list[str]) -> bool:
    return any(fnmatch.fnmatch(path, p.replace("**", "*")) for p in pats)


def sys_path_item(p: str, **kw) -> dict:
    """A system folder restored from the backup, with its owner (service users get other numbers elsewhere)."""
    try:
        st = os.stat(p)
        return item("path", p, owner=f"{_uname(st.st_uid)}:{_gname(st.st_gid)}", uid=st.st_uid, gid=st.st_gid, **kw)
    except OSError:
        return item("path", p, **kw)


def capture_paths(ctx: dict) -> list[dict]:
    """Things restored straight from the encrypted backup: secrets, your home directory, extra folders."""
    items = []
    for p in ("/etc/NetworkManager/system-connections", "/var/lib/bluetooth", "/var/lib/tailscale", "/var/lib/fprint", "/root"):
        if Path(p).exists():
            items.append(sys_path_item(p))
    if any(Path("/etc/ssh").glob("ssh_host_*")):
        items.append(item("path", "/etc/ssh/ssh_host_*"))
    try:
        if any(Path("/srv").iterdir()):
            items.append(sys_path_item("/srv"))
    except OSError:
        pass
    covered = {i["id"] for i in items}
    for p in CONF["EXTRA_PATHS"].split():         # folders you added (e.g. /var/lib/openlinkhub)
        if (p in covered or p in ("/etc", "/usr/local") or p.startswith(("/etc/", "/usr/", "/opt/"))
                or p == str(HOME) or p.startswith(str(HOME) + "/") or not Path(p).exists()):
            continue                              # /etc, /usr, /opt are recorded file by file; home has its own items
        items.append(sys_path_item(p, extra=True))
    try:
        ctx["bt_macs"] = [d.name.lower() for d in Path("/var/lib/bluetooth").iterdir() if re.fullmatch(r"[0-9A-Fa-f:]{17}", d.name)]
    except OSError:
        ctx["bt_macs"] = []

    pats, mounts = load_excludes(), mountpoints()
    cand = [e for e in HOME.iterdir() if e.name not in (".config", ".local")]
    for sub in (".config", ".local", ".local/share", ".config/autostart"):   # autostart: one item per app
        d = HOME / sub
        if d.is_dir():
            cand += [e for e in d.iterdir() if not (sub == ".local" and e.name == "share")
                     and not (sub == ".config" and e.name == "autostart")]
    for e in sorted(cand):
        # user units are restored through their own bundles (so unticking e.g. iCloud really skips it)
        if str(e) in mounts or excluded(str(e), pats) or e == HOME / ".config/systemd":
            continue
        items.append(item("path", str(e), home=str(e.relative_to(HOME))))
    return items


# ------------------------------------------------------------------ bundles

BUILTIN = {  # id: (title, default when-rule)
    "packages": ("Packages from the official & CachyOS repos", "always"),
    "aur": ("AUR packages (rebuilt with paru)", "always"),
    "repos": ("Extra package repositories", "always"),
    "services": ("Enabled system services", "always"),
    "user-services": ("Enabled user services", "always"),
    "configs": ("Tweaked package settings", "always"),
    "other-files": ("Other hand-installed system files", "always"),
    "boot": ("Initramfs & kernel module settings", "samehost"),
    "kparams": ("Kernel parameters", "always"),
    "locale": ("Locale, keyboard & timezone", "always"),
    "account": ("Group memberships & login shell", "always"),
    "login": ("Login screen & automatic login", "samehost"),
    "fingerprint": ("Fingerprint login: reader driver, fingerprint for sudo/polkit, enrolled prints", "samehost"),
    "xkb": ("Keyboard tweaks (/etc/xkb)", "always"),
    "boot-limine": ("Limine bootloader tools & snapshot boot entries", "bootloader:limine"),
    "boot-grub": ("GRUB bootloader tools", "bootloader:grub"),
    "snapshots": ("Btrfs snapshots (snapper)", "rootfs:btrfs"),
    "dev-tools": ("Language tools (cargo, pipx, npm, flatpak)", "always"),
    "network": ("Wi-Fi & VPN connections (with passwords)", "always"),
    "bluetooth": ("Bluetooth pairings", "samehost"),
    "identity": ("Machine identity: hostname, SSH host keys, Tailscale node", "samehost"),
    "root-home": ("root's home directory", "samehost"),
    "srv": ("/srv", "samehost"),
    "other-paths": ("Other backed-up system paths", "samehost"),
    "home-keys": ("Home: SSH & GPG keys", "always"),
    "home-saves": ("Home: Steam data & Proton game saves", "always"),
    "home-files": ("Home: your files (Documents, Pictures, Projects...)", "always"),
    "home-apps": ("Home: app settings & data (dotfiles)", "always"),
    "home-display": ("Home: monitor layout", "samehost"),
    "disk-layout": ("Disk layout & bootloader config (shown as a diff, never copied)", "samehost"),
}
ORDER = ["repos", "packages", "aur", "*apps", "services", "user-services", "configs", "other-files", "*hw",
         "boot-limine", "boot-grub", "snapshots", "boot", "kparams", "locale", "xkb", "account", "login", "fingerprint",
         "dev-tools", "network", "bluetooth", "identity", "root-home",
         "srv", "other-paths", "home-keys", "home-saves", "home-files", "home-apps", "home-display", "disk-layout"]


def rank(b: str) -> tuple[int, str]:
    if b in ORDER:
        return ORDER.index(b), b
    if b.startswith(("kparam-", "firmware-")):
        return ORDER.index("*hw"), b
    return ORDER.index("*apps"), b


def early_rules() -> list[tuple[str, str, str]]:
    h = re.escape(str(HOME))
    return [
        ("disk-layout", "file", REVIEW_ONLY.pattern),
        ("locale", "file", r"^/etc/(locale\.(conf|gen)|vconsole\.conf|timezone|localtime|default/(keyboard|locale)"
                           r"|X11/xorg\.conf\.d/00-keyboard\.conf)$"),
        ("identity", "file", r"^/etc/(hostname|hosts|machine-info)$"),
        ("identity", "path", r"^/(etc/ssh/ssh_host_\*|var/lib/tailscale)$"),
        ("root-home", "path", r"^/root$"),
        ("network", "path", r"^/etc/NetworkManager/system-connections$"),
        ("bluetooth", "path", r"^/var/lib/bluetooth$"),
        ("srv", "path", r"^/srv$"),
        ("home-keys", "path", rf"^{h}/\.(ssh|gnupg|pki|password-store)$"),
        ("home-saves", "path", rf"^{h}/\.local/share/Steam$"),
        ("home-display", "path", rf"^{h}/(\.config/(kwinoutputconfig\.json|monitors\.xml)|\.local/share/kscreen)$"),
        ("home-files", "path", rf"^{h}/[^./][^/]*$"),
        ("fingerprint", "path", r"^/var/lib/fprint$"),
        ("fingerprint", "pkg", r"^(fprintd|libfprint.*)$"),
        ("fingerprint", "aur", r"(fprint|fingerprint)"),
        ("xkb", "file", r"^/etc/xkb/"),
        # autologin settings would let anyone in on another machine
        ("login", "file", r"^/etc/(plasmalogin\.conf|sddm\.conf(\.d/.*)?|lightdm/lightdm\.conf|gdm/custom\.conf|ly/config\.ini|greetd/config\.toml)$"),
        ("login", "group", r"^(nopasswdlogin|autologin)$"),
        # bootloader / filesystem specific: only where that bootloader / btrfs is in use
        ("boot-limine", "pkg", r"^limine(-.*)?$"),
        ("boot-limine", "file", r"^/etc/(limine-snapper-sync\.conf|limine-entry-tool\.conf|mkinitcpio\.conf\.d/[^/]*limine[^/]*)$"),
        ("boot-limine", "svc", r"^limine-.*\.service$"),
        ("boot-grub", "pkg", r"^(grub|grub-.*)$"),
        ("snapshots", "pkg", r"^(snapper|snap-pac|btrfs-assistant|cachyos-snapper-support|timeshift(-autosnap)?)$"),
        ("snapshots", "file", r"^/etc/(snapper/.*|conf\.d/snapper)$"),
        ("snapshots", "svc", r"^(snapper-.*|grub-btrfsd)\.(service|timer)$"),
        # initramfs hooks differ between machines (encryption!), so these default to this machine only
        ("boot", "file", r"^/etc/(mkinitcpio\.conf(\.d/.*)?|mkinitcpio\.d/.*|modprobe\.d/.*|modules-load\.d/.*|dracut\.conf(\.d/.*)?)$"),
    ]


FALLBACK = {"pkg": "packages", "aur": "aur", "repo": "repos", "svc": "services", "mask": "services",
            "usvc": "user-services", "uunit": "user-services", "umask": "user-services", "linger": "user-services",
            "kparam": "kparams", "group": "account", "shell": "account", "mkdir": "user-services",
            "cargo": "dev-tools", "pipx": "dev-tools", "npm": "dev-tools", "flatpak": "dev-tools"}


def read_local_rules():
    titles, whens, puts, drops = {}, {}, [], []
    for p in (CONF_DIR / "bundles.local", LIB_DIR / "bundles.local"):
        try:
            text = p.read_text()
        except OSError:
            continue
        for n, raw in enumerate(text.splitlines(), 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            f = line.split()
            if f[0] == "title" and len(f) >= 3:
                titles[f[1]] = line.split(None, 2)[2]
            elif f[0] == "when" and len(f) == 3:
                whens[f[1]] = f[2]
            elif f[0] == "put" and len(f) == 4:
                puts.append((f[1], f[2], f[3]))
            elif f[0] == "drop" and len(f) == 3:
                drops.append((f[1], f[2]))
            else:
                warn(f"{p}:{n}: can't parse: {line}")
        break
    return titles, whens, puts, drops


def glob_hit(it: dict, typ: str, pat: str) -> bool:
    if typ not in ("*", it["type"]):
        return False
    names = [it["id"]] + (["~/" + it["home"]] if "home" in it else [])
    return any(fnmatch.fnmatchcase(n, pat) for n in names)


def stem_of(name: str) -> str:
    base = re.sub(r"-(bin|git|appimage)$", "", UNIT_SUFFIX.sub("", name)).lower()
    head = base.split("-")[0]
    return head if len(head) >= 4 else base


def stem_matches(it: dict, s: str, repo_of: dict) -> bool:
    t, i = it["type"], it["id"].lower()
    if t in ("pkg", "aur"):
        return i == s or i.startswith(s + "-") or repo_of.get(it["id"]) == s
    if t == "repo":
        return i == s
    if t in ("svc", "usvc", "uunit", "mask", "umask"):
        return i.startswith(s)
    if t == "file":
        return s in i
    if t == "path" and ("home" in it or it.get("extra")):
        return s in Path(it.get("home") or it["id"]).name.lower()
    return False


def firmware_when(paths: list[str]) -> str:
    wanted = {re.sub(r"\.(zst|xz)$", "", p.split("/usr/lib/firmware/", 1)[1]) for p in paths}
    mods = set()
    for m in {ln.split()[0] for ln in Path("/proc/modules").read_text().splitlines()}:
        if wanted & set(sh(["modinfo", "-F", "firmware", m]).split()):
            mods.add(m)
    devs = set()
    for m in mods:
        # helper modules (btmtk) drive no device themselves; the module using them (btusb) does
        users = [m, *(h.name for h in (Path("/sys/module") / m / "holders").glob("*"))]
        found = set().union(*(devices_for_module(u) for u in users))
        devs |= found if len(found) <= 3 else set()   # a generic driver bound to lots of devices says nothing
    return "hw:" + ",".join(sorted(devs)) if devs else "samehost"


def assign_bundles(items: list[dict], ctx: dict) -> dict:
    titles, whens, puts, drops = read_local_rules()
    auto_titles, auto_whens = {}, {}
    items[:] = [it for it in items if not any(glob_hit(it, t, p) for t, p in drops)]

    for it in items:                                    # 1. your own rules (bundles.local)
        for b, t, p in puts:
            if glob_hit(it, t, p):
                it["bundle"] = b
                break

    # 2. app bundles: whatever shares a name with a hand-made unit, an extra repo or an AUR package
    anchors = {stem_of(i["id"]) for i in items
               if (i["type"] == "svc" and not i.get("pkg")) or i["type"] == "repo" or (i["type"] == "uunit" and "/" not in i["id"])}
    stems = {s for s in anchors | {stem_of(i["id"]) for i in items if i["type"] == "aur"} if len(s) >= 4}
    for it in items:
        if "bundle" not in it:
            hits = [s for s in stems if stem_matches(it, s, ctx["repo_of"])]
            if hits:
                it["bundle"] = max(hits, key=len)
    counts = Counter(i.get("bundle") for i in items)
    for it in items:
        if it.get("bundle") in stems and it["bundle"] not in anchors and counts[it["bundle"]] < 2:
            del it["bundle"]

    def apply_rules(rules):
        for it in items:
            if "bundle" in it:
                continue
            for b, t, rx in rules:
                if it["type"] == t and re.search(rx, it["id"]):
                    it["bundle"] = b
                    break

    for it in items:                                    # fingerprint lines in PAM (sudo, polkit...) go with the reader
        if "bundle" not in it and it["type"] == "file" and it["id"].startswith("/etc/pam.d/"):
            try:
                if "pam_fprintd" in (ctx["stage"] / "files" / it["id"].lstrip("/")).read_text(errors="replace"):
                    it["bundle"] = "fingerprint"
            except OSError:
                pass
    apply_rules(early_rules())                          # 3. well-known places
    if ctx.get("fp_readers"):
        auto_whens["fingerprint"] = "hw:" + ",".join(ctx["fp_readers"])
    xkb = sorted(Path(i["id"]).name for i in items if i.get("bundle") == "xkb" and "/symbols/" in i["id"])
    if xkb:
        auto_titles["xkb"] = f"Keyboard tweaks (/etc/xkb): {', '.join(xkb)}"

    groups = defaultdict(list)                          # 4. a package's edited configs (+ its service)
    for it in items:
        if "bundle" not in it and it["type"] == "file" and it.get("kind") == "modified" and it.get("pkg"):
            groups[it["pkg"]].append(it)
    for pkg, its in groups.items():
        rx = re.compile(rf"(^|[/_-]){re.escape(pkg)}([/._-]|$)")
        extra = [i for i in items if "bundle" not in i and i not in its and (
            (i["type"] == "svc" and i.get("pkg") == pkg)
            or (i["type"] == "file" and i.get("kind") == "unowned" and rx.search(i["id"])))]
        if len(its) + len(extra) >= 2:
            for i in its + extra:
                i["bundle"] = "cfg-" + pkg
            auto_titles["cfg-" + pkg] = f"{pkg} settings" + (" + service" if any(i["type"] == "svc" for i in extra) else "")

    # 4b. apps: a settings folder named like an installed package (~/.config/OpenRGB <-> openrgb) forms a bundle
    #     with that package and its autostart entry
    explicit = {app_key(i["id"]): i for i in items if i["type"] in ("pkg", "aur") and not i.get("asdeps")}
    by_name = {i["id"]: i for i in explicit.values()}
    for autostart in (False, True):
        for it in items:
            rel = it.get("home", "")
            if "bundle" in it or it["type"] != "path" or rel.startswith(".config/autostart/") != autostart:
                continue
            if not autostart and not rel.startswith((".config/", ".local/share/")):
                continue
            k = app_key(Path(rel).stem if autostart else Path(rel).name)
            pkg = explicit.get(k) if len(k) >= 3 else None
            if pkg is None and autostart:                 # e.g. com.example.App.desktop -> its package
                pkg = by_name.get(autostart_owner(it["id"]))
            if pkg is None:
                continue
            if "bundle" not in pkg:
                pkg["bundle"] = "app-" + pkg["id"]
                auto_titles[pkg["bundle"]] = f"{pkg['id'] if autostart else Path(rel).name} (app + {'autostart' if autostart else 'settings'})"
            it["bundle"] = pkg["bundle"]

    for it in items:                                    # added system folders named like a package (/var/lib/foo <-> foo)
        if "bundle" not in it and it.get("extra"):
            pkg = explicit.get(app_key(Path(it["id"]).name))
            if pkg is not None:
                if "bundle" not in pkg:
                    pkg["bundle"] = "app-" + pkg["id"]
                    auto_titles[pkg["bundle"]] = f"{Path(it['id']).name} (app + data)"
                it["bundle"] = pkg["bundle"]

    fw = defaultdict(list)                             # 5. hardware-specific bits
    for it in items:
        if "bundle" in it:
            continue
        if it["type"] == "kparam":
            k, _, v = it["id"].partition("=")
            mod = k.split(".")[0] if "." in k else ""
            if k in ("pci-stub.ids", "vfio-pci.ids", "vfio_pci.ids") and v:
                b = "kparam-" + k
                it["bundle"] = b
                ids = ["pci:" + x.lower() for x in v.split(",")]
                auto_whens[b] = "hw:" + ",".join(ids)
                auto_titles[b] = f"Kernel param {it['id']} (hides {device_name(ids[0])})"
            elif mod and sh(["modinfo", "-n", mod]).strip():
                b = "kparam-" + mod
                it["bundle"] = b
                auto_whens[b] = "hw:module:" + mod.replace("-", "_")
                auto_titles[b] = f"Kernel params for the {mod} driver"
        elif it["type"] == "file" and it["id"].startswith("/usr/lib/firmware/"):
            d = str(Path(it["id"]).parent).removeprefix("/usr/lib/firmware/")
            it["bundle"] = "firmware-" + d.replace("/", "-")
            fw[it["bundle"]].append(it["id"])
    for b, paths in fw.items():
        auto_whens[b] = firmware_when(paths)
        devs = [x for x in auto_whens[b].removeprefix("hw:").split(",") if x.startswith(("pci:", "usb:"))]
        auto_titles[b] = f"Firmware {b.removeprefix('firmware-')}" + (f" for {device_name(devs[0])}" if devs else "")
    if ctx.get("bt_macs"):
        auto_whens["bluetooth"] = "hw:" + ",".join("bt:" + m for m in ctx["bt_macs"])

    for it in items:                                    # 6. everything else
        if "bundle" in it or "for_unit" in it:
            continue
        if it["type"] == "file":
            it["bundle"] = "configs" if it.get("kind") == "modified" else "other-files"
        elif it["type"] == "path":
            it["bundle"] = "home-apps" if "home" in it else "other-paths"
        else:
            it["bundle"] = FALLBACK.get(it["type"], "other")
    unit_bundle = {i["id"]: i["bundle"] for i in items if i["type"] in ("svc", "uunit") and "bundle" in i}
    for it in items:                                    # 7. a unit's needs go wherever the unit went
        if "bundle" not in it:
            it["bundle"] = unit_bundle.get(it.get("for_unit", ""), FALLBACK.get(it["type"], "other"))
    present = usb_present()
    for b in sorted({i["bundle"] for i in items}):     # 8. apps made for specific USB devices (e.g. OpenLinkHub)
        if b in BUILTIN or b in whens or b in auto_whens:
            continue
        pkgs = [i["id"] for i in items if i["bundle"] == b and i["type"] in ("pkg", "aur") and not i.get("asdeps")]
        if devs := app_devices(pkgs, present):
            auto_whens[b] = "hw:" + ",".join(f"usb:{d}" for d in sorted(devs))
    auto_titles["identity"] = f"Machine identity: hostname {HOST}, SSH host keys, Tailscale node"

    bundles = {}
    for it in items:
        b = it["bundle"]
        if b not in bundles:
            title, when = BUILTIN.get(b, (None, None))
            bundles[b] = {"title": titles.get(b) or auto_titles.get(b) or title or f"{b.capitalize()} (auto-grouped)",
                          "when": whens.get(b) or auto_whens.get(b) or when or "always"}
    return dict(sorted(bundles.items(), key=lambda kv: rank(kv[0])))


def app_key(name: str) -> str:
    return re.sub(r"[^a-z0-9]", "", re.sub(r"-(bin|git|appimage|launcher)$", "", name.lower()))


def autostart_owner(desktop_file: str) -> str:
    """Package that owns the program an autostart entry launches ('' if unknown)."""
    try:
        m = re.search(r"^Exec=(.+)$", Path(desktop_file).read_text(errors="replace"), re.M)
        args = shlex.split(m.group(1)) if m else []
    except (OSError, ValueError):
        return ""
    prog = next((x for x in args if x != "env" and "=" not in x), "")
    prog = prog if prog.startswith("/") else (shutil.which(prog) or "") if prog else ""
    return file_owners([prog]).get(prog, "") if prog else ""


def usb_present() -> set[str]:
    return {f"{m.group(1)}:{m.group(2)}" for m in re.finditer(r"ID ([0-9a-f]{4}):([0-9a-f]{4})", sh(["lsusb"]))}


def app_devices(pkgs: list[str], present: set[str]) -> set[str]:
    """Plugged-in USB devices that these packages' udev rules are written for. Only for apps made for a few
    device makers (OpenLinkHub: Corsair); broad ones like OpenRGB (dozens of makers) say nothing about a machine."""
    if not pkgs:
        return set()
    pairs, vendors = set(), set()
    for ln in sh(["pacman", "-Ql", *pkgs]).splitlines():
        f = ln.split(" ", 1)[-1]
        if not re.search(r"/udev/rules\.d/[^/]+\.rules$", f):
            continue
        try:
            text = Path(f).read_text(errors="replace")
        except OSError:
            continue
        for rule in text.splitlines():
            v = re.search(r'idVendor\}?==\s*"([0-9a-fA-F]{4})"', rule)
            p = re.search(r'idProduct\}?==\s*"([0-9a-fA-F]{4})"', rule)
            if v and p:
                pairs.add(f"{v.group(1).lower()}:{p.group(1).lower()}")
            elif v:
                vendors.add(v.group(1).lower())
    makers = {x.split(":")[0] for x in pairs} | vendors
    if not makers or len(makers) > 3:
        return set()
    return {d for d in present if d in pairs or d.split(":")[0] in vendors}


def fingerprint_readers() -> list[str]:
    """USB IDs of fingerprint readers on this machine (by name, or listed by libfprint/systemd's udev data)."""
    present, named = set(), set()
    for line in sh(["lsusb"]).splitlines():
        m = re.search(r"ID ([0-9a-f]{4}):([0-9a-f]{4})\s*(.*)$", line)
        if m:
            present.add(f"usb:{m.group(1)}:{m.group(2)}")
            if re.search(r"(?i)finger|fingprint|fprint", m.group(3)):
                named.add(f"usb:{m.group(1)}:{m.group(2)}")
    known = set()
    for f in [*Path("/usr/lib/udev/hwdb.d").glob("*finger*"), *Path("/usr/lib/udev/hwdb.d").glob("*fprint*"),
              *Path("/usr/lib/udev/rules.d").glob("*fprint*")]:
        for m in re.finditer(r"(?i)usb:v([0-9a-f]{4})p([0-9a-f]{4})", f.read_text(errors="replace")):
            known.add(f"usb:{m.group(1).lower()}:{m.group(2).lower()}")
    return sorted(named | (known & present))


def describe(it: dict) -> str:
    t, i = it["type"], it["id"]
    return {
        "pkg": f"package    {i}" + (f"  [{it['repo']}]" if it.get("repo") not in (None, "?") else "") + ("  (as dependency)" if it.get("asdeps") else ""),
        "aur": f"AUR        {i}" + ("  (as dependency)" if it.get("asdeps") else ""),
        "repo": f"repo       [{i}]" + (f" + signing key ({brief(it.get('key_names', []), 2)})" if it.get("keys") else ""),
        "file": f"file       {i}" + ("  (edited package file)" if it.get("kind") == "modified" else ""),
        "svc": f"enable     {i}", "mask": f"mask       {i}",
        "usvc": f"user unit  enable {i}", "uunit": f"user unit  {i}", "umask": f"user unit  mask {i}",
        "kparam": f"kernel     {i}", "group": f"group      {i}", "shell": f"shell      {i}", "linger": f"linger     {i}",
        "cargo": f"cargo      {i}", "pipx": f"pipx       {i}", "npm": f"npm -g     {i}", "flatpak": f"flatpak    {i}",
        "path": f"backup     {'~/' + it['home'] if 'home' in it else i}",
        "mkdir": f"folder     {'~/' + it['rel'] if it.get('rel') else i}",
    }.get(t, f"{t} {i}")


# ------------------------------------------------------------------ manifest on disk

def read_items(d: Path) -> list[dict]:
    return [json.loads(ln) for ln in (d / "items.jsonl").read_text().splitlines() if ln.strip()]


def load_manifest(d: Path) -> tuple[dict, list[dict], dict]:
    try:
        return (json.loads((d / "meta.json").read_text()), read_items(d), json.loads((d / "bundles.json").read_text()))
    except OSError as e:
        die(f"no readable system record in {d}: {e.strerror}")


def fingerprint(it: dict) -> tuple:
    return it.get("sha"), it.get("link"), it.get("mode"), it.get("owner"), it.get("repo")


def diff_items(old: list[dict], new: list[dict]) -> tuple[list, list, list]:
    o, n = {key(i): i for i in old}, {key(i): i for i in new}
    return ([n[k] for k in sorted(n.keys() - o.keys())], [o[k] for k in sorted(o.keys() - n.keys())],
            [n[k] for k in sorted(n.keys() & o.keys()) if fingerprint(n[k]) != fingerprint(o[k])])


def change_lines(added, removed, changed) -> list[str]:
    return ([f"+ {describe(i)}" for i in added] + [f"- {describe(i)}" for i in removed]
            + [f"~ {describe(i)}" for i in changed])


def summary_md(meta: dict, items: list[dict], bundles: dict) -> str:
    by = defaultdict(list)
    for it in items:
        by[it["bundle"]].append(it)
    out = [f"# {meta['host']}: system record", "",
           f"User `{meta['user']}`, kernel {meta['kernel']}, {meta['chassis'] or 'unknown'} chassis. "
           + ("Complete." if meta["complete"] else "**Incomplete** (captured without root)."), "",
           "Restore offers each bundle; its *when* rule decides whether it starts ticked on the target machine.", ""]
    for b, info in bundles.items():
        its = by[b]
        out.append(f"## {info['title']}")
        out.append(f"`{b}` · {len(its)} items · when: `{info['when']}`")
        out.append("")
        out += [f"- {describe(i)}" for i in its[:30]]
        if len(its) > 30:
            out.append(f"- ... {len(its) - 30} more")
        out.append("")
    out += ["## Deliberately not in this record",
            "Secrets live only in the encrypted backup: Wi-Fi/VPN passwords, SSH host keys, /etc/shadow, "
            "Bluetooth link keys, Tailscale state, /root and your home directory.", ""]
    return "\n".join(out)


REDOWNLOAD = "not-backed-up.md"
# what an excluded path is and how it comes back: (regex on the exclude pattern, label, how to get it back)
REDOWNLOAD_HINTS = [
    (r"Steam/steamapps/(common|downloading|temp)$", None, None),          # games are listed from Steam itself
    (r"Steam/steamapps/shadercache$", "Steam shader caches", "rebuilt by Steam/Proton while you play"),
    (r"Steam/steamapps/workshop$", "Steam Workshop items", "Steam downloads them again for your subscriptions"),
    (r"pfx/drive_c/windows$", "Proton's Windows system files", "Proton recreates them when a game starts"),
    (r"Steam/(ubuntu12_\w+|steamrt\w*|linux32|linux64|package|appcache|depotcache|logs)$", "Steam client runtime",
     "Steam downloads it on first start"),
    (r"discord/app-\*$", "Discord app files", "the `discord` package downloads them on first start"),
    (r"Claude/claude-code$", "Claude Code inside the Claude desktop app", "the Claude desktop app downloads it again"),
    (r"spotify-launcher/install$", "Spotify", "`spotify-launcher` downloads it on first start"),
    (r"\.rustup/toolchains$", "Rust toolchains", "`rustup toolchain install <name>` (cargo does it on first use)"),
    (r"\.rustup/downloads$", None, None),
    (r"\.cargo/(registry|git)$", "Cargo crate cache", "cargo downloads crates again when you build"),
    (r"\.pub-cache$", "Dart/Flutter package cache", "`dart pub get` / `flutter pub get` in your projects"),
    (r"\.npm/_cacache$", "npm cache", "npm refills it"),
    (r"\.gradle/caches$", "Gradle caches", "Gradle refills them"),
    (r"go/pkg/mod$", "Go module cache", "go refills it"),
]
STEAM_TOOLS = re.compile(r"(?i)^(proton|steam linux runtime|steamworks common|steamvr)")


def human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.0f} {unit}" if unit in ("B", "KB", "MB") else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} TB"


def disk_usage(paths: list[str]) -> int:
    out = sh(["du", "-scb", "--", *paths], timeout=300).strip().splitlines()
    return int(out[-1].split()[0]) if out and out[-1].split()[0].isdigit() else 0


def steam_games() -> list[tuple[str, str, int]]:
    """(name, appid, bytes) of installed Steam games, from Steam's own manifests."""
    libs = {HOME / ".local/share/Steam"}
    try:
        vdf = (HOME / ".local/share/Steam/steamapps/libraryfolders.vdf").read_text(errors="replace")
        libs |= {Path(p) for p in re.findall(r'"path"\s+"([^"]+)"', vdf)}
    except OSError:
        pass
    games = []
    for lib in libs:
        for acf in (lib / "steamapps").glob("appmanifest_*.acf"):
            try:
                t = acf.read_text(errors="replace")
            except OSError:
                continue
            get = lambda k: (re.search(rf'"{k}"\s+"([^"]*)"', t) or [None, ""])[1]
            games.append((get("name") or acf.stem, get("appid"), int(get("SizeOnDisk") or 0)))
    return sorted(games, key=lambda g: g[0].lower())


def redownload_list(meta: dict) -> str:
    """Readable list of what's installed but deliberately not in the backup, and how to get it back."""
    groups: dict[str, dict] = {}
    for pat in load_excludes():
        hint = next(((lbl, how) for rx, lbl, how in REDOWNLOAD_HINTS if re.search(rx, pat)), ("", ""))
        if hint[0] is None:
            continue
        found = sorted(p for p in glob.glob(pat.replace("/home/*", str(HOME), 1), recursive=True) if os.path.lexists(p))
        if not found:
            continue
        label, how = hint if hint[0] else ("Caches & other regenerated data", "apps rebuild them as needed")
        e = groups.setdefault(label, {"how": how, "paths": [], "bytes": 0})
        e["paths"] += found
    mounts = mountpoints()                   # never walk into a mount (~/iCloud would crawl all of iCloud Drive)
    for e in groups.values():
        local = [p for p in e["paths"] if p not in mounts]
        e["bytes"] = disk_usage(local) if local else None
    lines = [f"# Not in the backup: re-download after a restore", "",
             f"{meta['host']}, recorded {dt.date.today()}. These were left out on purpose: they re-download "
             "themselves or already live somewhere else. Your settings for all of them *are* backed up.", ""]
    games = steam_games()
    if games:
        real = [g for g in games if not STEAM_TOOLS.match(g[0])]
        tools = [g for g in games if STEAM_TOOLS.match(g[0])]
        lines += [f"## Steam games: {len(real)}, {human(sum(g[2] for g in real))}", "",
                  "Reinstall from your Steam library; saves in Proton prefixes and Steam Cloud come back on their own.", ""]
        lines += [f"- {n} ({human(b)}, appid {a})" for n, a, b in real]
        if tools:
            lines += ["", f"Steam installs these itself when a game needs them: " + ", ".join(n for n, _, _ in tools)]
        lines.append("")
    for label, e in sorted(groups.items(), key=lambda kv: -(kv[1]["bytes"] or 0)):
        names = [Path(p).name for p in e["paths"]]
        if len(e["paths"]) <= 3:
            for p in e["paths"]:
                if Path(p).is_dir() and not Path(p).is_symlink():
                    names += [c.name for c in sorted(Path(p).iterdir())[:6]]
        versions = sorted(set(re.findall(r"\d+(?:\.\d+){1,3}", " ".join(names))))
        extra = ""
        if label == "Rust toolchains":
            extra = " (" + ", ".join(c.name for p in e["paths"] for c in sorted(Path(p).iterdir())) + ")"
        elif versions and label not in ("Caches & other regenerated data",):
            extra = f" (version {', '.join(versions[-3:])})"
        where = ", ".join(p.replace(str(HOME), "~") for p in e["paths"][:3]) + (f" +{len(e['paths']) - 3} more" if len(e["paths"]) > 3 else "")
        size = f", {human(e['bytes'])}" if e["bytes"] is not None else ""
        lines += [f"- **{label}**{extra}{size}: {e['how']}.", f"  `{where}`"]
    return "\n".join(lines) + "\n"


def capture(out: Path = MANIFEST_DIR, quiet: bool = False, commit: bool = True, inventory: bool = False) -> Path:
    say = (lambda m: None) if quiet else log
    if not IS_ROOT and not quiet:
        warn("not root: root-only files are skipped and the record is marked incomplete")
    tmp = Path(tempfile.mkdtemp(prefix="sysbackup-"))
    try:
        stage = tmp / "manifest"
        (stage / "files").mkdir(parents=True)
        ctx: dict = {}
        say("packages & repositories")
        items = capture_packages(ctx) + capture_repos(ctx, stage, tmp)
        say("files changed or added outside of packages")
        items += capture_files(stage, ctx)
        say("services")
        items += capture_services(stage)
        items += unit_needs(items, stage, ctx)
        say("kernel, account, language tools")
        items += capture_kernel() + capture_account() + capture_tools()
        say("home directory & secrets")
        items += capture_paths(ctx)
        say("grouping into bundles")
        ctx["stage"], ctx["fp_readers"] = stage, fingerprint_readers()
        bundles = assign_bundles(items, ctx)
        items.sort(key=lambda i: (rank(i["bundle"]), i["bundle"], i["type"], i["id"]))
        meta = {"host": HOST, "user": USER.pw_name, "home": str(HOME), "kernel": os.uname().release,
                "chassis": sh(["hostnamectl", "chassis"]).strip(), "complete": IS_ROOT and not ctx["skipped"],
                "machine": machine_fingerprint(), "format": 1}
        (stage / "meta.json").write_text(json.dumps(meta, indent=2, sort_keys=True) + "\n")
        (stage / "items.jsonl").write_text("".join(json.dumps(i, sort_keys=True) + "\n" for i in items))
        (stage / "bundles.json").write_text(json.dumps(bundles, indent=2) + "\n")
        (stage / "versions.txt").write_text(sh(["pacman", "-Q"]))
        (stage / "SUMMARY.md").write_text(summary_md(meta, items, bundles))
        (stage / ".gitignore").write_text(f"last-capture\n{REDOWNLOAD}\n")
        if inventory:
            say("listing what's installed but deliberately not backed up")
            (stage / REDOWNLOAD).write_text(redownload_list(meta))
        if ctx["skipped"]:
            (stage / "skipped.txt").write_text("\n".join(ctx["skipped"]) + "\n")
        publish(stage, out, items, commit)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    say(f"system record written to {out}")
    return out


def publish(stage: Path, out: Path, items: list[dict], commit: bool) -> None:
    old = read_items(out) if (out / "items.jsonl").exists() else []
    out.mkdir(parents=True, exist_ok=True)
    if IS_ROOT:
        os.chmod(out, 0o700)
    for child in out.iterdir():
        if child.name == ".git" or (child.name == REDOWNLOAD and not (stage / REDOWNLOAD).exists()):
            continue                          # quick captures (pacman hook) keep the last re-download list
        if child.is_dir() and not child.is_symlink():
            shutil.rmtree(child)
        else:
            child.unlink()
    for child in stage.iterdir():
        shutil.move(str(child), out / child.name)
    (out / "last-capture").write_text(now_iso() + "\n")
    if not (commit and have("git")):
        return
    git = ["git", "-C", str(out), "-c", "user.name=sysbackup", "-c", f"user.email=sysbackup@{HOST}"]
    if not (out / ".git").exists():
        sh(git + ["init", "-q"])
    sh(git + ["add", "-A"])
    if subprocess.run(git + ["diff", "--cached", "--quiet"]).returncode == 0:
        return
    lines = change_lines(*diff_items(old, items)) if old else ["first record"]
    subject = f"{len(lines)} change(s): " + brief([re.sub(r"\s+", " ", ln) for ln in lines], 4) if old else "first record"
    sh(git + ["commit", "-q", "-m", subject[:200], "-m", "\n".join(lines[:300])])


# ------------------------------------------------------------------ backup

def cmd_capture(a) -> None:
    capture(a.out, quiet=a.quiet, commit=a.out == MANIFEST_DIR, inventory=True)


CONF_FILES = ("sysbackup.conf", "bundles.local", "excludes.txt")   # never the password


def toolkit_digest() -> str:
    h = hashlib.sha256()
    for p in sorted([*LIB_DIR.rglob("*"), *[CONF_DIR / f for f in CONF_FILES], MANIFEST_DIR / REDOWNLOAD]):
        if p.is_file() and "__pycache__" not in p.parts:
            h.update(str(p).encode())
            h.update(p.read_bytes())
    return h.hexdigest()


def rclone_user(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(user_cmd(["rclone", *args]), capture_output=True, text=True)


def mirror_toolkit() -> None:
    """Keep the toolkit in iCloud usable for a restore (no secrets in there).

    The Toolkit folder is your editable copy, so it's only re-created if it's gone. Each machine's
    settings go to Toolkit/machines/<host>/ and the guide to "How to restore.md" next to it.
    """
    dest = CONF.get("TOOLKIT_MIRROR", "")
    stamp = STATE_DIR / "toolkit-mirrored"
    digest = toolkit_digest()
    if not dest or (stamp.exists() and stamp.read_text().strip() == digest):
        return
    tmp = Path(tempfile.mkdtemp(prefix="sysbackup-mirror-"))
    try:
        os.chmod(tmp, 0o755)
        for f in CONF_FILES:
            if (CONF_DIR / f).is_file():
                shutil.copy(CONF_DIR / f, tmp / f)
                os.chmod(tmp / f, 0o644)
        if (MANIFEST_DIR / REDOWNLOAD).is_file():
            shutil.copy(MANIFEST_DIR / REDOWNLOAD, tmp / REDOWNLOAD)
            os.chmod(tmp / REDOWNLOAD, 0o644)
        steps = [["copy", str(tmp), f"{dest}/machines/{HOST}"]]
        if "sysbackup" not in rclone_user(["lsf", dest]).stdout.split():
            steps.append(["copy", str(LIB_DIR), dest, "--exclude", "__pycache__/**"])
        if (LIB_DIR / "README.md").is_file():
            steps.append(["copyto", str(LIB_DIR / "README.md"), f"{dest.rsplit('/', 1)[0]}/How to restore.md"])
        for args in steps:
            r = rclone_user(args)
            if r.returncode != 0:
                warn(f"couldn't update {dest} ({r.stderr.strip()[-200:]}). If iCloud signed you out: rclone reconnect iCloud:")
                return
        stamp.write_text(digest + "\n")
        log(f"settings of {HOST} saved to {dest}/machines/{HOST}")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def cmd_backup(a) -> None:
    need_root("backup")
    if RESTORING.exists() and not a.force:
        log("automatic backups are paused while a restore is in progress (so the half-restored system never "
            "becomes the newest snapshot). They resume when `sysbackup restore` finishes; --force overrides.")
        return
    if not Path(CONF["RESTIC_PASSWORD_FILE"]).is_file():
        die(f"no password file at {CONF['RESTIC_PASSWORD_FILE']}: run install.sh first")
    capture(quiet=a.quiet, inventory=True)
    if not online():
        log("offline: trying again in an hour")
        subprocess.run(["systemd-run", "--quiet", "--on-active=1h", "--unit=sysbackup-retry", "--timer-property=AccuracySec=5min",
                        "/usr/bin/systemctl", "start", "--no-block", "sysbackup.service"], capture_output=True)
        return
    if problem := cloud_problem():
        die(f"can't reach {CONF['RESTIC_REPOSITORY']}: {problem}")
    restic(["unlock"])   # removes only stale locks, e.g. from a backup cut off by a shutdown
    pats = CONF_DIR / "excludes.txt" if (CONF_DIR / "excludes.txt").is_file() else LIB_DIR / "excludes.txt"
    paths = [p for p in dict.fromkeys([*ESSENTIAL_PATHS, *CONF["EXTRA_PATHS"].split()]) if Path(p).exists()] + [str(STATE_DIR), str(HOME)]
    log(f"backing up to {CONF['RESTIC_REPOSITORY']}")
    r = restic(["backup", "--one-file-system", "--exclude-caches", "--exclude-file", str(pats),
                "--host", HOST, "--group-by", "host", "--tag", a.tag, *(["--quiet"] if a.quiet else []), *paths], quiet=False)
    if r.returncode == 3:
        warn("some files couldn't be read; the snapshot is still usable")
    elif r.returncode != 0:
        die("backup failed: " + restic_error(r.stderr))
    log("applying retention: " + CONF["KEEP"])
    r = restic(["forget", "--host", HOST, "--group-by", "host", *shlex.split(CONF["KEEP"])])
    if r.returncode != 0:
        warn("retention (forget) failed, will retry next time: " + restic_error(r.stderr))
    stamp = STATE_DIR / "last-prune"
    if not stamp.exists():          # first backup: nothing to prune yet, start the weekly clock
        stamp.write_text(now_iso() + "\n")
    elif (dt.datetime.now().timestamp() - stamp.stat().st_mtime) > 86400 * float(CONF["PRUNE_EVERY_DAYS"]):
        log("pruning old data (weekly)")
        r = restic(["prune", "--max-unused", "5%"])
        if r.returncode == 0:
            stamp.write_text(now_iso() + "\n")
        else:
            warn("prune failed, will retry next time: " + restic_error(r.stderr))
    mirror_toolkit()
    (STATE_DIR / "last-backup").write_text(now_iso() + "\n")
    log("backup done")


def online() -> bool:
    try:
        urllib.request.urlopen(urllib.request.Request("https://www.icloud.com", method="HEAD"), timeout=15)
    except urllib.error.HTTPError:
        pass                      # reachable, it just answered with an HTTP error
    except Exception:  # noqa: BLE001
        return False
    return True


def cmd_pacman_hook(a) -> None:
    """Called by pacman after installs/removals: refresh the record now, back up a while after the last change."""
    if not (CONF_DIR / "sysbackup.conf").exists():
        return
    try:
        capture(quiet=True)
    except BaseException as e:  # never break pacman
        print(f"sysbackup: couldn't refresh the system record: {e}", file=sys.stderr)
    delay = CONF["AFTER_INSTALL_DELAY"].strip().lower()
    if delay in ("", "0", "off", "no", "never") or RESTORING.exists():
        return
    if sh(["systemctl", "is-enabled", "sysbackup.timer"]).strip() != "enabled":
        return
    # every install/removal pushes the backup back, so a session of changes ends in a single backup
    unit = "sysbackup-after-pacman"
    subprocess.run(["systemctl", "stop", f"{unit}.timer", f"{unit}.service"], capture_output=True)
    subprocess.run(["systemctl", "reset-failed", f"{unit}.timer", f"{unit}.service"], capture_output=True)
    subprocess.run(["systemd-run", "--quiet", f"--on-active={delay}", f"--unit={unit}", "--timer-property=AccuracySec=1min",
                    "/usr/bin/systemctl", "start", "--no-block", "sysbackup.service"], capture_output=True)


def cmd_notify(a) -> None:
    title = "Backup check found problems" if "verify" in a.unit else "Backup failed"
    err = last_error(a.unit)
    body = (err[:300] + "\n" if err else "") + "Details: sysbackup status"
    repo = CONF["RESTIC_REPOSITORY"]
    if repo.startswith("rclone:") and online():
        remote = repo.split(":")[1]
        try:
            ok = subprocess.run(user_cmd(["rclone", "lsf", "--max-depth", "1", f"{remote}:"]),
                                capture_output=True, timeout=120).returncode == 0
        except subprocess.TimeoutExpired:
            ok = False
        if not ok:
            title, body = "iCloud signed you out", f"Backups (and your iCloud mount) can't reach iCloud.\nRun: rclone reconnect {remote}:"
    sh(["notify-send", "-u", "critical", "-a", "sysbackup", "-i", "dialog-warning", title, body], user=True)


def cmd_status(a) -> None:
    for name in ("last-capture", "last-backup", "last-prune"):
        p = STATE_DIR / name if name != "last-capture" else MANIFEST_DIR / name
        try:
            print(f"{name:13} {p.read_text().strip()}")
        except OSError:
            print(f"{name:13} never")
    print("\nAutomatic runs:")
    for unit, timer, label in (("sysbackup.service", "sysbackup.timer", "backup"),
                               ("sysbackup-verify.service", "sysbackup-verify.timer", "weekly check")):
        p = dict(ln.split("=", 1) for ln in sh(["systemctl", "show", "-p", "Result", "-p", "ActiveState", "-p",
                                               "ExecMainExitTimestamp", unit]).splitlines() if "=" in ln)
        nxt = sh(["systemctl", "show", "-p", "NextElapseUSecRealtime", "--value", timer]).strip()
        if p.get("ActiveState") == "activating":
            state = "running now"
        elif not p.get("ExecMainExitTimestamp"):
            state = "hasn't run automatically yet"
        elif p.get("Result") == "success":
            state = f"OK ({p['ExecMainExitTimestamp']})"
        else:
            err = last_error(unit)
            state = paint("1;31", f"FAILED ({p['ExecMainExitTimestamp']})") + (": " + err.replace("\n", "\n" + " " * 17) if err else "")
        print(f"  {label:13}  {state}" + (paint("2", f"\n  {'':13}  next: {nxt}") if nxt else ""))
    if "FAILED" in sh(["systemctl", "is-failed", "sysbackup.service", "sysbackup-verify.service"]).upper():
        print(paint("2", "  full log: journalctl -u sysbackup -u sysbackup-verify -e"))
    if (MANIFEST_DIR / ".git").exists():
        print("\nRecent system changes:")
        print(sh(["git", "-C", str(MANIFEST_DIR), "log", "-10", "--date=format:%Y-%m-%d %H:%M", "--format=  %ad  %s"]))
    if IS_ROOT and have("restic") and Path(CONF["RESTIC_PASSWORD_FILE"]).is_file():
        snaps = restic_snapshots()
        print(f"Snapshots in {CONF['RESTIC_REPOSITORY']}:")
        for h in sorted({s["hostname"] for s in snaps}):
            mine = [s for s in snaps if s["hostname"] == h]
            print(f"  {h:20} {len(mine):3} kept, newest {mine[-1]['_t']:%Y-%m-%d %H:%M}, oldest {mine[0]['_t']:%Y-%m-%d}")


def cmd_restic(a) -> None:
    """Run any restic command against the configured repository (password & iCloud handled)."""
    need_root("restic")
    sys.exit(subprocess.run(restic_cmd(a.args), env=restic_env()).returncode)


def cmd_bundles(a) -> None:
    d = Path(a.manifest) if a.manifest else MANIFEST_DIR
    meta, items, bundles = load_manifest(d)
    facts = target_facts()
    by = defaultdict(list)
    for it in items:
        by[it["bundle"]].append(it)
    print(f"System record of {meta['host']} ({len(items)} items in {len(bundles)} bundles); ticks are for this machine ({HOST}):\n")
    for b, info in bundles.items():
        ok, why = when_applies(info["when"], facts, meta)
        print(paint("1", f"[{'x' if ok else ' '}] {info['title']}") + paint("2", f"   {b} · when {info['when']}")
              + (paint("33", f"   ({why})") if not ok else ""))
        its = by[b]
        shown = its if a.all else its[:6]
        for it in shown:
            print(f"      {describe(it)}")
        if len(its) > len(shown):
            print(paint("2", f"      ... {len(its) - len(shown)} more (--all)"))


# ------------------------------------------------------------------ verify

class Report:
    def __init__(self):
        self.fails = self.warns = 0

    def section(self, t):
        print("\n" + paint("1", t))

    def ok(self, m, detail=()):
        print("  " + paint("1;32", "PASS"), m)

    def info(self, m, detail=()):
        print("  " + paint("2", "info"), m)

    def warn(self, m, detail=()):
        print("  " + paint("1;33", "WARN"), m)
        self._detail(detail)
        self.warns += 1

    def fail(self, m, detail=()):
        print("  " + paint("1;31", "FAIL"), m)
        self._detail(detail)
        self.fails += 1

    @staticmethod
    def _detail(lines):
        lines = list(lines)
        for ln in lines[:15]:
            print("         " + ln)
        if len(lines) > 15:
            print(f"         ... and {len(lines) - 15} more")


def aur_info(names: list[str]) -> dict:
    url = "https://aur.archlinux.org/rpc/v5/info?" + urllib.parse.urlencode([("arg[]", n) for n in names])
    with urllib.request.urlopen(url, timeout=20) as r:
        return {x["Name"]: x for x in json.load(r).get("results", [])}


def repo_reachable(repo: str, section: str) -> tuple[bool, str]:
    last = "no Server line"
    for m in re.finditer(r"^\s*Server\s*=\s*(\S+)", section, re.M):
        url = m.group(1).replace("$repo", repo).replace("$arch", "x86_64").rstrip("/") + f"/{repo}.db"
        try:
            urllib.request.urlopen(urllib.request.Request(url, method="HEAD"), timeout=15)
            return True, url
        except Exception as e:  # noqa: BLE001
            last = f"{url}: {e}"
    return False, last


def covered(path: str, manifest: Path, owners: dict) -> bool:
    if path in owners or (manifest / "files" / path.lstrip("/")).exists() or os.path.lexists(manifest / "files" / path.lstrip("/")):
        return True
    return path.startswith(str(HOME) + "/") and not excluded(path, load_excludes())


def cmd_verify(a) -> None:
    if not IS_ROOT:
        warn("not root: root-only files can't be compared, expect false drift; cloud checks skipped")
    R, M = Report(), MANIFEST_DIR
    max_age = float(CONF["MAX_AGE_HOURS"])

    R.section("1. System record")
    if not (M / "items.jsonl").exists():
        R.fail(f"no system record at {M}: run sudo sysbackup backup")
        return finish(R)
    meta, items, bundles = load_manifest(M)
    (R.ok if meta.get("complete") else R.fail)("record is complete" if meta.get("complete") else "record is incomplete (captured without root)")
    if RESTORING.exists():
        R.fail(f"automatic backups are paused: a restore started {RESTORING.read_text().strip()} never finished",
               ["finish it with: sudo sysbackup restore", "or resume backups with: sudo rm " + str(RESTORING)])
    try:
        t = parse_time((M / "last-capture").read_text())
        (R.ok if age_hours(t) < max_age else R.fail)(f"last captured {t:%Y-%m-%d %H:%M} ({age_hours(t):.0f}h ago)")
    except (OSError, ValueError):
        R.fail("no capture timestamp")
    if IS_ROOT:
        try:
            t = parse_time((STATE_DIR / "last-backup").read_text())
            (R.ok if age_hours(t) < max_age else R.fail)(f"last successful backup {t:%Y-%m-%d %H:%M} ({age_hours(t):.0f}h ago)")
        except (OSError, ValueError):
            R.fail("no successful backup yet")
    tmpd = Path(tempfile.mkdtemp(prefix="sysbackup-verify-"))
    try:
        capture(tmpd / "now", quiet=True, commit=False)
        drift = change_lines(*diff_items(items, read_items(tmpd / "now")))
        if drift:
            R.warn(f"{len(drift)} change(s) since the last capture; the next backup picks them up", drift)
        else:
            R.ok("record matches the running system")

        R.section("2. Can it be reinstalled?")
        sync = set(sh(["pacman", "-Slq"]).split())
        pkgs = [i["id"] for i in items if i["type"] == "pkg"]
        gone = [p for p in pkgs if p not in sync]
        (R.fail if gone else R.ok)(f"{len(pkgs) - len(gone)}/{len(pkgs)} repo packages are available", gone)
        aur = [i["id"] for i in items if i["type"] == "aur"]
        if aur:
            try:
                info = aur_info(aur)
                missing = [p for p in aur if p not in info]
                (R.fail if missing else R.ok)(f"{len(aur) - len(missing)}/{len(aur)} AUR packages still exist on the AUR", missing)
                stale = [f"{p}: orphaned" for p in aur if p in info and not info[p].get("Maintainer")]
                stale += [f"{p}: flagged out of date" for p in aur if p in info and info[p].get("OutOfDate")]
                if stale:
                    R.info("AUR notes: " + brief(stale, 4))
            except Exception as e:  # noqa: BLE001
                R.warn(f"couldn't reach the AUR to check {brief(aur, 4)}: {e}")
            (R.ok if "paru" in pkgs or "paru" in sync else R.warn)("paru (AUR helper) is in the package list" if "paru" in pkgs else "paru isn't in the package list; restore will install it first")
        for r in (i for i in items if i["type"] == "repo"):
            ok, where = repo_reachable(r["id"], r.get("section", ""))
            (R.ok if ok else R.fail)(f"extra repo [{r['id']}] reachable" if ok else f"extra repo [{r['id']}] unreachable: {where}")
            has_key = (M / "repo-keys.asc").exists() and (M / "repo-keys.asc").stat().st_size > 0
            (R.ok if has_key else R.warn)(f"[{r['id']}] signing key saved" if has_key else f"no hand-added signing key found for [{r['id']}]")

        R.section("3. Is everything custom actually captured?")
        units = [i for i in items if i["type"] == "svc" and not i.get("pkg")]
        exec_paths: dict[str, list[str]] = {}
        for u in units:
            out = sh(["systemctl", "show", "-p", "ExecStartPre", "-p", "ExecStart", "-p", "ExecStartPost", "--", u["id"]])
            exec_paths[u["id"]] = re.findall(r"path=(\S+)", out)
        for f in sorted(p for p in (M / "user-units").rglob("*") if p.is_file()):
            ps = []
            for m in re.finditer(r"^Exec[A-Za-z]*=[-@:+!|]*(\S+)", f.read_text(), re.M):
                ps.append(m.group(1).replace("%h", str(HOME)))
            exec_paths["user:" + f.name] = ps
        allp = sorted({p for ps in exec_paths.values() for p in ps if p.startswith("/")})
        owners = file_owners(allp)
        bad = [f"{u} runs {p}" for u, ps in exec_paths.items() for p in ps if p.startswith("/") and not covered(p, M, owners)]
        (R.fail if bad else R.ok)(f"programs started by your {len(exec_paths)} hand-made units are all backed up", bad)

        dk = re.findall(r"^([^/,\s]+)/([^,\s]+)", sh(["dkms", "status"]), re.M)
        srcs = sorted({f"/usr/src/{n}-{v}" for n, v in dk})
        so = file_owners(srcs)
        bad = [s for s in srcs if s not in so and not (M / "files" / s.lstrip("/")).exists()]
        if srcs:
            (R.fail if bad else R.ok)(f"DKMS driver sources come from packages ({brief([so.get(s, '?') for s in srcs], 3)})", bad)

        kp = {i["id"] for i in items if i["type"] == "kparam"}
        live = [p for p in Path("/proc/cmdline").read_text().split() if not ROOTISH.match(p)]
        bad = [p for p in live if p not in kp]
        (R.fail if bad else R.ok)("running kernel parameters are all in a saved config file", [f"{p}: active now but not saved anywhere" for p in bad])

        fstab = (M / "files/etc/fstab").read_text() if (M / "files/etc/fstab").exists() else ""
        fstab_targets = {ln.split()[1] for ln in fstab.splitlines() if ln.strip() and not ln.lstrip().startswith("#") and len(ln.split()) > 1}
        unit_text = "\n".join(p.read_text(errors="replace") for p in [*(M / "user-units").rglob("*"), *(M / "files/etc/systemd").rglob("*")] if p.is_file() and not p.is_symlink())
        pseudo = {"proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "securityfs", "cgroup2", "pstore", "efivarfs", "bpf",
                  "debugfs", "tracefs", "configfs", "fusectl", "mqueue", "hugetlbfs", "autofs", "binfmt_misc", "ramfs",
                  "nsfs", "fuse.portal", "fuse.gvfsd-fuse", "squashfs", "overlay"}
        bad = []
        for line in Path("/proc/self/mounts").read_text().splitlines():
            f = line.split()
            tgt, fs = f[1].encode().decode("unicode_escape"), f[2]
            if fs in pseudo or tgt.startswith(("/proc", "/sys", "/dev", "/run")) or tgt in fstab_targets:
                continue
            rel = "%h/" + tgt.removeprefix(str(HOME) + "/") if tgt.startswith(str(HOME) + "/") else None
            if tgt in unit_text or (rel and rel in unit_text):
                continue
            bad.append(f"{tgt} ({fs}): nothing saved recreates this mount")
        (R.fail if bad else R.ok)("every mount is recreated by fstab or a saved unit", bad)

        mods = [ln.split()[0] for ln in Path("/proc/modules").read_text().splitlines()]
        paths = {m: sh(["modinfo", "-n", m]).strip() for m in mods}
        mo = file_owners([p for p in paths.values() if p.startswith("/")])
        stray = [f"{m}: {p}" for m, p in paths.items() if p.startswith("/") and p not in mo and "/updates/dkms/" not in p]
        (R.warn if stray else R.ok)("all loaded kernel modules come from packages or DKMS", stray)

        links = [*Path("/etc/systemd/system").glob("*.wants/*"), *(HOME / ".config/systemd/user").glob("*.wants/*")]
        dead = [f"{p} -> {os.readlink(p)}" for p in links if p.is_symlink() and not p.exists()]
        (R.warn if dead else R.ok)("no leftover enable links to uninstalled units (safe to delete these)" if dead
                                   else "no leftover enable links to uninstalled units", dead)
        if (M / "skipped.txt").exists():
            R.warn("files that couldn't be read at capture time", (M / "skipped.txt").read_text().split())

        R.section("4. Cloud backup")
        if not IS_ROOT or a.no_cloud:
            R.info("skipped (needs root)" if not IS_ROOT else "skipped (--no-cloud)")
        elif not online():
            R.warn("offline: cloud checks skipped this time")
        else:
            verify_cloud(R, M, tmpd, deep=a.deep or a.full, full=a.full)
    finally:
        shutil.rmtree(tmpd, ignore_errors=True)
    finish(R)


def verify_cloud(R: Report, M: Path, tmpd: Path, deep: bool, full: bool) -> None:
    max_age = float(CONF["MAX_AGE_HOURS"])
    if not have("restic"):
        return R.fail("restic isn't installed")
    if not Path(CONF["RESTIC_PASSWORD_FILE"]).is_file():
        return R.fail(f"password file missing: {CONF['RESTIC_PASSWORD_FILE']}")
    r = restic(["snapshots", "--json"])          # one iCloud round trip: proves the password works + gives ages
    if r.returncode != 0:
        why = cloud_problem() or ([ln for ln in r.stderr.strip().splitlines() if ln.strip()] or ["?"])[-1]
        return R.fail(f"can't open {CONF['RESTIC_REPOSITORY']}", why.splitlines())
    R.ok(f"repository {CONF['RESTIC_REPOSITORY']} opens with the saved password")
    snaps = json.loads(r.stdout or "[]")
    for s in snaps:
        s["_t"] = parse_time(s["time"])
    snaps.sort(key=lambda s: s["_t"])
    mine = [s for s in snaps if s["hostname"] == HOST]
    if not mine:
        return R.fail(f"no snapshots for {HOST} yet")
    last = mine[-1]
    (R.ok if age_hours(last["_t"]) < max_age else R.fail)(f"newest snapshot {last['_t']:%Y-%m-%d %H:%M} ({age_hours(last['_t']):.0f}h ago), {len(mine)} kept for {HOST}")
    others = sorted({s["hostname"] for s in snaps} - {HOST})
    if others:
        R.info("other machines in this backup: " + ", ".join(others))
    confirmed = STATE_DIR / "password-confirmed"
    (R.ok if confirmed.exists() else R.fail)(
        f"you confirmed the backup password is saved outside this machine ({confirmed.read_text().strip()})" if confirmed.exists()
        else "backup password not confirmed as saved elsewhere: store it in your password manager, then: sudo touch /var/lib/sysbackup/password-confirmed")
    if not deep:
        R.info("quick check. --deep also re-downloads a sample, tests a restore and compares the Toolkit (weekly check)")
        return
    verify_toolkit(R)

    want = [str(M / "items.jsonl"), str(CONF_DIR / "sysbackup.conf"), str(HOME / ".config")]
    if str(LIB_DIR).startswith(("/usr/local/", "/etc/", "/root/")):
        want.append(str(LIB_DIR / "sysbackup"))
    for p in ("/etc/NetworkManager/system-connections", "/var/lib/bluetooth", "/var/lib/tailscale", "/var/lib/fprint"):
        if Path(p).exists():
            want.append(p)
    listing = set(restic(["ls", last["id"], *sorted({str(Path(w).parent) for w in want})]).stdout.splitlines())
    missing = [w for w in want if w not in listing]
    (R.fail if missing else R.ok)("snapshot contains the record, the toolkit, its config, your settings and secrets", missing)

    subset = "100%" if full else CONF["VERIFY_READ_SUBSET"]
    r = restic(["check", f"--read-data-subset={subset}"])
    (R.ok if r.returncode == 0 else R.fail)(f"integrity check passed (re-downloaded and verified {subset} of the data)" if r.returncode == 0
                                             else "integrity check FAILED", [] if r.returncode == 0 else r.stdout.strip().splitlines()[-8:])

    target = tmpd / "restore"
    r = restic(["restore", last["id"], "--target", str(target), "--include", str(M / "items.jsonl"), "--include", str(CONF_DIR / "sysbackup.conf")])
    got = target / str(M / "items.jsonl").lstrip("/")
    if r.returncode != 0 or not got.exists():
        R.fail("test restore failed", r.stderr.strip().splitlines()[-4:])
    else:
        same = got.read_bytes() == (M / "items.jsonl").read_bytes()
        (R.ok if same else R.info)("test restore: the restored system record matches the one on disk" if same
                                   else "test restore worked; the record changed after this snapshot (normal if you installed something since)")


def verify_toolkit(R: Report) -> None:
    dest = CONF.get("TOOLKIT_MIRROR", "")
    if dest:
        names = rclone_user(["lsf", dest]).stdout.split()
        if "sysbackup" not in names or "machines/" not in names:
            R.fail(f"the Toolkit in {dest} is missing or incomplete (the next backup re-creates it)")
        else:
            remote = rclone_user(["cat", f"{dest}/sysbackup"]).stdout
            if hashlib.sha256(remote.encode()).hexdigest() == sha_file(LIB_DIR / "sysbackup"):
                R.ok(f"the Toolkit in {dest} matches what's installed, with this machine's settings")
            else:
                R.warn(f"the Toolkit in {dest} differs from the installed one: if you edited it, "
                       "install your changes with: bash install.sh (from that folder)")


def finish(R: Report) -> None:
    print()
    if R.fails:
        print(paint("1;31", f"{R.fails} problem(s), {R.warns} warning(s)"))
        sys.exit(1)
    print(paint("1;32", "All good") + (f" ({R.warns} warning(s))" if R.warns else ""))


# ------------------------------------------------------------------ restore: picking

def checklist(title: str, text: str, entries: list[tuple[str, str, bool]]) -> list[str] | None:
    if not entries:
        return []
    if have("whiptail"):
        cols, rows = shutil.get_terminal_size((100, 30))
        w, h = max(60, min(cols - 4, 150)), max(16, rows - 2)
        cmd = ["whiptail", "--title", title, "--notags", "--separate-output", "--checklist", text,
               str(h), str(w), str(max(5, min(len(entries), h - 10)))]
        for tag, label, on in entries:
            cmd += [tag, label[: w - 12], "ON" if on else "OFF"]
        r = subprocess.run(cmd, stderr=subprocess.PIPE, text=True)
        return None if r.returncode != 0 else r.stderr.split()
    state = {t: on for t, _, on in entries}
    while True:
        print(f"\n{title}\n{text}")
        for n, (t, label, _) in enumerate(entries, 1):
            print(f"  {n:3}. [{'x' if state[t] else ' '}] {label}")
        ans = input("Numbers to toggle (e.g. 3 7), Enter to continue, q to cancel: ").strip()
        if ans == "q":
            return None
        if not ans:
            return [t for t, _, _ in entries if state[t]]
        for tok in ans.split():
            if tok.isdigit() and 1 <= int(tok) <= len(entries):
                state[entries[int(tok) - 1][0]] ^= True


def item_picker(title: str, its: list[dict], keep: set[str]) -> set[str]:
    """Let the user untick single items of a bundle; returns the keys to keep."""
    if have("fzf"):
        binds = "load:select-all" + "".join(f"+pos({n + 1})+toggle" for n, i in enumerate(its) if key(i) not in keep) + "+first"
        r = subprocess.run(["fzf", "--multi", "--reverse", "--height=90%", "--delimiter=\t", "--with-nth=2..",
                            "--bind", binds, "--bind", "ctrl-a:select-all,ctrl-d:deselect-all", "--prompt", "filter> ",
                            "--header", f"{title}\nTab untick/tick · Ctrl-A all · Ctrl-D none · Enter done · Esc keep as it was"],
                           input="\n".join(f"{n}\t{describe(i)}" for n, i in enumerate(its)), stdout=subprocess.PIPE, text=True)
        if r.returncode == 130:
            return {key(i) for i in its if key(i) in keep}
        return {key(its[int(ln.split("\t", 1)[0])]) for ln in r.stdout.splitlines() if ln}
    chosen = checklist(title, "Untick what you don't want", [(str(n), describe(i), key(i) in keep) for n, i in enumerate(its)])
    return {key(i) for i in its if key(i) in keep} if chosen is None else {key(its[int(n)]) for n in chosen}


def menu(title: str, text: str, entries: list[tuple[str, str]], default: str | None = None) -> str:
    if have("whiptail"):
        cols, rows = shutil.get_terminal_size((100, 30))
        cmd = ["whiptail", "--title", title, "--menu", text, str(max(14, rows - 4)), str(max(60, min(cols - 4, 120))),
               str(max(4, min(len(entries), rows - 12)))]
        if default:
            cmd += ["--default-item", default]
        for tag, label in entries:
            cmd += [tag, label]
        r = subprocess.run(cmd, stderr=subprocess.PIPE, text=True)
        if r.returncode != 0:
            die("cancelled")
        return r.stderr.strip()
    for n, (tag, label) in enumerate(entries, 1):
        print(f"  {n}. {tag}  {label}")
    ans = input(f"{text} [{default or 1}]: ").strip()
    return entries[int(ans) - 1][0] if ans.isdigit() and 1 <= int(ans) <= len(entries) else (default or entries[0][0])


def confirm(q: str, default: bool = False) -> bool:
    ans = input(f"{q} [{'Y/n' if default else 'y/N'}] ").strip().lower()
    return ans.startswith("y") if ans else default


def get_source(a) -> tuple[Path, dict, dict | None, Path | None]:
    if a.manifest:
        d = Path(a.manifest)
        return d, load_manifest(d)[0], None, None
    snaps = restic_snapshots()
    if not snaps:
        die("the backup has no snapshots yet")
    hosts = sorted({s["hostname"] for s in snaps})
    src = a.from_host
    if not src:
        if len(hosts) == 1:
            src = hosts[0]
        elif sys.stdin.isatty():
            newest = {h: [s for s in snaps if s["hostname"] == h][-1]["_t"] for h in hosts}
            src = menu("Restore from which machine?", "Machines in the backup:",
                       [(h, f"newest backup {newest[h]:%Y-%m-%d %H:%M}") for h in hosts], HOST if HOST in hosts else None)
        else:
            die(f"several machines in the backup ({', '.join(hosts)}); choose one with --from")
    mine = [s for s in snaps if s["hostname"] == src]
    if not mine:
        die(f"no snapshots from {src}; machines in the backup: {', '.join(hosts)}")
    if a.snapshot == "latest":
        snap = mine[-1]
    elif a.snapshot == "pick":
        sid = menu(f"Which backup of {src}?", "Newest first:",
                   [(s["short_id"], f"{s['_t']:%Y-%m-%d %H:%M}  {','.join(s.get('tags') or [])}") for s in reversed(mine)])
        snap = next(s for s in mine if s["short_id"] == sid)
    else:
        snap = next((s for s in mine if s["id"].startswith(a.snapshot)), None) or die(f"no snapshot {a.snapshot} from {src}")
    log(f"fetching the system record of {src} from the backup of {snap['_t']:%Y-%m-%d %H:%M}")
    tmp = Path(tempfile.mkdtemp(prefix="sysbackup-src-", dir="/var/tmp"))
    r = restic(["restore", snap["id"], "--target", str(tmp), "--include", str(MANIFEST_DIR)])
    d = tmp / str(MANIFEST_DIR).lstrip("/")
    if r.returncode != 0 or not (d / "items.jsonl").exists():
        die("that snapshot has no system record: " + r.stderr.strip()[-300:])
    return d, load_manifest(d)[0], snap, tmp


def pick(items, bundles, defaults, prev, src_host) -> tuple[set[str], set[str]]:
    by = defaultdict(list)
    for it in items:
        by[it["bundle"]].append(it)
    use_prev = bool(prev) and confirm(f"Start from the choices you made on {prev['date'][:10]}?", True)
    entries = []
    for b, info in bundles.items():
        on, why = defaults[b]
        if use_prev:
            on = b in prev["bundles"]
        entries.append((b, f"{info['title']}  ({len(by[b])})" + (f"  - {why}" if why and not defaults[b][0] else ""), on))
    chosen = checklist(f"Restore from {src_host} onto {HOST}",
                       "Space ticks/unticks, Enter continues. Unticked bundles are skipped entirely. Nothing changes until you confirm the plan.",
                       entries)
    if chosen is None:
        die("cancelled")
    dropped = set(prev["deselected"]) if use_prev else set()
    multi = [b for b in bundles if b in chosen and len(by[b]) > 1]
    fine = checklist("Fine-tune", "Tick bundles you want to open and choose single items from (Enter with none ticked = take them whole):",
                     [(b, f"{bundles[b]['title']}  ({len(by[b])})", False) for b in multi]) or []
    for b in fine:
        its = by[b]
        keep = item_picker(bundles[b]["title"], its, {key(i) for i in its if key(i) not in dropped})
        dropped = (dropped - {key(i) for i in its}) | {key(i) for i in its if key(i) not in keep}
    return set(chosen), dropped


# ------------------------------------------------------------------ restore: plan & apply

def adapt(data: bytes, meta: dict) -> bytes:
    """Rewrite the backed-up user's home path (and `User=` lines) for this machine's user."""
    old_home, old_user = meta.get("home", ""), meta.get("user", "")
    if not old_home or (old_home == str(HOME) and old_user == USER.pw_name) or b"\0" in data:
        return data
    out = re.sub(re.escape(old_home.encode()) + rb"(?![\w.-])", str(HOME).encode(), data)
    if old_user and old_user != USER.pw_name:
        out = re.sub(rb"(?im)^([ \t]*(?:auto)?user(?:name)?[ \t]*=[ \t]*)" + re.escape(old_user.encode()) + rb"(?=[ \t]*\r?$)",
                     lambda m: m.group(1) + USER.pw_name.encode(), out)
    return out


def adapt_text(text: str, meta: dict) -> str:
    return adapt(text.encode(), meta).decode()


def saved_bytes(it: dict, src: Path, meta: dict) -> bytes:
    return adapt((src / "files" / it["id"].lstrip("/")).read_bytes(), meta)


def adapt_path(p: Path, meta: dict) -> int:
    """Adapt text files (<= 2 MiB) and symlinks restored from another user's home; returns how many changed."""
    if meta.get("home", "") in ("", str(HOME)):
        return 0
    targets = [p] if not p.is_dir() or p.is_symlink() else [
        Path(d) / n for d, dirs, files in os.walk(p) for n in files + [x for x in dirs if os.path.islink(os.path.join(d, x))]]
    changed = 0
    for f in targets:
        try:
            if f.is_symlink():
                t = os.readlink(f)
                if (nt := adapt_text(t, meta)) != t:
                    f.unlink()
                    os.symlink(nt, f)
                    changed += 1
            elif f.is_file() and f.stat().st_size <= 2 << 20:
                data = f.read_bytes()
                if (new := adapt(data, meta)) != data:
                    f.write_bytes(new)
                    changed += 1
        except OSError:
            pass
    return changed


def file_state(it: dict, src: Path, meta: dict) -> str:
    live = Path(it["id"])
    if "link" in it:
        if live.is_symlink() and os.readlink(live) == adapt_text(it["link"], meta):
            return "same"
        return "replace" if os.path.lexists(live) else "new"
    if not os.path.lexists(live):
        return "new"
    try:
        want = hashlib.sha256(saved_bytes(it, src, meta)).hexdigest()
        return "same" if not live.is_symlink() and sha_file(live) == want else "replace"
    except OSError:
        return "replace"


STORAGE_HOOKS = {"encrypt", "sd-encrypt", "lvm2", "mdadm", "mdadm_udev", "zfs", "resume", "btrfs", "bcache"}


def hooks_of(text: str) -> set[str]:
    m = re.search(r"^\s*HOOKS=\(([^)]*)\)", text, re.M)
    return set(m.group(1).split()) if m else set()


def boot_risk(it: dict, src: Path) -> str:
    """Why copying this initramfs config could leave the machine unbootable ('' if it can't)."""
    if not it["id"].startswith("/etc/mkinitcpio.conf"):
        return ""
    try:
        saved = hooks_of((src / "files" / it["id"].lstrip("/")).read_text())
    except OSError:
        return ""
    live = set()
    for p in [Path("/etc/mkinitcpio.conf"), *sorted(Path("/etc/mkinitcpio.conf.d").glob("*.conf"))]:
        try:
            live |= hooks_of(p.read_text())
        except OSError:
            pass
    lost = (live - saved) & STORAGE_HOOKS if saved else set()
    return f"this machine needs initramfs hooks the backup lacks ({', '.join(sorted(lost))}): copying it could stop it booting" if lost else ""


def unified_diff(live: Path, saved: Path) -> str:
    return sh(["diff", "-u", "--label", f"{live} (this machine)", "--label", f"{live} (backup)", str(live), str(saved)])


def make_plan(sel: list[dict], src: Path, meta: dict) -> tuple[dict, list]:
    P: dict[str, list] = defaultdict(list)
    skipped: list = []
    installed = set(sh(["pacman", "-Qq"]).split())
    sync = set(sh(["pacman", "-Slq"]).split())
    live_repos = set(sh(["pacman-conf", "--repo-list"]).split())
    new_repos = {i["id"] for i in sel if i["type"] == "repo" and i["id"] not in live_repos}
    groups_now = {g.gr_name for g in grp.getgrall() if USER.pw_name in g.gr_mem}
    _, kparams_now = cmdline_config()
    udir = HOME / ".config/systemd/user"
    tool_now = {"cargo": set(), "pipx": set(), "npm": set(), "flatpak": set()}
    for it in capture_tools():
        tool_now[it["type"]].add(it["id"])
    for it in sel:
        t, i = it["type"], it["id"]
        if t == "repo":
            if i in new_repos:
                P["repos"].append(it)
        elif t == "pkg":
            if i in installed:
                continue
            if i in sync or it.get("repo") in new_repos:
                P["packages"].append(it)
            else:
                skipped.append((it, "not in any repository"))
        elif t == "aur":
            if i not in installed:
                P["aur"].append(it)
        elif t == "file":
            if it.get("too_big"):
                skipped.append((it, "too big to keep in the record; copy it from the backup by hand"))
            elif REVIEW_ONLY.match(i):
                if file_state(it, src, meta) != "same":
                    P["review"].append(it)
            elif (st := file_state(it, src, meta)) != "same":
                why = boot_risk(it, src)
                if why:
                    P["review"].append({**it, "_why": why})
                else:
                    P["files"].append({**it, "_state": st})
        elif t == "path":
            P["data"].append(it)
        elif t == "group":
            if i not in groups_now:
                P["account"].append(it)
        elif t == "shell":
            if i != USER.pw_shell:
                P["account"].append(it)
        elif t == "linger":
            if not (Path("/var/lib/systemd/linger") / USER.pw_name).exists():
                P["account"].append(it)
        elif t == "mkdir":
            if not (HOME / it["rel"] if it.get("rel") else Path(i)).is_dir():
                P["dirs"].append(it)
        elif t in ("svc", "mask"):
            want = {"svc": "enabled", "mask": "masked"}[t]
            if sh(["systemctl", "is-enabled", "--", i]).strip() != want:
                P["services"].append(it)
        elif t == "usvc":
            if not list(udir.glob(f"*.wants/{i}")) + list(udir.glob(f"*.requires/{i}")):
                P["user-services"].append(it)
        elif t == "uunit":
            p = udir / i
            want = hashlib.sha256(adapt((src / "user-units" / i).read_bytes(), meta)).hexdigest()
            if not p.is_file() or sha_file(p) != want:
                P["user-services"].append(it)
        elif t == "umask":
            if not ((udir / i).is_symlink() and os.readlink(udir / i) == "/dev/null"):
                P["user-services"].append(it)
        elif t == "kparam":
            if i not in kparams_now:
                P["boot"].append(it)
        elif t in tool_now:
            if i not in tool_now[t]:
                P["tools"].append(it)
    return P, skipped


PHASES = [("repos", "Package repos"), ("packages", "Packages"), ("aur", "AUR (paru)"), ("files", "System files"),
          ("review", "Review by hand"), ("data", "From backup"), ("account", "Account"), ("services", "Services"),
          ("dirs", "Folders"), ("user-services", "User units"), ("boot", "Kernel params"), ("tools", "Language tools")]


def show_plan(P: dict, skipped: list, total: int, src: Path, meta: dict, header: str) -> Path:
    todo = sum(len(v) for v in P.values())
    print("\n" + paint("1", header))
    for ph, label in PHASES:
        its = P.get(ph, [])
        if not its:
            continue
        if ph == "files":
            new = sum(1 for i in its if i["_state"] == "new")
            line = f"{new} new, {len(its) - new} replaced: " + brief([i["id"] for i in its], 5)
        elif ph == "data":
            line = brief(["~/" + i["home"] if "home" in i else i["id"] for i in its], 6)
        elif ph == "review":
            line = brief([i["id"] for i in its], 4) + "  (diffs in the plan file, never copied automatically)"
        else:
            line = brief([i["id"] for i in its], 8)
        print(f"  {label:15} {len(its):4}  {line}")
    if skipped:
        print(f"  {paint('33', 'Skipped'):15}      " + brief([f"{i['id']} ({why})" for i, why in skipped], 4))
    print(f"  {'Already done':15} {total - todo - len(skipped):4}  items need no change")
    plan_file = (STATE_DIR if IS_ROOT else Path(tempfile.gettempdir())) / "restore-plan.txt"
    with open(plan_file, "w") as f:
        f.write(header + "\n\n")
        for ph, label in PHASES:
            for it in P.get(ph, []):
                f.write(f"[{label}] {describe(it)}" + (f"  ({it['_state']})" if "_state" in it else "")
                        + (f"\n    NOT COPIED: {it['_why']}" if "_why" in it else "") + "\n")
                if it["type"] == "file" and os.path.lexists(it["id"]) and "link" not in it:
                    with tempfile.NamedTemporaryFile("wb", suffix=".saved") as t:
                        t.write(saved_bytes(it, src, meta))
                        t.flush()
                        f.write(unified_diff(Path(it["id"]), Path(t.name)) + "\n")
        for it, why in skipped:
            f.write(f"[skipped] {describe(it)}: {why}\n")
    print(paint("2", f"\n  Full plan with diffs: {plan_file}"))
    if (src / REDOWNLOAD).is_file():
        text = (src / REDOWNLOAD).read_text()
        heads = []
        for ln in text.splitlines():
            if m := re.match(r"## Steam games: (\d+), (.+)$", ln):
                heads.append(f"{m.group(1)} Steam games ({m.group(2)})")
            elif m := re.match(r"- \*\*(.+?)\*\*", ln):
                heads.append(m.group(1))
        print(paint("1", "\n  Not in the backup, re-download after restoring: ") + brief(heads, 6))
        print(paint("2", f"  The full list is saved to ~/{REDOWNLOAD} when you apply the restore."))
    return plan_file


def install_one_by_one(pkgs: list[str], asdeps: bool = False) -> list[str]:
    if not pkgs:
        return []
    if not confirm(f"pacman failed. Install the {len(pkgs)} packages one at a time instead, skipping any that fail?", True):
        return [f"packages not installed: {brief(pkgs, 30)}"]
    flag = ["--asdeps"] if asdeps else []
    return [f"package {p} couldn't be installed" for p in pkgs if call(["pacman", "-S", "--needed", "--noconfirm", *flag, p]) != 0]


def apply_plan(P: dict, src: Path, meta: dict, snap: dict | None, a) -> None:
    yes = ["--noconfirm"] if a.yes else []
    failures: list[str] = []
    regen_boot = False

    def note(msg: str) -> None:
        warn(msg)
        failures.append(msg)

    if P["repos"]:
        log("adding package repositories")
        pc = Path("/etc/pacman.conf")
        text = pc.read_text()
        shutil.copy2(pc, str(pc) + ".pre-restore")
        for it in P["repos"]:
            if it["conf"]:
                conf = Path(it["conf"])
                if not conf.exists() or f"[{it['id']}]" not in conf.read_text():
                    with conf.open("a") as f:
                        f.write(it["section"])
                if not re.search(rf"^\s*Include\s*=\s*{re.escape(it['conf'])}\s*$", text, re.M):
                    text = text.rstrip("\n") + f"\n\nInclude = {it['conf']}\n"
            elif f"[{it['id']}]" not in text:
                text = text.rstrip("\n") + "\n\n" + it["section"]
        pc.write_text(text)
        keys = sorted({k for it in P["repos"] for k in it.get("keys", [])})
        if keys and (src / "repo-keys.asc").exists():
            call(["pacman-key", "--add", str(src / "repo-keys.asc")])
            for k in keys:
                if call(["pacman-key", "--lsign-key", k]) != 0:
                    note(f"couldn't trust signing key {k}")

    if P["repos"] or P["packages"] or P["aur"]:
        # Arch only supports installing on an up-to-date system: sync + upgrade + install in one go
        pk = [i["id"] for i in P["packages"] if not i.get("asdeps")]
        dep = [i["id"] for i in P["packages"] if i.get("asdeps")]
        log(f"updating the system and installing {len(pk) + len(dep)} packages")
        if call(["pacman", "-Syu", "--needed", *yes, *pk]) != 0:
            failures += install_one_by_one(pk)
        if dep and call(["pacman", "-S", "--needed", "--asdeps", *yes, *dep]) != 0:
            failures += install_one_by_one(dep, asdeps=True)

    if P["aur"]:
        log(f"building {len(P['aur'])} AUR packages with paru (as {USER.pw_name})")
        call(["pacman", "-S", "--needed", *yes, "paru", "base-devel"])
        for it in P["aur"]:
            if call(["paru", "-S", "--needed", *yes, *(["--asdeps"] if it.get("asdeps") else []), it["id"]], user=True) != 0:
                note(f"AUR package {it['id']} failed to build")

    if P["files"]:
        log(f"copying {len(P['files'])} system files")
        for it in P["files"]:
            dst, srcf = Path(it["id"]), src / "files" / it["id"].lstrip("/")
            try:
                dst.parent.mkdir(parents=True, exist_ok=True)
                if os.path.lexists(dst):
                    os.replace(dst, str(dst) + ".pre-restore")
                if "link" in it:
                    os.symlink(adapt_text(it["link"], meta), dst)
                else:
                    dst.write_bytes(saved_bytes(it, src, meta))
                    os.chmod(dst, int(it["mode"], 8))
                    u, _, g = it["owner"].partition(":")
                    try:
                        os.chown(dst, pwd.getpwnam(u).pw_uid, grp.getgrnam(g).gr_gid)
                    except KeyError:
                        warn(f"{dst}: owner {it['owner']} doesn't exist here, left as root")
            except OSError as e:
                note(f"couldn't write {dst}: {e.strerror}")
        touched = [i["id"] for i in P["files"]]
        if "/etc/locale.gen" in touched:
            call(["locale-gen"])
        if any(t.startswith(("/etc/udev/", "/usr/lib/udev/")) for t in touched):
            call(["udevadm", "control", "--reload"])
        regen_boot = any(t.startswith(("/etc/mkinitcpio", "/etc/modprobe.d/", "/usr/lib/firmware/")) for t in touched)

    for it in P["review"]:
        print(paint("1;33", f"\nReview by hand: {it['id']}")
              + (f"\n  NOT COPIED: {it['_why']}" if "_why" in it else " (disk UUIDs differ between installs, so this is never copied)"))
        print(unified_diff(Path(it["id"]), src / "files" / it["id"].lstrip("/")) or "  (missing here)")

    if P["data"]:
        failures += restore_data(P["data"], meta, snap)

    if P["account"]:
        log("account")
        add = [i["id"] for i in P["account"] if i["type"] == "group"]
        for g in add:
            try:
                grp.getgrnam(g)
            except KeyError:
                call(["groupadd", "-r", g])
        if add and call(["usermod", "-aG", ",".join(add), USER.pw_name]) != 0:
            note(f"couldn't add you to groups {', '.join(add)}")
        for it in P["account"]:
            if it["type"] == "shell":
                if Path(it["id"]).exists():
                    call(["usermod", "-s", it["id"], USER.pw_name])
                else:
                    note(f"login shell {it['id']} isn't installed; kept the current one")
            elif it["type"] == "linger":
                call(["loginctl", "enable-linger", USER.pw_name])

    if P["services"]:
        log("system services")
        call(["systemctl", "daemon-reload"])
        for verb, t in (("enable", "svc"), ("mask", "mask")):
            for u in [i["id"] for i in P["services"] if i["type"] == t]:
                if call(["systemctl", verb, "--", u]) != 0:
                    note(f"couldn't {verb} {u} (its package may not be installed)")

    for it in P["dirs"]:
        d = HOME / it["rel"] if it.get("rel") else Path(it["id"])
        d.mkdir(parents=True, exist_ok=True)
        if it.get("rel"):                                  # created as root: hand the new folders to you
            p = d
            while p != HOME and HOME in p.parents:
                os.chown(p, USER.pw_uid, USER.pw_gid)
                p = p.parent

    if P["user-services"]:
        log("user units")
        udir = HOME / ".config/systemd/user"
        udir.mkdir(parents=True, exist_ok=True)
        for it in P["user-services"]:
            if it["type"] == "uunit":
                (udir / it["id"]).parent.mkdir(parents=True, exist_ok=True)
                (udir / it["id"]).write_bytes(adapt((src / "user-units" / it["id"]).read_bytes(), meta))
            elif it["type"] == "umask":
                if os.path.lexists(udir / it["id"]):
                    (udir / it["id"]).unlink()
                os.symlink("/dev/null", udir / it["id"])
        sh(["chown", "-R", f"{USER.pw_name}:", str(HOME / ".config/systemd")])
        units = [i["id"] for i in P["user-services"] if i["type"] == "usvc"]
        if units and Path(f"/run/user/{USER.pw_uid}/bus").exists():
            sh(["systemctl", "--user", "daemon-reload"], user=True)
            if call(["systemctl", "--user", "enable", *units], user=True) != 0:
                note(f"couldn't enable user units {', '.join(units)}")
        elif units:
            note(f"you weren't logged in, so after logging in run: systemctl --user enable {' '.join(units)}")

    if P["boot"]:
        add = [i["id"] for i in P["boot"]]
        cfg, _ = cmdline_config()
        entry = next((e for e in CMDLINE_SOURCES if e[0] == cfg), None)
        if entry is None:
            note(f"no supported bootloader config found: add kernel parameters {' '.join(add)} by hand")
        else:
            path, rx, _ = entry
            log(f"adding kernel parameters to {path}: {' '.join(add)}")
            shutil.copy2(path, str(path) + ".pre-restore")
            new, n = re.subn(rx, lambda m: m.group(1) + (m.group(2) + " " + " ".join(add)).strip(), path.read_text(), count=1, flags=re.M)
            if n:
                path.write_text(new)
                regen_boot = True
            else:
                note(f"couldn't find the command line in {path}: add {' '.join(add)} by hand")

    if regen_boot:
        log("regenerating initramfs & boot entries")
        cfg, _ = cmdline_config()
        regen = next((e[2] for e in CMDLINE_SOURCES if e[0] == cfg), None)
        steps = [regen] if regen == ["limine-update"] else [["mkinitcpio", "-P"]] + ([regen] if regen and regen[0] != "mkinitcpio" else [])
        for st in steps:
            if have(st[0]) and call(st) != 0:
                note(f"`{' '.join(st)}` failed: fix this before rebooting")

    if P["tools"]:
        log("language tools")
        kinds = {i["type"] for i in P["tools"]}
        if "cargo" in kinds and have("rustup") and not sh(["rustup", "default"], user=True).strip():
            call(["rustup", "default", "stable"], user=True)
        if "flatpak" in kinds:
            call(["pacman", "-S", "--needed", *yes, "flatpak"])
            call(["flatpak", "remote-add", "--if-not-exists", "flathub", "https://dl.flathub.org/repo/flathub.flatpakrepo"])
        for it in P["tools"]:
            t, i = it["type"], it["id"]
            if t == "cargo":
                rc = call(["cargo", "install", "--locked", i], user=True)
            elif t == "pipx":
                rc = call(["pipx", "install", i], user=True)
            elif t == "npm":
                rc = call(["npm", "install", "-g", i])
            else:
                u = it.get("scope") == "user"
                rc = call(["flatpak", "install", "-y", *(["--user"] if u else []), it.get("origin", "flathub"), i], user=u)
            if rc != 0:
                note(f"{t} {i} couldn't be installed")

    if failures:
        print(paint("1;33", "\nFinished, but these need your attention:", _err_tty), file=sys.stderr)
        for f in failures:
            print("  - " + f, file=sys.stderr)
    if (src / REDOWNLOAD).is_file():
        (HOME / REDOWNLOAD).write_text((src / REDOWNLOAD).read_text())
        os.chown(HOME / REDOWNLOAD, USER.pw_uid, USER.pw_gid)
        log(f"what to re-download (Steam games, app files...): ~/{REDOWNLOAD}")
    log("done. Reboot so every service, module and kernel parameter comes up cleanly.")


def remap_owner(it: dict) -> str:
    """restic restores owners by number; a service user (e.g. openlinkhub) can have another number here.
    Re-owns what belonged to that user, by name. Returns a problem description or ''."""
    owner, uid, gid = it.get("owner", ""), it.get("uid"), it.get("gid")
    if not owner or uid is None or owner == "root:root" or not os.path.isdir(it["id"]):
        return ""
    u, _, g = owner.partition(":")
    try:
        nu, ng = pwd.getpwnam(u).pw_uid, grp.getgrnam(g).gr_gid
    except KeyError:
        return f"{it['id']}: its owner {owner} doesn't exist here; install the app that creates it, then restore again"
    if (nu, ng) == (uid, gid):
        return ""
    for root, dirs, files in os.walk(it["id"]):
        for n in [root, *(os.path.join(root, x) for x in dirs + files)]:
            st = os.lstat(n)
            os.lchown(n, nu if st.st_uid == uid else -1, ng if st.st_gid == gid else -1)
    log(f"{it['id']}: handed back to {owner} (user number {uid} there, {nu} here)")
    return ""


RCLONE_CONF = ".config/rclone/rclone.conf"   # holds the iCloud login this restore is using right now


def merge_rclone(keep: Path, old: Path) -> int:
    """Keep this machine's working remotes; add the backup's other remotes. Returns how many were added."""
    k, o = keep.read_text(), old.read_text()
    if "RCLONE_ENCRYPT" in k + o:
        return 0
    have = set(re.findall(r"^\[(.+)\]\s*$", k, re.M))
    add = [b for b in re.split(r"(?m)^(?=\[)", o) if (m := re.match(r"\[(.+)\]", b)) and m.group(1) not in have]
    if add:
        keep.write_text(k.rstrip("\n") + "\n\n" + "".join(add))
    return len(add)


def restore_data(its: list[dict], meta: dict, snap: dict | None) -> list[str]:
    problems: list[str] = []
    if snap is None:
        snaps = restic_snapshots(meta["host"])
        if not snaps:
            return [f"no backup of {meta['host']} to restore data from"]
        snap = snaps[-1]
    home_its = [i for i in its if "home" in i]
    sys_its = [i for i in its if "home" not in i]
    if home_its and sh(["pgrep", "-u", USER.pw_name, "plasmashell"]).strip():
        warn("Plasma is running for you: it rewrites its settings when you log out, undoing part of this restore.")
        if not confirm("Best done from a TTY (Ctrl+Alt+F3) while logged out of the desktop. Restore home files anyway?"):
            home_its = []
            problems.append("home files weren't restored (Plasma was running): run the restore again from a TTY")
    work = Path(tempfile.mkdtemp(prefix="sysbackup-restore-", dir="/var/tmp"))
    try:
        if sys_its:
            log(f"restoring {len(sys_its)} system paths from the backup")
            (work / "sys.txt").write_text("\n".join(i["id"] for i in sys_its) + "\n")
            if restic(["restore", snap["id"], "--target", "/", "--include-file", str(work / "sys.txt")], quiet=False).returncode != 0:
                problems.append("restoring system paths from the backup reported errors (see above)")
            for it in sys_its:
                if problem := remap_owner(it):
                    problems.append(problem)
            prints = Path("/var/lib/fprint")
            if any(i["id"] == str(prints) for i in sys_its) and meta["user"] != USER.pw_name:
                if (prints / meta["user"]).is_dir() and not (prints / USER.pw_name).exists():
                    (prints / meta["user"]).rename(prints / USER.pw_name)        # fprintd keeps prints per user
                    log(f"moved {meta['user']}'s enrolled fingerprints to {USER.pw_name}")
        if home_its:
            log(f"restoring {len(home_its)} home paths from the backup")
            src_home = meta["home"]
            (work / "home.txt").write_text("\n".join(f"{src_home}/{i['home']}" for i in home_its) + "\n")
            if restic(["restore", snap["id"], "--target", str(work / "t"), "--include-file", str(work / "home.txt")], quiet=False).returncode != 0:
                problems.append("restoring home files from the backup reported errors (see above)")
            base = work / "t" / src_home.lstrip("/")
            if (HOME / RCLONE_CONF).exists() and (base / RCLONE_CONF).is_file():
                added = merge_rclone(HOME / RCLONE_CONF, base / RCLONE_CONF)
                (base / RCLONE_CONF).unlink()
                log(f"kept this machine's working iCloud login" + (f", added {added} other remote(s) from the backup" if added else ""))
            adapted = 0
            for it in home_its:
                s, d = base / it["home"], HOME / it["home"]
                if not os.path.lexists(s):
                    continue
                try:
                    d.parent.mkdir(parents=True, exist_ok=True)
                    if s.is_dir() and not s.is_symlink():
                        shutil.copytree(s, d, symlinks=True, dirs_exist_ok=True)
                    else:
                        if os.path.lexists(d) and (d.is_symlink() or not d.is_dir()):
                            d.unlink()
                        shutil.copy2(s, d, follow_symlinks=False)
                    adapted += adapt_path(d, meta)       # before chown: it may recreate symlinks as root
                    sh(["chown", "-R", "-h", f"{USER.pw_name}:", str(d)])
                except OSError as e:
                    problems.append(f"couldn't restore ~/{it['home']}: {e}")
            if adapted:
                log(f"pointed {adapted} restored files from {src_home} to {HOME}")
            if any(i["home"] == ".local/share/kwalletd" for i in home_its):
                problems.append("KDE Wallet was restored: it unlocks with the login password you had when it was created. "
                                "If yours is different now, enter the old one once, then change it in KDE Wallet Manager "
                                "(browsers keep their saved passwords in it)")
    finally:
        shutil.rmtree(work, ignore_errors=True)
    for p in problems:
        warn(p)
    return problems


def cmd_restore(a) -> None:
    if not IS_ROOT and not a.dry_run:
        die("run it with sudo (add --dry-run to only see the plan)")
    src, meta, snap, tmp = get_source(a)
    try:
        _, items, bundles = load_manifest(src)
        log("looking at this machine's hardware")
        facts = target_facts()
        defaults = {b: when_applies(info["when"], facts, meta) for b, info in bundles.items()}
        sel_file = CONF_DIR / "selections" / f"{meta['host']}.json"
        try:
            prev = json.loads(sel_file.read_text())
        except (OSError, ValueError):
            prev = None
        if a.defaults or not sys.stdin.isatty():
            chosen, dropped = {b for b, (ok, _) in defaults.items() if ok}, set()
        else:
            chosen, dropped = pick(items, bundles, defaults, prev, meta["host"])
        if IS_ROOT:
            sel_file.parent.mkdir(parents=True, exist_ok=True)
            sel_file.write_text(json.dumps({"date": now_iso(), "target": HOST, "bundles": sorted(chosen),
                                            "deselected": sorted(dropped)}, indent=2) + "\n")
        sel = [i for i in items if i["bundle"] in chosen and key(i) not in dropped]
        P, skipped = make_plan(sel, src, meta)
        when = f", backup of {snap['_t']:%Y-%m-%d %H:%M}" if snap else ""
        header = f"Restore plan: {meta['host']}{when} -> {HOST}  ({len(chosen)}/{len(bundles)} bundles)"
        if meta["home"] != str(HOME) or meta["user"] != USER.pw_name:
            header += f"\n  {meta['user']}'s files go to {USER.pw_name} ({HOME}); paths inside them are adapted"
        show_plan(P, skipped, len(sel), src, meta, header)
        if a.dry_run or not any(P.values()):
            return
        if not confirm("\nApply this plan?"):
            return
        RESTORING.parent.mkdir(parents=True, exist_ok=True)
        if not RESTORING.exists():
            RESTORING.write_text(now_iso() + "\n")
        apply_plan(defaultdict(list, P), src, meta, snap, a)
        RESTORING.unlink(missing_ok=True)
        log("automatic backups are active again")
    finally:
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)


# ------------------------------------------------------------------ main

def main() -> None:
    ap = argparse.ArgumentParser(prog="sysbackup", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True, metavar="command")
    p = sub.add_parser("capture", help="record the system state")
    p.add_argument("--out", type=Path, default=MANIFEST_DIR)
    p.add_argument("-q", "--quiet", action="store_true")
    p = sub.add_parser("backup", help="capture + back up to the cloud")
    p.add_argument("--tag", default="auto")
    p.add_argument("-q", "--quiet", action="store_true")
    p.add_argument("--force", action="store_true", help="back up even while a restore is in progress")
    p = sub.add_parser("verify", help="check the backup is current, complete and restorable")
    p.add_argument("--deep", action="store_true", help="also re-download a sample of the backup and test a restore")
    p.add_argument("--full", action="store_true", help="like --deep, but re-download and verify all backup data")
    p.add_argument("--no-cloud", action="store_true", help="skip the repository checks")
    p = sub.add_parser("restore", help="pick bundles from a backed-up machine and apply them here")
    p.add_argument("--from", dest="from_host", help="machine to restore from (default: ask)")
    p.add_argument("--snapshot", default="latest", help="latest (default), pick, or a snapshot ID")
    p.add_argument("--manifest", help="use a local system record instead of fetching it from the backup")
    p.add_argument("--dry-run", action="store_true", help="only show the plan")
    p.add_argument("--defaults", action="store_true", help="no menus: take the automatic choices")
    p.add_argument("--yes", action="store_true", help="don't let pacman/paru ask for confirmation")
    p = sub.add_parser("bundles", help="show the bundles in a system record")
    p.add_argument("--manifest")
    p.add_argument("--all", action="store_true")
    sub.add_parser("status", help="last backups and recent system changes")
    p = sub.add_parser("restic", help="run a restic command on the backup, e.g. `sysbackup restic snapshots`")
    p.add_argument("args", nargs=argparse.REMAINDER)
    sub.add_parser("pacman-hook", help=argparse.SUPPRESS)
    p = sub.add_parser("notify", help=argparse.SUPPRESS)
    p.add_argument("unit")
    a = ap.parse_args()
    {"capture": cmd_capture, "backup": cmd_backup, "verify": cmd_verify, "restore": cmd_restore,
     "bundles": cmd_bundles, "status": cmd_status, "restic": cmd_restic, "pacman-hook": cmd_pacman_hook, "notify": cmd_notify}[a.cmd](a)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
