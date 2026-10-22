package dest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/engine"
)

// fakeRunner returns canned output and records the commands it was asked to run.
type fakeRunner struct {
	err   error
	calls [][]string
}

func (r *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.err != nil {
		return nil, r.err
	}
	return []byte("backups/\n"), nil
}

var usbIdentity = Identity{FSUUID: usbUUID, MountPoint: usbMount, RepoID: "abc123"}

func TestCheckLocal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		root   string
		url    string
		want   Identity
		repo   *fakeRepo
		status Status
		mount  string // the mount the result names
	}{
		{"plugged in", "testdata/usb", usbDest, usbIdentity, &fakeRepo{id: "abc123"}, OK, usbMount},
		{"local: prefix", "testdata/usb", "local:" + usbDest, usbIdentity, &fakeRepo{id: "abc123"}, OK, usbMount},
		{"unplugged", "testdata/unplugged", usbDest, usbIdentity, &fakeRepo{id: "abc123"}, NotMounted, "/run"},
		{"different disk", "testdata/otherdisk", usbDest, usbIdentity, &fakeRepo{id: "abc123"}, DifferentDisk, usbMount},
		{"different repository", "testdata/usb", usbDest, usbIdentity, &fakeRepo{id: "fff999"}, RepoMismatch, usbMount},
		{"folder gone", "testdata/usb", usbMount + "/renamed", usbIdentity, &fakeRepo{id: "abc123"}, Missing, usbMount},
		{"repository unreadable", "testdata/usb", usbDest, usbIdentity, &fakeRepo{err: errors.New("Fatal: unable to open config file")}, Unreachable, usbMount},
		{"wrong password", "testdata/usb", usbDest, usbIdentity, &fakeRepo{err: &engine.Error{Problem: engine.Problem{Cause: "wrong password or no key found"}, Code: engine.ExitBadPassword, Refused: true}}, CantOpen, usbMount},
		{"no mount table", "testdata/nothing-here", usbDest, usbIdentity, &fakeRepo{id: "abc123"}, Unreachable, ""},
		{"no recorded mount", "testdata/usb", usbDest, Identity{RepoID: "abc123"}, &fakeRepo{id: "abc123"}, Unreachable, ""},
		{"no recorded repository", "testdata/usb", usbDest, Identity{FSUUID: usbUUID, MountPoint: usbMount}, &fakeRepo{id: "abc123"}, Unreachable, ""},
		{"relative path", "testdata/usb", "carryon", usbIdentity, &fakeRepo{id: "abc123"}, Unreachable, ""},
		{"symlinked destination", "testdata/luks", "/backup", Identity{FSUUID: luksUUID, MountPoint: "/mnt/vault", RepoID: "abc123"}, &fakeRepo{id: "abc123"}, OK, "/mnt/vault"},
		{"no UUID recorded", "testdata/luks", "/backup", Identity{MountPoint: "/mnt/vault", RepoID: "abc123"}, &fakeRepo{id: "abc123"}, OK, "/mnt/vault"},
		{"UUID case differs", "testdata/usb", usbDest, Identity{FSUUID: strings.ToUpper(usbUUID), MountPoint: usbMount, RepoID: "abc123"}, &fakeRepo{id: "abc123"}, OK, usbMount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Checker{Root: tc.root, Repo: tc.repo}
			r := c.Check(context.Background(), tc.url, tc.want)
			if r.Status != tc.status {
				t.Fatalf("status %v, want %v (%s)", r.Status, tc.status, r)
			}
			if r.OK() != (tc.status == OK) {
				t.Errorf("OK() = %v with status %v", r.OK(), r.Status)
			}
			if !r.OK() && (r.Message == "" || strings.Contains(r.Message, "%!")) {
				t.Errorf("bad message %q", r.Message)
			}
			if r.Mount.MountPoint != tc.mount {
				t.Errorf("mount %q, want %q", r.Mount.MountPoint, tc.mount)
			}
		})
	}
}

