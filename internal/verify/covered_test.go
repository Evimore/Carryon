package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

const showSync = "systemctl show -p ExecStartPre -p ExecStart -p ExecStartPost -- sync-it.service"

func TestUnitPrograms(t *testing.T) {
	handMade := record.Item{Type: record.TypeSvc, ID: "sync-it.service", UnitFile: "/etc/systemd/system/sync-it.service"}
	show := "ExecStartPre=\nExecStart={ path=/opt/sync/bin/sync-it ; argv[]=/opt/sync/bin/sync-it --all ; ignore_errors=no }\nExecStartPost=\n"
	tests := []struct {
		name     string
		setup    func(env *testEnv, v *verifier)
		level    Level
		text     string
		details  string
		userUnit string // a user unit file for alice, in the record
	}{
		{name: "not backed up", level: Problem, text: "aren't in the backup", details: "sync-it.service runs /opt/sync/bin/sync-it"},
		{name: "owned by a package", setup: func(env *testEnv, _ *verifier) {
			env.pm.owners = map[string]string{"/opt/sync/bin/sync-it": "sync-it"}
		}, level: OK, text: "your 1 hand-made units start are all backed up"},
		{name: "copied into the record", setup: func(env *testEnv, _ *verifier) {
			write(t, filepath.Join(env.dirs.RecordDir(), capture.FilesDir, "opt/sync/bin/sync-it"), "#!/bin/sh\n")
		}, level: OK, text: "all backed up"},
		{name: "in extra paths", setup: func(_ *testEnv, v *verifier) {
			v.opt.Config.Backup.ExtraPaths = append(v.opt.Config.Backup.ExtraPaths, "/opt/sync")
		}, level: OK, text: "all backed up"},
		{name: "user unit runs a program in the home", userUnit: "[Service]\nExecStart=%h/bin/tool --serve\n",
			level: OK, text: "your 2 hand-made units"},
		{name: "user unit runs an excluded program", userUnit: "[Service]\nExecStart=-%h/.cache/tool\n",
			setup: func(_ *testEnv, v *verifier) { v.setExcludes([]string{"/home/*/.cache"}) },
			level: Problem, text: "aren't in the backup", details: "user:alice:tool.service runs /home/alice/.cache/tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			env.pm.owners = map[string]string{}
			env.run.On(showSync, sysexec.Response{Stdout: show})
			v := env.withItems(env.options(), handMade)
			if tt.userUnit != "" {
				write(t, filepath.Join(env.dirs.RecordDir(), capture.UserUnitsDir, "alice", "tool.service"), tt.userUnit)
				env.pm.owners["/opt/sync/bin/sync-it"] = "sync-it"
			}
			if tt.setup != nil {
				tt.setup(env, v)
			}
			v.checkUnitPrograms(context.Background())
			l := want(t, v.rep, tt.level, tt.text)
			if tt.details != "" && strings.Join(l.Details, "\n") != tt.details {
				t.Errorf("details = %q, want %q", l.Details, tt.details)
			}
		})
	}
}

func TestUserUnitFiles(t *testing.T) {
	env := newEnv(t)
	write(t, filepath.Join(env.dirs.RecordDir(), capture.UserUnitsDir, "alice", "tool.service"), "[Service]\nExecStart=/usr/local/bin/tool\n")
	v := env.verifier(env.options())
	v.rec = &record.Record{Meta: record.Meta{Format: record.FormatCurrent, User: "alice", Home: "/home/alice",
		Users: []record.User{{Name: "alice", Home: "/home/alice"}}}}
	got := v.userUnitFiles()
	if len(got) != 1 || got[0].user != "alice" || filepath.Base(got[0].file) != "tool.service" {
		t.Fatalf("user unit files = %+v", got)
	}
}

