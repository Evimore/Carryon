package capture

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/sysexec"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// asUser is the command line a Fake with Root set sees for cmd run as a user.
func asUser(name string, uid int, cmd string) string {
	home := "/home/" + name
	rt := "/run/user/" + strconv.Itoa(uid)
	return "runuser -u " + name + " -- env HOME=" + home + " USER=" + name + " XDG_RUNTIME_DIR=" + rt +
		" DBUS_SESSION_BUS_ADDRESS=unix:path=" + rt + "/bus PATH=" + home + "/.cargo/bin:" + home +
		"/.local/bin:/usr/bin " + cmd
}

// desktopRunner answers the programs capture runs on the machine in testdata/system.txt.
func desktopRunner() *sysexec.Fake {
	f := &sysexec.Fake{Root: true, Path: "/usr/bin"}
	f.On("systemctl list-unit-files --no-legend --no-pager --state=enabled,masked", sysexec.Response{Stdout: `sshd.service                       enabled  disabled
rclone-backup.service              enabled  disabled
carryon-backup.timer               enabled  enabled
getty@.service                     enabled  enabled
old-thing.service                  enabled  disabled
systemd-networkd.service           masked   enabled
NetworkManager-wait-online.service masked   disabled
`})
	f.On("systemctl show -p LoadState -p FragmentPath -- getty@tty2.service old-thing.service rclone-backup.service sshd.service",
		sysexec.Response{Stdout: `FragmentPath=/usr/lib/systemd/system/getty@.service
LoadState=loaded

FragmentPath=
LoadState=not-found

FragmentPath=/etc/systemd/system/rclone-backup.service
LoadState=loaded

FragmentPath=/usr/lib/systemd/system/sshd.service
LoadState=loaded
`})
	f.On("modinfo -n nvidia-drm", sysexec.Response{Stdout: "/usr/lib/modules/6.11.5-arch1-1/extramodules/nvidia-drm.ko.zst\n"})
	f.On("npm ls -g --depth=0 --parseable", sysexec.Response{Stdout: "/usr/lib\n/usr/lib/node_modules/corepack\n/usr/lib/node_modules/npm\n/usr/lib/node_modules/typescript\n"})
	f.On("flatpak list --app --system --columns=application,origin", sysexec.Response{Stdout: "org.example.App\tflathub\n"})
	f.On(asUser("alice", 1000, "flatpak list --app --user --columns=application,origin"), sysexec.Response{Stdout: "com.example.Notes\tflathub\n"})
	f.On(asUser("bob", 1001, "flatpak list --app --user --columns=application,origin"), sysexec.Response{})
	f.On(asUser("alice", 1000, "cargo install --list"), sysexec.Response{Stdout: "bat v0.24.0:\n    bat\nripgrep v14.1.0:\n    rg\n"})
	f.On(asUser("bob", 1001, "pipx list --short"), sysexec.Response{Stdout: "httpie 3.2.2\n"})
	return f
}

// desktopOptions captures testdata/system.txt into a new folder.
func desktopOptions(t *testing.T, root string) Options {
	cfg := config.Default()
	cfg.MachineName = "desktop"
	cfg.Backup.ExtraPaths = []string{"/root", "/srv", "/usr/local", "/var/lib/jellyfin", "/home/alice/Music", "/var/lib/missing"}
	r, problems, err := rules.ParseBundleRules(strings.NewReader("title home-files Home: documents and music\nput music path ~/Music\n"), "bundles.conf")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	return Options{
		BundleRules: r,
		Dir:         filepath.Join(t.TempDir(), "record"),
		Root:        root,
		Runner:      desktopRunner(),
		Packages:    desktopPM(),
		Config:      cfg,
		Excludes:    []string{"/home/*/.cache", "/home/alice/.local/share/Steam", "node_modules"},
		IsRoot:      true,
		Now:         func() time.Time { return time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC) },
	}
}

// capture runs a capture with the tree's owners and an optional unreadable file.
func capture(t *testing.T, root string, opt Options, unreadable ...string) *Result {
	t.Helper()
	c, err := newCapturer(opt)
	if err != nil {
		t.Fatal(err)
	}
	c.owner = treeOwner(root, map[string]int{"home/alice/.config/systemd/user/planted.service": 1001})
	c.readFile = func(p string) ([]byte, error) {
		rel, _ := filepath.Rel(root, p)
		if slices.Contains(unreadable, rel) {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return os.ReadFile(p)
	}
	res, err := c.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/capture -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run go test ./internal/capture -update after checking the change):\n%s",
			name, lineDiff(string(want), string(got)))
	}
}

