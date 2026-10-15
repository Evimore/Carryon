package dest

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseMountinfoLine(t *testing.T) {
	in := `120 25 8:17 / /run/media/alice/Backup\040Drive rw,nosuid,nodev,relatime shared:400 master:7 - ext4 /dev/sdb1 rw,errors=remount-ro` + "\n"
	got, err := ParseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := MountTable{{
		ID: 120, ParentID: 25, Major: 8, Minor: 17, Root: "/",
		MountPoint:   "/run/media/alice/Backup Drive",
		Options:      []string{"rw", "nosuid", "nodev", "relatime"},
		Optional:     []string{"shared:400", "master:7"},
		FSType:       "ext4",
		Source:       "/dev/sdb1",
		SuperOptions: []string{"rw", "errors=remount-ro"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseMountinfoNoOptionalFields(t *testing.T) {
	got, err := ParseMountinfo(strings.NewReader("26 1 0:26 /@ / rw,noatime - btrfs /dev/nvme0n1p2 rw,subvol=/@\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Optional != nil || got[0].Root != "/@" || got[0].FSType != "btrfs" {
		t.Errorf("got %+v", got)
	}
}

func TestParseMountinfoErrors(t *testing.T) {
	for _, line := range []string{
		"26 1 0:26 / / rw shared:1 ext4 /dev/sda1 rw", // no separator
		"x 1 0:26 / / rw - ext4 /dev/sda1 rw",
		"26 1 026 / / rw - ext4 /dev/sda1 rw",
		"26 1 0:26 / / rw -",
	} {
		if _, err := ParseMountinfo(strings.NewReader(line)); err == nil {
			t.Errorf("%q: no error", line)
		}
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`/plain`:                 "/plain",
		`/a\040b`:                "/a b",
		`/tab\011and\012newline`: "/tab\tand\nnewline",
		`/back\134slash`:         `/back\slash`,
		`/not\08escape`:          `/not\08escape`,
		`/short\04`:              `/short\04`,
		`/too\777big`:            `/too\777big`,
		`/end\`:                  `/end\`,
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadMountsMissing(t *testing.T) {
	if _, err := ReadMounts(t.TempDir()); err == nil {
		t.Error("no error for a root without /proc")
	}
}

func TestContaining(t *testing.T) {
	mounts, err := ReadMounts("testdata/usb")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/":                                     "/",
		"/etc/carryon":                          "/",
		"/home":                                 "/home",
		"/home/alice/Documents":                 "/home",
		"/homework":                             "/",
		"/run/media/alice/Backup Drive":         "/run/media/alice/Backup Drive",
		"/run/media/alice/Backup Drive/carryon": "/run/media/alice/Backup Drive",
		"/run/media/alice/Backup":               "/run",
		"/run/user/1000/../../media/alice":      "/run",
	} {
		m, ok := mounts.Containing(path)
		if !ok || m.MountPoint != want {
			t.Errorf("Containing(%q) = %q, %v; want %q", path, m.MountPoint, ok, want)
		}
	}
	if _, ok := (MountTable{}).Containing("/x"); ok {
		t.Error("an empty table contains /x")
	}
}

func TestContainingStackedMounts(t *testing.T) {
	mounts := MountTable{
		{MountPoint: "/"},
		{MountPoint: "/mnt/backup", Source: "/dev/sdb1"},
		{MountPoint: "/mnt/backup", Source: "/dev/sdc1"}, // mounted on top, hides sdb1
	}
	m, _ := mounts.Containing("/mnt/backup/carryon")
	if m.Source != "/dev/sdc1" {
		t.Errorf("got %s, want the top mount /dev/sdc1", m.Source)
	}
}

func TestUnder(t *testing.T) {
	mounts, err := ReadMounts("testdata/remote")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mounts.Under("/home/alice", "/home/bob", "/home/alice") {
		got = append(got, m.MountPoint)
	}
	want := []string{
		"/home/alice/iCloud", "/home/alice/server", "/home/alice/My Drive",
		"/home/alice/Media", "/home/alice/home-on-server",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Under(/home/alice) = %q, want %q", got, want)
	}
	if got := mounts.Under("/home"); len(got) != len(want) {
		t.Errorf("Under(/home) has %d mounts, want %d (not /home itself)", len(got), len(want))
	}
	if got := mounts.Under("/srv"); len(got) != 0 {
		t.Errorf("Under(/srv) = %v, want none", got)
	}
}