func TestDKMS(t *testing.T) {
	status := "examplefs/2.1.0, 6.11.1-arch1-1, x86_64: installed\nexamplefs/2.1.0, 6.6.50-1-lts, x86_64: installed\nwifi-driver/0.9: added\n"
	tests := []struct {
		name   string
		owners map[string]string
		inRec  bool
		level  Level
		text   string
	}{
		{name: "from packages", owners: map[string]string{"/usr/src/examplefs-2.1.0": "examplefs-dkms", "/usr/src/wifi-driver-0.9": "wifi-driver-dkms"},
			level: OK, text: "come from packages (examplefs-dkms, wifi-driver-dkms)"},
		{name: "by hand", owners: map[string]string{"/usr/src/examplefs-2.1.0": "examplefs-dkms"},
			level: Problem, text: "aren't from a package or in the backup"},
		{name: "in the record", owners: map[string]string{"/usr/src/examplefs-2.1.0": "examplefs-dkms"}, inRec: true,
			level: OK, text: "come from packages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			env.pm.owners = tt.owners
			env.run.On("dkms status", sysexec.Response{Stdout: status})
			if tt.inRec {
				write(t, filepath.Join(env.dirs.RecordDir(), capture.FilesDir, "usr/src/wifi-driver-0.9/dkms.conf"), "PACKAGE_NAME=wifi-driver\n")
			}
			v := env.withItems(env.options())
			v.checkDKMS(context.Background())
			l := want(t, v.rep, tt.level, tt.text)
			if tt.level == Problem && strings.Join(l.Details, ",") != "/usr/src/wifi-driver-0.9: no package provides it" {
				t.Errorf("details = %q", l.Details)
			}
		})
	}
	t.Run("no dkms", func(t *testing.T) {
		env := newEnv(t)
		v := env.withItems(env.options())
		v.checkDKMS(context.Background())
		if len(v.rep.Sections) != 0 {
			t.Errorf("report:\n%s", levels(v.rep))
		}
	})
}

func TestKernelParams(t *testing.T) {
	cmdline := "BOOT_IMAGE=/vmlinuz-linux root=UUID=0000 rw quiet splash example.mode=1\n"
	tests := []struct {
		name  string
		saved []string
		level Level
		bad   string
	}{
		{name: "all saved", saved: []string{"quiet", "splash", "example.mode=1"}, level: OK},
		{name: "one missing", saved: []string{"quiet", "splash"}, level: Problem, bad: "example.mode=1: active now but not saved in a config file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			write(t, filepath.Join(env.root, "proc/cmdline"), cmdline)
			var items []record.Item
			for _, p := range tt.saved {
				items = append(items, record.Item{Type: record.TypeKparam, ID: p, Source: "/etc/kernel/cmdline"})
			}
			v := env.withItems(env.options(), items...)
			v.checkKernelParams()
			l := want(t, v.rep, tt.level, "kernel")
			if strings.Join(l.Details, ",") != tt.bad {
				t.Errorf("details = %q, want %q", l.Details, tt.bad)
			}
		})
	}
}

func TestMounts(t *testing.T) {
	base := dest.MountTable{
		{MountPoint: "/", FSType: "ext4"},
		{MountPoint: "/proc", FSType: "proc"},
		{MountPoint: "/tmp", FSType: "tmpfs"},
		{MountPoint: "/run/media/alice/stick", FSType: "vfat"},
		{MountPoint: "/var/lib/docker/overlay2/x/merged", FSType: "overlay"},
	}
	tests := []struct {
		name   string
		mount  dest.Mount
		fstab  string
		units  map[string]string // record path -> content
		level  Level
		detail string
	}{
		{name: "in fstab", mount: dest.Mount{MountPoint: "/mnt/my data", FSType: "ext4"}, fstab: "UUID=1 /mnt/my\\040data ext4 defaults 0 2\n", level: OK},
		{name: "by a system mount unit", mount: dest.Mount{MountPoint: "/mnt/data", FSType: "ext4"},
			units: map[string]string{"files/etc/systemd/system/mnt-data.mount": "[Mount]\nWhat=/dev/sdb1\n"}, level: OK},
		{name: "by a user unit", mount: dest.Mount{MountPoint: "/home/alice/cloud", FSType: "fuse.rclone"},
			units: map[string]string{"user-units/alice/cloud.service": "[Service]\nExecStart=/usr/bin/rclone mount remote: %h/cloud\n"}, level: OK},
		{name: "nothing", mount: dest.Mount{MountPoint: "/mnt/data", FSType: "ext4"}, level: Problem,
			detail: "/mnt/data (ext4): nothing saved mounts this again"},
		{name: "only pseudo filesystems", level: OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			opt := env.options()
			opt.System.Mounts = append(dest.MountTable{}, base...)
			if tt.mount.MountPoint != "" {
				opt.System.Mounts = append(opt.System.Mounts, tt.mount)
			}
			write(t, filepath.Join(env.dirs.RecordDir(), "files/etc/fstab"), "UUID=0 / ext4 rw 0 1\n# UUID=9 /mnt/old ext4 rw 0 2\n"+tt.fstab)
			for p, body := range tt.units {
				write(t, filepath.Join(env.dirs.RecordDir(), p), body)
			}
			v := env.withItems(opt)
			v.checkMounts()
			l := want(t, v.rep, tt.level, "mount")
			if strings.Join(l.Details, ",") != tt.detail {
				t.Errorf("details = %q, want %q", l.Details, tt.detail)
			}
		})
	}
}