// lineDiff lists the lines only in want (-) or only in got (+).
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for _, l := range w {
		if !slices.Contains(g, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range g {
		if !slices.Contains(w, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

func TestCaptureDesktop(t *testing.T) {
	root := buildTree(t, "system.txt")
	big := filepath.Join(root, "opt/big/disk.img")
	mkdir(t, filepath.Dir(big))
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, MaxFile+1); err != nil { // sparse: takes no space
		t.Fatal(err)
	}
	opt := desktopOptions(t, root)
	opt.Nightly = true
	res := capture(t, root, opt)

	items, err := os.ReadFile(filepath.Join(opt.Dir, record.ItemsFile))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "desktop.items.jsonl", items)

	rec, err := record.Load(opt.Dir)
	if err != nil {
		t.Fatalf("the staged record doesn't load: %v", err)
	}
	if !reflect.DeepEqual(rec.Items, res.Items) {
		t.Error("Result.Items differ from items.jsonl")
	}
	m := rec.Meta
	wantMeta := record.Meta{
		Carryon: m.Carryon, Chassis: "desktop", Complete: true, Distro: "arch", Format: record.FormatCurrent,
		Home: "/home/alice", Host: "desktop", Kernel: "6.11.5-arch1-1", Machine: m.Machine, MachineName: "desktop",
		User: "alice", Users: []record.User{{Home: "/home/alice", Name: "alice"}, {Home: "/home/bob", Name: "bob"}},
	}
	if !reflect.DeepEqual(m, wantMeta) || len(m.Machine) != 16 || m.Carryon == "" {
		t.Errorf("meta = %+v\nwant %+v", m, wantMeta)
	}
	if !reflect.DeepEqual(rec.Bundles, res.Bundles) {
		t.Error("Result.Bundles differ from bundles.json")
	}
	for _, it := range rec.Items {
		if _, ok := rec.Bundles.Get(it.Bundle); !ok {
			t.Errorf("%s is in bundle %q, which bundles.json doesn't list", it.Key(), it.Bundle)
		}
	}

	// the staged folder
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(opt.Dir, name))
		if err != nil {
			t.Error(err)
		}
		return string(b)
	}
	if got := read("files/etc/ssh/sshd_config"); got != "PasswordAuthentication no\n" {
		t.Errorf("copied sshd_config = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(opt.Dir, "files/etc/sudoers.d/10-wheel")); err != nil || fi.Mode().Perm() != 0o440 {
		t.Errorf("copied sudoers file: %v, %v; want mode 0440", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(opt.Dir, "files/usr/local/bin/hello")); err != nil || l != "/opt/hello/bin/hello" {
		t.Errorf("copied symlink = %q, %v", l, err)
	}
	if _, err := os.Lstat(filepath.Join(opt.Dir, "files/opt/big/disk.img")); err == nil {
		t.Error("a file over 100 MiB was copied")
	}
	if got := read("user-units/alice/syncthing.service.d/override.conf"); !strings.Contains(got, "syncthing serve") {
		t.Errorf("drop-in = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(opt.Dir, "user-units/alice/planted.service")); err == nil {
		t.Error("a unit file another user owns was copied")
	}
	if got := read(RepoKeysFile); !strings.Contains(got, "0123456789ABCDEF0123456789ABCDEF01234567") {
		t.Errorf("repo-keys.asc = %q", got)
	}
	if got := read(VersionsFile); got != "base 3-2\nopenssh 9.9p1-2\n" {
		t.Errorf("versions.txt = %q", got)
	}
	golden(t, "desktop.bundles.json", []byte(read(record.BundlesFile)))
	if got := read(NotBackedUpMD); !strings.Contains(got, "desktop, alice's home") || !strings.Contains(got, "desktop, bob's home") {
		t.Errorf("not-backed-up.md = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(opt.Dir, SkippedFile)); err == nil {
		t.Error("skipped.txt was written though nothing was skipped")
	}
	if fi, err := os.Stat(opt.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("record folder: %v, %v; want mode 0700", fi, err)
	}

	for _, cmd := range opt.Runner.(*sysexec.Fake).Commands() {
		if _, ok := opt.Runner.(*sysexec.Fake).Responses[cmd]; !ok {
			t.Errorf("ran an unplanned program: %s", cmd)
		}
	}

	// what bundling gets
	if res.RepoOf["fuse3"] != "extra" || !res.Owned["/etc/ssh/sshd_config"] {
		t.Errorf("RepoOf or Owned missing: %v", res.RepoOf)
	}
	if !reflect.DeepEqual(res.RepoFiles, []string{"/etc/pacman.d/example.conf"}) {
		t.Errorf("RepoFiles = %v", res.RepoFiles)
	}
	if !reflect.DeepEqual(res.BluetoothMACs, []string{"00:11:22:aa:bb:cc"}) {
		t.Errorf("BluetoothMACs = %v", res.BluetoothMACs)
	}
	wantWarnings := []string{
		"not copying /opt/big/disk.img (100 MiB); restore it by hand if you need it",
		"not reading ~alice/.config/systemd/user/planted.service: it belongs to another user",
		"not reading bob's units: ~/.config/systemd/user leads outside their home folder",
	}
	if !reflect.DeepEqual(res.Warnings, wantWarnings) {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(res.Warnings, "\n"), strings.Join(wantWarnings, "\n"))
	}
}

func TestQuickCapture(t *testing.T) {
	root := buildTree(t, "system.txt")
	opt := desktopOptions(t, root)
	pm := opt.Packages.(*fakePM)
	res := capture(t, root, opt)
	if slices.Contains(pm.calls, "EditedFiles") {
		t.Error("the quick capture checked every package file")
	}
	for _, it := range res.Items {
		if it.Kind == record.KindEdited {
			t.Errorf("quick capture found an edited file: %s", it.ID)
		}
	}
	if _, err := os.Lstat(filepath.Join(opt.Dir, NotBackedUpMD)); err == nil {
		t.Error("the quick capture wrote not-backed-up.md")
	}
}

func TestCaptureWithoutRoot(t *testing.T) {
	root := buildTree(t, "system.txt")
	opt := desktopOptions(t, root)
	opt.IsRoot = false
	res := capture(t, root, opt, "etc/sudoers.d/10-wheel", "home/alice/.config/systemd/user/rclone-mount.service")
	if res.Meta.Complete {
		t.Error("a capture without root is complete")
	}
	want := []string{"/etc/sudoers.d/10-wheel", "/home/alice/.config/systemd/user/rclone-mount.service"}
	if !reflect.DeepEqual(res.Skipped, want) {
		t.Errorf("skipped = %v, want %v", res.Skipped, want)
	}
	b, err := os.ReadFile(filepath.Join(opt.Dir, SkippedFile))
	if err != nil || string(b) != strings.Join(want, "\n")+"\n" {
		t.Errorf("skipped.txt = %q, %v", b, err)
	}
	for _, it := range res.Items {
		if it.ID == "/etc/sudoers.d/10-wheel" || it.ID == "rclone-mount.service" && it.Type == record.TypeUunit {
			t.Errorf("unreadable %s was recorded", it.Key())
		}
	}
	if !strings.HasPrefix(res.Warnings[0], "not running as root") {
		t.Errorf("first warning = %q", res.Warnings[0])
	}
}

func TestCaptureRefusesAFullFolder(t *testing.T) {
	root := buildTree(t, "system.txt")
	opt := desktopOptions(t, root)
	mkdir(t, opt.Dir)
	if err := os.WriteFile(filepath.Join(opt.Dir, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestCaptureNeedsThePackageManager(t *testing.T) {
	root := buildTree(t, "system.txt")
	opt := desktopOptions(t, root)
	opt.Packages.(*fakePM).present = false
	if _, err := Run(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "pacman isn't installed") {
		t.Fatalf("err = %v", err)
	}
}

func TestSelectUsers(t *testing.T) {
	root := buildTree(t, "system.txt")
	names := func(us []User) []string {
		var out []string
		for _, u := range us {
			out = append(out, u.Name)
		}
		return out
	}
	tests := []struct {
		config   []string
		want     []string
		warnings int
	}{
		{[]string{"*"}, []string{"alice", "bob"}, 0},            // carol has no login shell, nobody is nobody
		{[]string{"bob"}, []string{"bob"}, 0},                   // limited by the config
		{[]string{"bob", "alice"}, []string{"bob", "alice"}, 0}, // in the config's order
		{[]string{"carol"}, []string{"carol"}, 0},               // named: included whatever the shell
		{[]string{"*", "dave"}, []string{"alice", "bob"}, 1},    // dave doesn't exist
		{[]string{"root"}, nil, 1},                              // root's home is /root
	}
	for _, tt := range tests {
		got, warnings, err := SelectUsers(root, tt.config)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(names(got), tt.want) || len(warnings) != tt.warnings {
			t.Errorf("SelectUsers(%v) = %v, %v; want %v with %d warnings", tt.config, names(got), warnings, tt.want, tt.warnings)
		}
	}
	if _, _, err := SelectUsers(t.TempDir(), []string{"*"}); err == nil {
		t.Error("no error without /etc/passwd")
	}
}

func TestKernelSources(t *testing.T) {
	tests := []struct {
		name, file, text string
		want             []string
	}{
		{"limine", "etc/default/limine", "KERNEL_CMDLINE[default]=\"quiet root=UUID=1 rw\"\nKERNEL_CMDLINE[default]+=\"splash quiet\"\n",
			[]string{"quiet", "splash"}},
		{"systemd-boot", "etc/sdboot-manage.conf", "LINUX_OPTIONS=\"zswap.enabled=0 rd.luks.uuid=1 cryptdevice=x\"\n",
			[]string{"zswap.enabled=0"}},
		{"cmdline file", "etc/kernel/cmdline", "root=PARTUUID=1 rootflags=subvol=@ ro nowatchdog\n",
			[]string{"nowatchdog"}},
		{"grub", "etc/default/grub", "GRUB_CMDLINE_LINUX_DEFAULT='loglevel=3 resume=UUID=2 resume_offset=9'\nGRUB_CMDLINE_LINUX=\"amd_pstate=active\"\n",
			[]string{"loglevel=3", "amd_pstate=active"}},
		{"running kernel", "proc/cmdline", "BOOT_IMAGE=/vmlinuz initrd=/x root=/dev/sda2 rw mitigations=off\n",
			[]string{"mitigations=off"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			mkdir(t, filepath.Join(root, filepath.Dir(tt.file)))
			if err := os.WriteFile(filepath.Join(root, tt.file), []byte(tt.text), 0o644); err != nil {
				t.Fatal(err)
			}
			c := &capturer{root: root, readFile: os.ReadFile, res: &Result{}}
			c.captureKernel()
			var got []string
			for _, it := range c.items {
				got = append(got, it.ID)
				if it.Source != "/"+tt.file {
					t.Errorf("%s: source = %q", it.ID, it.Source)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("params = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeverCaptured(t *testing.T) {
	for path, never := range map[string]bool{
		"/etc/machine-id":                          true,
		"/etc/passwd-":                             true,
		"/etc/shells":                              true,
		"/etc/ssl/certs/ca.pem":                    true,
		"/etc/ssh/ssh_host_rsa_key.pub":            true,
		"/etc/NetworkManager/system-connections/x": true,
		"/etc/carryon/config.toml":                 true,
		"/usr/local/bin/carryon":                   true,
		"/etc/pacman.d/hooks/carryon.hook":         true,
		"/etc/systemd/system/carryon-backup.timer": true,
		"/etc/sysbackup/sysbackup.conf":            true,
		"/etc/pacman.d/mirrorlist":                 true,
		"/etc/fstab.pacnew":                        true,
		"/usr/lib/modules/6.11/modules.dep":        true,
		"/etc/fstab":                               false,
		"/etc/ssh/sshd_config":                     false,
		"/etc/systemd/system/backup.service":       false,
		"/usr/local/bin/carryon-helper":            false,
	} {
		if got := NeverCaptured(path); got != never {
			t.Errorf("NeverCaptured(%s) = %v", path, got)
		}
	}
}

func TestItemsAreSorted(t *testing.T) {
	items := []record.Item{
		{Type: "pkg", ID: "b"}, {Type: "group", ID: "wheel", User: "bob"}, {Type: "aur", ID: "z"},
		{Type: "group", ID: "wheel", User: "alice"}, {Type: "pkg", ID: "a"},
	}
	sortItems(items)
	var got []string
	for _, it := range items {
		got = append(got, it.Key()+"/"+it.User)
	}
	want := []string{"aur:z/", "group:wheel/alice", "group:wheel/bob", "pkg:a/", "pkg:b/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v", got)
	}
}
