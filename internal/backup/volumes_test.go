package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/record"
)

func TestNestedSubvolumesAndMounts(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{
		"home/alice/projects/data", "home/alice/vm", "home/alice/.snapshots/1", "home/alice/nas/share",
		"home/alice/usb", "home/alice/skipme/sub", "srv/db", "etc",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Device numbers as the kernel would give them: each btrfs subvolume has its own.
	devs := map[string]uint64{
		"home/alice/projects/data": 40, // a subvolume made with `btrfs subvolume create`
		"home/alice/vm":            41, // a mounted subvolume of the same disk
		"home/alice/.snapshots":    42, // snapper's snapshots
		"home/alice/.snapshots/1":  43,
		"home/alice/nas":           50, // a network share
		"home/alice/nas/share":     50,
		"home/alice/usb":           60, // another disk
		"home/alice/skipme/sub":    70, // excluded
	}
	devOf := func(real string) (uint64, error) {
		rel, _ := filepath.Rel(root, real)
		for p := rel; p != "." && p != "/"; p = filepath.Dir(p) {
			if d, ok := devs[p]; ok {
				return d, nil
			}
		}
		return 30, nil
	}
	mounts := dest.MountTable{
		{ID: 1, MountPoint: "/", FSType: "btrfs", Source: "/dev/nvme0n1p2"},
		{ID: 2, MountPoint: "/home", FSType: "btrfs", Source: "/dev/nvme0n1p2"},
		{ID: 3, MountPoint: "/home/alice/vm", FSType: "btrfs", Source: "/dev/nvme0n1p2"},
		{ID: 4, MountPoint: "/home/alice/nas", FSType: "nfs4", Source: "server:/export"},
		{ID: 5, MountPoint: "/home/alice/usb", FSType: "ext4", Source: "/dev/sdb1"},
		{ID: 6, MountPoint: "/home/alice/.snapshots", FSType: "btrfs", Source: "/dev/nvme0n1p2"},
	}
	s := volumeScan{
		sys:      System{Root: root},
		mounts:   mounts,
		excluded: func(p string) bool { return p == "/home/alice/skipme" },
		devOf:    devOf,
	}
	add, left := s.scan([]string{"/etc", "/home/alice", "/srv"})
	slices.Sort(add)
	if want := []string{"/home/alice/projects/data", "/home/alice/vm"}; !slices.Equal(add, want) {
		t.Errorf("added %q, want %q", add, want)
	}
	var leftPaths []string
	for _, l := range left {
		leftPaths = append(leftPaths, l.Path)
	}
	slices.Sort(leftPaths)
	if want := []string{"/home/alice/.snapshots", "/home/alice/nas", "/home/alice/usb"}; !slices.Equal(leftPaths, want) {
		t.Errorf("left out %q, want %q", leftPaths, want)
	}
	for _, l := range left {
		if l.Path == "/home/alice/usb" && !strings.Contains(l.String(), "ext4 filesystem (/dev/sdb1)") {
			t.Errorf("message = %q", l)
		}
	}

	// One path at a time, as carryon why asks.
	for _, c := range []struct {
		target, path, subvolume, leftOut string
	}{
		{"/home/alice", "/home/alice/notes.txt", "", ""},
		{"/home/alice", "/home/alice", "", ""},
		{"/home/alice", "/home/alice/projects/data/x.csv", "/home/alice/projects/data", ""},
		{"/home/alice", "/home/alice/vm/disk.img", "/home/alice/vm", ""},
		{"/home/alice", "/home/alice/.snapshots/1/x", "", "/home/alice/.snapshots"},
		{"/home/alice", "/home/alice/nas/share/film.mkv", "", "/home/alice/nas"},
		{"/home/alice", "/home/alice/usb", "", "/home/alice/usb"},
		{"/home/alice", "/home/alice/not/there/yet", "", ""},
		{"/srv", "/home/alice/usb", "", ""}, // not inside the target
	} {
		got := s.crossing(c.target, c.path)
		left := ""
		if got.LeftOut != nil {
			left = got.LeftOut.Path
		}
		if got.Subvolume != c.subvolume || left != c.leftOut {
			t.Errorf("crossing(%s, %s) = %q, %q; want %q, %q", c.target, c.path, got.Subvolume, left, c.subvolume, c.leftOut)
		}
	}
}

func TestWithoutSubvolumes(t *testing.T) {
	mounts := dest.MountTable{
		{ID: 1, MountPoint: "/", FSType: "btrfs", Source: "/dev/sda2"},
		{ID: 2, MountPoint: "/home", FSType: "btrfs", Source: "/dev/sda2"},
		{ID: 3, MountPoint: "/home/alice/other", FSType: "btrfs", Source: "/dev/sdb1"},
		{ID: 4, MountPoint: "/home/alice/cloud", FSType: "fuse.rclone", Source: "drive:"},
	}
	var got []string
	for _, m := range WithoutSubvolumes(mounts) {
		got = append(got, m.MountPoint)
	}
	if want := []string{"/", "/home/alice/other", "/home/alice/cloud"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTargets(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"etc/ssh", "root", "home/alice/.config", "var/lib/bluetooth", "opt/x"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "etc/ssh/ssh_host_ed25519_key"), "x")
	cfg := config.Default()
	cfg.Backup.ExtraPaths = []string{"/opt/x", "relative", "/missing"}
	// A real capture also records each of backup.extra_paths as a path item marked extra.
	items := itemsOf(`{"type":"path","id":"/var/lib/bluetooth"}`, `{"type":"path","id":"/etc/ssh/ssh_host_*"}`,
		`{"type":"path","id":"/home/alice/.config","user":"alice","home":".config"}`, `{"extra":true,"id":"/opt/x","type":"path"}`)
	got := targets(System{Root: root}, cfg, items, usersOf("alice"))
	want := []string{"/etc", "/home/alice", "/opt/x", "/root", "/var/lib/bluetooth"}
	if !slices.Equal(got, want) {
		t.Errorf("targets = %q, want %q", got, want)
	}
	cfg.Backup.ExtraPaths = append(cfg.Backup.ExtraPaths, "/root")
	var kinds []string
	for _, tg := range Targets(System{Root: root}, cfg, items, usersOf("alice")) {
		kinds = append(kinds, tg.Path+" "+tg.Kind+" "+tg.User)
	}
	wantKinds := []string{"/etc system ", "/home/alice home alice", "/opt/x extra_paths ", "/root system ", "/var/lib/bluetooth record "}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("Targets = %q, want %q", kinds, wantKinds)
	}
}

func itemsOf(lines ...string) []record.Item {
	items, err := record.ReadItems(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		panic(err)
	}
	return items
}

func usersOf(names ...string) []capture.User {
	var out []capture.User
	for i, n := range names {
		out = append(out, capture.User{Name: n, UID: 1000 + i, Home: "/home/" + n})
	}
	return out
}
