package dest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	rootUUID  = "0f3c9a52-6e1b-4d8a-9c27-1a2b3c4d5e6f"
	usbUUID   = "7d1e4b90-2a5c-4f63-8b11-aabbccddeeff"
	otherUUID = "c4f2a8e1-93d0-4b7e-a6f5-112233445566"
	luksUUID  = "5b8e2f17-0c4d-4e9a-b3f1-998877665544"
	usbDest   = "/run/media/alice/Backup Drive/carryon"
	usbMount  = "/run/media/alice/Backup Drive"
)

// fakeRepo answers RepositoryID with a fixed ID or error, and counts calls.
type fakeRepo struct {
	id    string
	err   error
	calls int
}

func (r *fakeRepo) RepositoryID(context.Context) (string, error) {
	r.calls++
	return r.id, r.err
}

func TestFSUUID(t *testing.T) {
	for _, tc := range []struct {
		root, mountPoint, want string
	}{
		{"testdata/usb", usbMount, usbUUID},
		{"testdata/usb", "/", rootUUID},
		{"testdata/usb", "/home", rootUUID}, // a btrfs subvolume of the system disk
		{"testdata/usb", "/boot", "A1B2-C3D4"},
		{"testdata/otherdisk", usbMount, otherUUID},
		{"testdata/luks", "/mnt/vault", luksUUID}, // /dev/mapper/luks-... and by-uuid both lead to dm-0
	} {
		mounts, err := ReadMounts(tc.root)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := mounts.Containing(tc.mountPoint)
		got, err := FSUUID(tc.root, m)
		if err != nil || got != tc.want {
			t.Errorf("%s %s: got %q, %v; want %q", tc.root, tc.mountPoint, got, err, tc.want)
		}
	}
}

func TestFSUUIDNone(t *testing.T) {
	for _, m := range []Mount{
		{MountPoint: "/run", FSType: "tmpfs", Source: "run"},
		{MountPoint: "/home/alice/iCloud", FSType: "fuse.rclone", Source: "iCloud:"},
		{MountPoint: "/mnt/x", FSType: "ext4", Source: "/dev/sdz9"}, // udev made no link
	} {
		if got, err := FSUUID("testdata/usb", m); !errors.Is(err, ErrNoUUID) {
			t.Errorf("%s: got %q, %v; want ErrNoUUID", m.Source, got, err)
		}
	}
	// No /dev/disk/by-uuid at all.
	if _, err := FSUUID(t.TempDir(), Mount{Source: "/dev/sda1"}); !errors.Is(err, ErrNoUUID) {
		t.Errorf("got %v, want ErrNoUUID", err)
	}
}

func TestUnescapeUdev(t *testing.T) {
	if got := unescapeUdev(`My\x20Disk\x2fA`); got != "My Disk/A" {
		t.Errorf("got %q", got)
	}
	if got := unescapeUdev(`odd\x2`); got != `odd\x2` {
		t.Errorf("got %q", got)
	}
}

func TestResolvePath(t *testing.T) {
	for in, want := range map[string]string{
		"/backup":                         "/mnt/vault/carryon",
		"/backup/snapshots":               "/mnt/vault/carryon/snapshots", // missing part kept as written
		"/dev/mapper/luks-" + luksUUID:    "/dev/dm-0",
		"/dev/disk/by-uuid/" + luksUUID:   "/dev/dm-0",
		"/mnt/vault/../vault/carryon":     "/mnt/vault/carryon",
		"/../../backup":                   "/mnt/vault/carryon", // never above root
		"/nothing/here/at/all":            "/nothing/here/at/all",
		"/mnt/vault/carryon/config/inner": "/mnt/vault/carryon/config/inner",
	} {
		got, err := ResolvePath("testdata/luks", in)
		if err != nil || got != want {
			t.Errorf("ResolvePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ResolvePath("testdata/luks", "relative/path"); err == nil {
		t.Error("no error for a relative path")
	}
}

func TestResolvePathLoop(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/a", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePath(root, "/a/x"); err == nil {
		t.Error("no error for a symlink loop")
	}
}

func TestRecord(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		root, path string
		want       Identity
	}{
		{"testdata/usb", usbDest, Identity{FSUUID: usbUUID, MountPoint: usbMount, RepoID: "abc123"}},
		{"testdata/luks", "/backup", Identity{FSUUID: luksUUID, MountPoint: "/mnt/vault", RepoID: "abc123"}},
		{"testdata/usb", "/run/carryon-test", Identity{MountPoint: "/run", RepoID: "abc123"}}, // tmpfs: no UUID
	} {
		got, err := Record(ctx, tc.root, tc.path, &fakeRepo{id: "abc123"})
		if err != nil || got != tc.want {
			t.Errorf("Record(%s, %q) = %+v, %v; want %+v", tc.root, tc.path, got, err, tc.want)
		}
	}
}

func TestRecordErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Record(ctx, "testdata/usb", usbDest, &fakeRepo{err: errors.New("no repository")}); err == nil {
		t.Error("no error when the repository can't be read")
	}
	if _, err := Record(ctx, "testdata/usb", usbDest, &fakeRepo{}); err == nil {
		t.Error("no error for an empty repository ID")
	}
	if _, err := Record(ctx, t.TempDir(), usbDest, &fakeRepo{id: "abc123"}); err == nil {
		t.Error("no error without a mount table")
	}
}

func TestFreeSpace(t *testing.T) {
	dir := t.TempDir()
	free, err := FreeSpace(dir, "/")
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("no free space reported in the test's temp folder")
	}
	if _, err := FreeSpace(dir, "/missing"); err == nil {
		t.Error("no error for a missing folder")
	}
}