func TestModules(t *testing.T) {
	env := newEnv(t)
	write(t, filepath.Join(env.root, "proc/modules"),
		"ext4 1 2 - Live 0x0\nexamplefs 3 0 - Live 0x0\nhomebrew 1 0 - Live 0x0 (OE)\nbuiltin_thing 0 0 - Live 0x0\n")
	env.run.On("modinfo -n ext4", sysexec.Response{Stdout: "/lib/modules/6.11.1-arch1-1/kernel/fs/ext4/ext4.ko.zst\n"})
	env.run.On("modinfo -n examplefs", sysexec.Response{Stdout: "/lib/modules/6.11.1-arch1-1/updates/dkms/examplefs.ko.zst\n"})
	env.run.On("modinfo -n homebrew", sysexec.Response{Stdout: "/lib/modules/6.11.1-arch1-1/extra/homebrew.ko\n"})
	env.run.On("modinfo -n builtin_thing", sysexec.Response{Stdout: "(builtin)\n"})
	env.pm.owners = map[string]string{"/lib/modules/6.11.1-arch1-1/kernel/fs/ext4/ext4.ko.zst": "linux"}
	v := env.withItems(env.options())
	v.checkModules(context.Background())
	l := want(t, v.rep, Warning, "kernel modules don't come from a package")
	if strings.Join(l.Details, ",") != "homebrew: /lib/modules/6.11.1-arch1-1/extra/homebrew.ko" {
		t.Errorf("details = %q", l.Details)
	}

	env.pm.owners["/lib/modules/6.11.1-arch1-1/extra/homebrew.ko"] = "homebrew-module"
	v = env.withItems(env.options())
	v.checkModules(context.Background())
	want(t, v.rep, OK, "all come from packages or DKMS")
}

func TestEnableLinks(t *testing.T) {
	env := newEnv(t)
	wants := filepath.Join(env.root, "etc/systemd/system/multi-user.target.wants")
	write(t, filepath.Join(env.root, "usr/lib/systemd/system/sshd.service"), "[Service]\n")
	userWants := filepath.Join(env.root, "home/alice/.config/systemd/user/default.target.wants")
	for _, d := range []string{wants, userWants} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		filepath.Join(wants, "sshd.service"):         "/usr/lib/systemd/system/sshd.service",
		filepath.Join(wants, "removed.service"):      "/usr/lib/systemd/system/removed.service",
		filepath.Join(userWants, "old-sync.service"): "../old-sync.service",
	}
	for l, target := range links {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	v := env.withItems(env.options())
	v.checkEnableLinks()
	l := want(t, v.rep, Warning, "enable links point to units that are gone")
	got := strings.Join(l.Details, "\n")
	wantDetails := "/etc/systemd/system/multi-user.target.wants/removed.service -> /usr/lib/systemd/system/removed.service\n" +
		"/home/alice/.config/systemd/user/default.target.wants/old-sync.service -> ../old-sync.service"
	if got != wantDetails {
		t.Errorf("details:\n%s\nwant:\n%s", got, wantDetails)
	}

	os.Remove(filepath.Join(wants, "removed.service"))
	os.Remove(filepath.Join(userWants, "old-sync.service"))
	v = env.withItems(env.options())
	v.checkEnableLinks()
	want(t, v.rep, OK, "No leftover enable links")
}

func TestSkippedFiles(t *testing.T) {
	env := newEnv(t)
	write(t, filepath.Join(env.dirs.RecordDir(), capture.SkippedFile), "/etc/shadow-ish\n/root\n")
	v := env.withItems(env.options())
	v.checkCoverage(context.Background())
	l := want(t, v.rep, Warning, "couldn't be read when the record was captured")
	if strings.Join(l.Details, ",") != "/etc/shadow-ish,/root" {
		t.Errorf("details = %q", l.Details)
	}
}
