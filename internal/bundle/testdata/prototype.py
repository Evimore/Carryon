#!/usr/bin/env python3
"""Runs the prototype's assign_bundles (legacy/sysbackup.py) on desktop.items.jsonl and writes
prototype.golden.txt, which TestMatchesPrototype compares Carryon's grouping with.

The prototype had one user, so only alice's items and the system's are given to it, without their
"user" field. Every place it would look at the machine is answered from machine.json: programs (sh,
shutil.which, file_owners, device_name, devices_for_module, usb_present) are replaced, and the
system files it reads (/proc, /sys, /home, /usr/lib/udev) are redirected into a temporary folder.
Nothing on the real system is read or changed.

Run from the repository root:

    PYTHONHASHSEED=0 python3 -I internal/bundle/testdata/prototype.py
"""
import importlib.util
import json
import os
import pathlib
import shutil
import sys
import tempfile
import types

sys.dont_write_bytecode = True  # keep legacy/ clean

HERE = pathlib.Path(__file__).resolve().parent
LEGACY = HERE.parents[2] / "legacy" / "sysbackup.py"

tmp = pathlib.Path(tempfile.mkdtemp(prefix="carryon-prototype-"))
conf = tmp / "conf"
conf.mkdir()
shutil.copy(HERE / "bundles.conf", conf / "bundles.local")
os.environ["SYSBACKUP_CONF_DIR"] = str(conf)
os.environ["SYSBACKUP_STATE_DIR"] = str(tmp / "state")

spec = importlib.util.spec_from_file_location("sysbackup", LEGACY)
sb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sb)

M = json.loads((HERE / "machine.json").read_text())

# the fake system tree
root = tmp / "root"
for path, text in M["files"].items():
    p = root / path.lstrip("/")
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text)
(root / "proc").mkdir()
(root / "proc/modules").write_text("".join(f"{m} 16384 0 - Live 0x0\n" for m in M["loaded_modules"]))
for mod, holders in M["holders"].items():
    for h in holders:
        (root / "sys/module" / mod / "holders" / h).mkdir(parents=True)

REDIRECT = ("/proc/", "/sys/", "/home/", "/usr/lib/udev/")
RealPath = type(pathlib.Path())


class FakePath(RealPath):
    def __new__(cls, *args):
        p = str(pathlib.PurePosixPath(*args))
        if p.startswith(REDIRECT) or p in ("/proc", "/sys"):
            return RealPath(root, p.lstrip("/"))
        return RealPath(p)


def fake_sh(cmd, **kw):
    if cmd[:2] == ["modinfo", "-n"]:
        return f"/usr/lib/modules/x/{cmd[2]}.ko.zst\n" if cmd[2] in M["modules_exist"] else ""
    if cmd[:3] == ["modinfo", "-F", "firmware"]:
        return "".join(f + "\n" for f in M["firmware"].get(cmd[3], []))
    if cmd[:2] == ["pacman", "-Ql"]:
        return "".join(f"{p} {f}\n" for p in cmd[2:] for f in M["package_files"].get(p, []))
    raise AssertionError(f"unplanned program: {cmd}")


sb.Path = FakePath
sb.sh = fake_sh
sb.shutil = types.SimpleNamespace(which=lambda prog: M["which"].get(prog))
sb.file_owners = lambda paths: {p: M["owners"][p] for p in paths if p in M["owners"]}
sb.device_name = lambda hwid: M["device_names"].get(hwid, hwid)
sb.devices_for_module = lambda mod: set(M["devices"].get(mod, []))
sb.usb_present = lambda: {d.removeprefix("usb:") for d in M["usb_present"]}
sb.HOME = RealPath("/home/alice")
sb.HOST = M["host"]

stage = tmp / "stage"
for path, text in M["record_files"].items():
    p = stage / "files" / path.lstrip("/")
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text)

items = []
for line in (HERE / "desktop.items.jsonl").read_text().splitlines():
    it = json.loads(line)
    if it.pop("user", "alice") == "alice":
        items.append(it)

ctx = {"repo_of": M["repo_of"], "stage": stage, "fp_readers": M["fingerprint_readers"],
       "bt_macs": M["bluetooth_macs"]}
bundles = sb.assign_bundles(items, ctx)
items.sort(key=lambda i: (sb.rank(i["bundle"]), i["bundle"], i["type"], i["id"]))

lines = [f'bundle {b} | {v["when"]} | {v["title"]}' for b, v in bundles.items()]
lines += [f'item {i["bundle"]} {i["type"]}:{i["id"]}' for i in items]
(HERE / "prototype.golden.txt").write_text("\n".join(lines) + "\n")
shutil.rmtree(tmp)
print(f"wrote {len(items)} items in {len(bundles)} bundles")