// The safety rule: nothing is ever written to a destination whose mount doesn't match. When the drive
// is unplugged, Check must refuse whatever else looks right, and must not even ask the repository
// (restic would be reading a stale folder on the system disk).
func TestCheckUnpluggedNeverOK(t *testing.T) {
	ids := []Identity{
		usbIdentity,
		{MountPoint: usbMount, RepoID: "abc123"}, // no UUID recorded
		{FSUUID: rootUUID, MountPoint: usbMount, RepoID: "abc123"},    // UUID of the system disk
		{FSUUID: usbUUID, MountPoint: usbMount + "/", RepoID: "abc1"}, // trailing slash
	}
	for _, id := range ids {
		for _, url := range []string{usbDest, usbMount, "local:" + usbDest, usbDest + "/../carryon"} {
			repo := &fakeRepo{id: id.RepoID} // the stray folder even holds a matching repository
			r := Checker{Root: "testdata/unplugged", Repo: repo}.Check(context.Background(), url, id)
			if r.OK() {
				t.Errorf("%s with %+v: OK on an unplugged drive", url, id)
			}
			if r.Status != NotMounted {
				t.Errorf("%s with %+v: status %v, want %v", url, id, r.Status, NotMounted)
			}
			if repo.calls != 0 {
				t.Errorf("%s: read the repository on the system disk", url)
			}
			if !strings.Contains(r.Message, usbMount) || !strings.Contains(r.Message, "wrote nothing") || !strings.Contains(r.Fix, "Plug in") {
				t.Errorf("unclear message: %s", r)
			}
		}
	}
}

// A drive meant for /mnt/backup, with the mount point on the system disk.
func TestCheckUnpluggedOnSystemDisk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, MountinfoPath, "26 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw\n")
	writeFile(t, root, "/mnt/backup/carryon/config", "")
	r := Checker{Root: root, Repo: &fakeRepo{id: "abc123"}}.Check(context.Background(), "/mnt/backup/carryon",
		Identity{FSUUID: usbUUID, MountPoint: "/mnt/backup", RepoID: "abc123"})
	if r.Status != NotMounted || !strings.Contains(r.Message, "system disk") {
		t.Errorf("got %v: %s", r.Status, r)
	}
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDifferentDiskNeverOK(t *testing.T) {
	repo := &fakeRepo{id: "abc123"} // even the same repository ID (a copied disk)
	r := Checker{Root: "testdata/otherdisk", Repo: repo}.Check(context.Background(), usbDest, usbIdentity)
	if r.Status != DifferentDisk {
		t.Fatalf("status %v, want %v", r.Status, DifferentDisk)
	}
	if !strings.Contains(r.Message, otherUUID) || !strings.Contains(r.Message, usbUUID) {
		t.Errorf("message doesn't name both disks: %s", r.Message)
	}
	if repo.calls != 0 {
		t.Error("read the repository on the wrong disk")
	}
}

func TestCheckRclone(t *testing.T) {
	ctx := context.Background()
	want := Identity{RepoID: "abc123"}

	run := &fakeRunner{}
	r := Checker{Repo: &fakeRepo{id: "abc123"}, Runner: run}.Check(ctx, "rclone:iCloud:Backups/carryon", want)
	if !r.OK() {
		t.Fatalf("got %s", r)
	}
	if wantCall := [][]string{{"rclone", "lsf", "--max-depth", "1", "iCloud:"}}; !reflect.DeepEqual(run.calls, wantCall) {
		t.Errorf("ran %q, want %q", run.calls, wantCall)
	}

	// The login expired: waiting won't help. rclone's own last line and the reconnect command.
	// Offline, the remote doesn't answer: that waits.
	for _, tc := range []struct {
		stderr string
		status Status
		says   string
	}{
		{"2026/10/15 NOTICE: something\n2026/10/15 ERROR : couldn't list: token expired\n", CantOpen, "token expired"},
		{"2026/10/15 ERROR : couldn't list: Get \"https://example.com/\": dial tcp: lookup example.com: no such host\n", Unreachable, "no such host"},
	} {
		run = &fakeRunner{err: &exec.ExitError{Stderr: []byte(tc.stderr)}}
		repo := &fakeRepo{id: "abc123"}
		r = Checker{Repo: repo, Runner: run}.Check(ctx, "rclone:iCloud:Backups/carryon", want)
		if r.Status != tc.status {
			t.Fatalf("%s: status %v, want %v", tc.says, r.Status, tc.status)
		}
		if !strings.Contains(r.Message, tc.says) || strings.Contains(r.Message, "NOTICE") {
			t.Errorf("message %q doesn't end with rclone's error", r.Message)
		}
		if !strings.Contains(r.Fix, "rclone reconnect iCloud:") {
			t.Errorf("fix %q lacks the reconnect command", r.Fix)
		}
		if repo.calls != 0 {
			t.Error("asked restic after rclone failed")
		}
	}

	// rclone answers but the repository is another one.
	r = Checker{Repo: &fakeRepo{id: "fff999"}, Runner: &fakeRunner{}}.Check(ctx, "rclone:iCloud:Backups/carryon", want)
	if r.Status != RepoMismatch {
		t.Errorf("status %v, want %v", r.Status, RepoMismatch)
	}

	// No runner.
	r = Checker{Repo: &fakeRepo{id: "abc123"}}.Check(ctx, "rclone:iCloud:x", want)
	if r.Status != Unreachable {
		t.Errorf("status %v without a runner, want %v", r.Status, Unreachable)
	}
}

