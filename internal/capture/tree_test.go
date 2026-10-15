package capture

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

var header = regexp.MustCompile(`^-- (.+?)(?: \[([0-7]{4})\])? --$`)

// buildTree makes a fake system tree in a temp folder from a description in testdata (see
// testdata/system.txt for the format). Trees are described in text rather than checked in, since
// they hold symlinks and colons, which Go module zips don't allow.
func buildTree(t *testing.T, desc string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", desc))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	type file struct {
		path string
		mode os.FileMode
		body strings.Builder
	}
	var files []*file
	var cur *file
	for _, line := range strings.SplitAfter(string(b), "\n") {
		m := header.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil {
			if cur != nil {
				cur.body.WriteString(line)
			}
			continue
		}
		cur = nil
		name := m[1]
		if from, to, ok := strings.Cut(name, " -> "); ok {
			p := filepath.Join(root, from)
			mkdir(t, filepath.Dir(p))
			if err := os.Symlink(to, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if strings.HasSuffix(name, "/") {
			mkdir(t, filepath.Join(root, name))
			continue
		}
		cur = &file{path: name, mode: 0o644}
		if m[2] != "" {
			n, _ := strconv.ParseUint(m[2], 8, 32)
			cur.mode = os.FileMode(n)
		}
		files = append(files, cur)
	}
	for _, f := range files {
		p := filepath.Join(root, f.path)
		mkdir(t, filepath.Dir(p))
		content := strings.TrimRight(f.body.String(), "\n")
		if content != "" {
			content += "\n" // blank lines between files belong to neither
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// treeOwner gives the fake tree's files their owners: real files all belong to whoever runs the
// tests, which would make the golden record differ between machines.
func treeOwner(root string, special map[string]int) func(string, fs.FileInfo) (int, int) {
	return func(path string, _ fs.FileInfo) (int, int) {
		rel, _ := filepath.Rel(root, path)
		if id, ok := special[rel]; ok {
			return id, id
		}
		for prefix, id := range map[string]int{"home/alice": 1000, "home/bob": 1001, "var/lib/jellyfin": 970} {
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return id, id
			}
		}
		return 0, 0
	}
}

// fakePM is a package manager with canned answers. It records which methods were called.
type fakePM struct {
	present  bool
	syncRepo map[string]string
	pkgs     []pkgmgr.Package
	owned    []string
	modified []string
	edited   []pkgmgr.EditedFile
	owners   map[string]string
	repos    []pkgmgr.Repo
	keys     []pkgmgr.Key
	calls    []string
}

var _ pkgmgr.Manager = (*fakePM)(nil)

func (f *fakePM) called(name string) { f.calls = append(f.calls, name) }

func (f *fakePM) Name() string  { return "pacman" }
func (f *fakePM) Present() bool { return f.present }
func (f *fakePM) Packages(context.Context) ([]pkgmgr.Package, error) {
	f.called("Packages")
	return f.pkgs, nil
}
func (f *fakePM) Installed(context.Context) ([]string, error) { return nil, nil }
func (f *fakePM) Versions(context.Context) ([]byte, error) {
	return []byte("base 3-2\nopenssh 9.9p1-2\n"), nil
}
func (f *fakePM) SyncPackages(context.Context) ([]string, error) { return nil, nil }
func (f *fakePM) SyncRepos(context.Context) (map[string]string, error) {
	return f.syncRepo, nil
}
func (f *fakePM) OwnedFiles(context.Context) ([]string, error) { return f.owned, nil }
func (f *fakePM) FilesOf(context.Context, ...string) (map[string][]string, error) {
	return nil, nil
}
func (f *fakePM) Owners(_ context.Context, paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		if o, ok := f.owners[p]; ok {
			out[p] = o
		}
	}
	return out, nil
}
func (f *fakePM) ModifiedConfigs(context.Context) ([]string, error) { return f.modified, nil }
func (f *fakePM) EditedFiles(context.Context) ([]pkgmgr.EditedFile, error) {
	f.called("EditedFiles")
	return f.edited, nil
}
func (f *fakePM) ConfiguredRepos(context.Context) ([]string, error) { return nil, nil }
func (f *fakePM) ExtraRepos(context.Context) ([]pkgmgr.Repo, error) { return f.repos, nil }
func (f *fakePM) ExtraKeys(context.Context) ([]pkgmgr.Key, error)   { return f.keys, nil }
func (f *fakePM) ExportKeys(_ context.Context, fprs []string) ([]byte, error) {
	return []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n" + strings.Join(fprs, "\n") + "\n-----END PGP PUBLIC KEY BLOCK-----\n"), nil
}
func (f *fakePM) TrustKeys(context.Context, string, []string) ([]string, error)  { return nil, nil }
func (f *fakePM) Install(context.Context, []string, pkgmgr.InstallOptions) error { return nil }
func (f *fakePM) InstallEach(context.Context, []string, bool) []string           { return nil }

// desktopPM is the package manager of testdata/system.txt.
func desktopPM() *fakePM {
	return &fakePM{
		present: true,
		syncRepo: map[string]string{
			"base": "core", "openssh": "core", "systemd": "core", "htop": "extra", "rclone": "extra",
			"fuse3": "extra", "libnotify": "extra", "python-pillow": "extra", "example-tool": "example",
		},
		pkgs: []pkgmgr.Package{
			{Name: "base", Repo: "core"},
			{Name: "openssh", Repo: "core"},
			{Name: "htop", Repo: "extra"},
			{Name: "rclone", Repo: "extra"},
			{Name: "example-tool", Repo: "example"},
			{Name: "gone-from-repos", Repo: pkgmgr.UnknownRepo},
			{Name: "paru-bin", Foreign: true},
			{Name: "python-pillow", Repo: "extra", AsDeps: true},
			{Name: "old-helper", Foreign: true, AsDeps: true},
		},
		owned: []string{
			"/etc", "/etc/ssh", "/etc/ssh/sshd_config", "/etc/pacman.conf", "/etc/pacman.d/mirrorlist",
			"/etc/passwd", "/etc/group", "/etc/shadow", "/etc/os-release", "/etc/default/grub",
			"/usr/bin/bash", "/usr/lib/systemd/system/sshd.service", "/usr/share/applications/htop.desktop",
		},
		modified: []string{"/etc/ssh/sshd_config", "/etc/pacman.conf"},
		edited: []pkgmgr.EditedFile{
			{Path: "/usr/share/applications/htop.desktop", Package: "htop", Reasons: []string{"Size mismatch"}},
			{Path: "/var/lib/htop/state", Package: "htop", Reasons: []string{"SHA256 checksum mismatch"}},
		},
		owners: map[string]string{
			"/etc/ssh/sshd_config":                   "openssh",
			"/etc/pacman.conf":                       "pacman",
			"/usr/lib/systemd/system/sshd.service":   "openssh",
			"/usr/lib/systemd/system/getty@.service": "systemd",
			"/usr/bin/rclone":                        "rclone",
			"/usr/bin/notify-send":                   "libnotify",
			"/usr/bin/fusermount3":                   "fuse3",
			"/usr/bin/syncthing":                     "syncthing",
		},
		repos: []pkgmgr.Repo{{Name: "example", Conf: "/etc/pacman.d/example.conf", Section: "[example]\nServer = https://repo.example.com/$arch\n"}},
		keys:  []pkgmgr.Key{{Fingerprint: "0123456789ABCDEF0123456789ABCDEF01234567", UID: "Example Repo <repo@example.com>"}},
	}
}