func TestCheckOtherBackends(t *testing.T) {
	ctx := context.Background()
	want := Identity{RepoID: "abc123"}
	for _, url := range []string{"sftp:alice@example.com:/srv/carryon", "s3:s3.example.com/my-bucket", "rest:https://example.com/carryon", "b2:my-bucket:carryon"} {
		run := &fakeRunner{}
		if r := (Checker{Repo: &fakeRepo{id: "abc123"}, Runner: run}).Check(ctx, url, want); !r.OK() {
			t.Errorf("%s: got %s", url, r)
		}
		if len(run.calls) != 0 {
			t.Errorf("%s: ran %q", url, run.calls)
		}
		r := Checker{Repo: &fakeRepo{err: errors.New("dial tcp: no route to host")}}.Check(ctx, url, want)
		if r.Status != Unreachable || !strings.Contains(r.Message, "no route to host") {
			t.Errorf("%s offline: got %v %q", url, r.Status, r.Message)
		}
		if r := (Checker{Repo: &fakeRepo{id: "fff999"}}).Check(ctx, url, want); r.Status != RepoMismatch {
			t.Errorf("%s: status %v, want %v", url, r.Status, RepoMismatch)
		}
	}
}

func TestIsLocal(t *testing.T) {
	for url, want := range map[string]bool{
		"/mnt/backup": true, "local:/mnt/backup": true,
		"sftp:alice@example.com:/x": false, "rclone:iCloud:x": false, "s3:example.com/my-bucket": false,
		"rest:https://example.com/": false, "b2:my-bucket:x": false, "azure:c:/": false,
		"gs:my-bucket:/": false, "swift:c:/": false,
	} {
		if got := IsLocal(url); got != want {
			t.Errorf("IsLocal(%q) = %v", url, got)
		}
	}
}

func TestRcloneRemote(t *testing.T) {
	for url, want := range map[string]string{
		"rclone:iCloud:Backups/carryon":       "iCloud:",
		"rclone:gdrive:":                      "gdrive:",
		"rclone::s3,provider=AWS:my-bucket/x": ":s3,provider=AWS:",
		"rclone:nopath":                       "",
		"rclone::":                            "",
		"rclone::onlybackend":                 "",
		"sftp:alice@example.com:/x":           "",
	} {
		got, err := RcloneRemote(url)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("RcloneRemote(%q) = %q, %v; want %q", url, got, err, want)
		}
	}
}

func TestStatusStrings(t *testing.T) {
	seen := map[string]bool{}
	for s := OK; s <= Unreachable; s++ {
		str := s.String()
		if strings.HasPrefix(str, "Status(") || seen[str] {
			t.Errorf("status %d has string %q", s, str)
		}
		seen[str] = true
	}
	if (Result{Status: OK}).String() == "" {
		t.Error("OK result has no text")
	}
}
