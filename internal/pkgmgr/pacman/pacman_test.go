package pacman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/pkgmgr"
)

// exitError is what a runner returns for a program that ran and exited non-zero.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

type reply struct {
	out []byte
	err error
}

// fakeRunner answers commands with canned output and records every call.
type fakeRunner struct {
	t       *testing.T
	replies map[string]reply // by "name arg arg..."
	prefix  map[string]reply // by the start of the command line, for commands with variable args
	calls   [][]string
	runs    [][]string
	failRun map[string]bool // Run fails for these command lines
	have    map[string]bool
}

func newFake(t *testing.T) *fakeRunner {
	return &fakeRunner{t: t, replies: map[string]reply{}, prefix: map[string]reply{}, failRun: map[string]bool{},
		have: map[string]bool{}}
}

// file sets the reply for a command to a testdata file.
func (f *fakeRunner) file(cmd, name string, err error) {
	f.t.Helper()
	data, rerr := os.ReadFile(filepath.Join("testdata", name))
	if rerr != nil {
		f.t.Fatal(rerr)
	}
	f.replies[cmd] = reply{data, err}
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	line := strings.Join(argv, " ")
	if r, ok := f.replies[line]; ok {
		return r.out, r.err
	}
	for p, r := range f.prefix {
		if strings.HasPrefix(line, p) {
			return r.out, r.err
		}
	}
	f.t.Errorf("unexpected command: %s", line)
	return nil, errors.New("unexpected command")
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	argv := append([]string{name}, args...)
	f.runs = append(f.runs, argv)
	if f.failRun[strings.Join(argv, " ")] {
		return exitError(1)
	}
	return nil
}

func (f *fakeRunner) Exists(name string) bool { return f.have[name] }

var ctx = context.Background()

func TestPackages(t *testing.T) {
	f := newFake(t)
	f.file("pacman -Qq", "qq.txt", nil)
	f.file("pacman -Sl", "sl.txt", nil)
	f.file("pacman -Qqen", "qqen.txt", nil)
	f.file("pacman -Qqem", "qqem.txt", nil)
	f.file("pacman -Qqdt", "qqdt.txt", nil)
	f.file("pacman -Qqdttn", "qqdttn.txt", nil)
	f.file("pacman -Qqdttm", "qqdttm.txt", nil)

	got, err := New(f, Options{}).Packages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pkg := func(name, repo string) pkgmgr.Package { return pkgmgr.Package{Name: name, Repo: repo} }
	dep := func(name, repo string) pkgmgr.Package { return pkgmgr.Package{Name: name, Repo: repo, AsDeps: true} }
	want := []pkgmgr.Package{
		pkg("base", "core"), pkg("bash", "core"), pkg("example-tool", "example"), pkg("firefox", "extra"),
		pkg("htop", "extra"), // in extra and example: the first repository listed wins
		pkg("linux", "core"), pkg("openssh", "core"), pkg("steam", "multilib"), pkg("vim", "extra"),
		pkg("zsh", "extra"), pkg("gone-from-repos", "?"),
		{Name: "paru", Foreign: true}, {Name: "my-own-tool", Foreign: true},
		dep("mpv", "extra"), dep("pipewire-jack", "extra"), // python-pillow is an orphan
		{Name: "some-aur-plugin", Foreign: true, AsDeps: true}, // old-build-dep is an orphan
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Packages:\n got %+v\nwant %+v", got, want)
	}
}

func TestPackagesEmptyQueries(t *testing.T) {
	// pacman exits 1 when a query matches nothing: no orphans, no foreign packages.
	f := newFake(t)
	f.file("pacman -Qq", "qq.txt", nil)
	f.file("pacman -Sl", "sl.txt", nil)
	f.replies["pacman -Qqen"] = reply{[]byte("bash\n"), nil}
	for _, q := range []string{"-Qqem", "-Qqdt", "-Qqdttn", "-Qqdttm"} {
		f.replies["pacman "+q] = reply{nil, exitError(1)}
	}
	got, err := New(f, Options{}).Packages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []pkgmgr.Package{{Name: "bash", Repo: "core"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPackagesBrokenDatabase(t *testing.T) {
	f := newFake(t)
	f.replies["pacman -Qq"] = reply{nil, exitError(1)}
	if _, err := New(f, Options{}).Packages(ctx); err == nil {
		t.Error("Packages succeeded although pacman couldn't list the installed packages")
	}
}

func TestQueryErrors(t *testing.T) {
	// a program that didn't run at all is an error even for queries that may exit 1
	f := newFake(t)
	f.replies["pacman -Slq"] = reply{nil, errors.New("executable file not found")}
	if _, err := New(f, Options{}).SyncPackages(ctx); err == nil {
		t.Error("SyncPackages: want an error")
	}
	f.replies["pacman -Slq"] = reply{nil, exitError(2)}
	if _, err := New(f, Options{}).SyncPackages(ctx); err == nil {
		t.Error("SyncPackages: want an error for exit code 2")
	}
}

func TestLists(t *testing.T) {
	f := newFake(t)
	f.file("pacman -Qq", "qq.txt", nil)
	f.file("pacman -Slq", "qqen.txt", nil)
	f.file("pacman -Sl", "sl.txt", nil)
	f.file("pacman -Qlq", "qlq.txt", nil)
	f.file("pacman -Ql htop openssh", "ql.txt", nil)
	f.replies["pacman -Q"] = reply{[]byte("bash 5.2.037-1\nzsh 5.9-5\n"), nil}
	p := New(f, Options{})

	inst, err := p.Installed(ctx)
	if err != nil || !reflect.DeepEqual(inst, []string{"base", "bash", "coreutils", "glibc", "linux", "pacman"}) {
		t.Errorf("Installed = %v, %v", inst, err)
	}
	sync, err := p.SyncPackages(ctx)
	if err != nil || len(sync) != 11 || sync[10] != "gone-from-repos" {
		t.Errorf("SyncPackages = %v, %v", sync, err)
	}
	repos, err := p.SyncRepos(ctx)
	if err != nil || repos["htop"] != "extra" || repos["example-tool"] != "example" || repos["steam"] != "multilib" {
		t.Errorf("SyncRepos = %v, %v; want htop from extra (listed first), example-tool from example", repos, err)
	}
	owned, err := p.OwnedFiles(ctx)
	wantOwned := []string{"/etc", "/etc/ssh", "/etc/ssh/sshd_config", "/usr", "/usr/bin", "/usr/bin/htop",
		"/usr/lib/systemd/system/sshd.service"}
	if err != nil || !reflect.DeepEqual(owned, wantOwned) {
		t.Errorf("OwnedFiles = %v, %v", owned, err)
	}
	files, err := p.FilesOf(ctx, "htop", "openssh")
	wantFiles := map[string][]string{
		"htop":    {"/usr/", "/usr/bin/", "/usr/bin/htop", "/usr/share/applications/htop.desktop"},
		"openssh": {"/etc/", "/etc/ssh/", "/etc/ssh/sshd_config", "/usr/lib/systemd/system/sshd.service"},
	}
	if err != nil || !reflect.DeepEqual(files, wantFiles) {
		t.Errorf("FilesOf = %v, %v", files, err)
	}
	if none, err := p.FilesOf(ctx); err != nil || len(none) != 0 {
		t.Errorf("FilesOf() = %v, %v; want nothing, without running pacman", none, err)
	}
	if v, err := p.Versions(ctx); err != nil || string(v) != "bash 5.2.037-1\nzsh 5.9-5\n" {
		t.Errorf("Versions = %q, %v", v, err)
	}
}

func TestOwners(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"usr/lib", "usr/bin", "etc/ssh", "usr/share/doc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"usr/lib/libc.so.6", "usr/bin/ls", "etc/ssh/sshd_config", "usr/share/doc/my file.txt"} {
		if err := os.WriteFile(filepath.Join(root, file), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("usr/lib", filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}

	f := newFake(t)
	// pacman gets the resolved paths, each once
	f.file("env LC_ALL=C pacman -Qo /usr/bin/ls /usr/lib/libc.so.6 /etc/ssh/sshd_config /usr/share/doc/my file.txt /usr/lib /etc/unowned",
		"qo.txt", exitError(1))
	got, err := New(f, Options{Root: root}).Owners(ctx, []string{
		"/usr/bin/ls", "/lib/libc.so.6", "/usr/lib/libc.so.6", "/etc/ssh/sshd_config", "",
		"/usr/share/doc/my file.txt", "/lib", "/etc/unowned",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/usr/bin/ls":                "coreutils",
		"/lib/libc.so.6":             "glibc", // keys are the paths as given
		"/usr/lib/libc.so.6":         "glibc",
		"/etc/ssh/sshd_config":       "openssh",
		"/usr/share/doc/my file.txt": "example-tool",
		"/lib":                       "glibc", // a folder: several owners, the last one listed wins
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Owners:\n got %v\nwant %v", got, want)
	}
}

func TestOwnersBatches(t *testing.T) {
	var paths []string
	for i := range 450 {
		paths = append(paths, fmt.Sprintf("/usr/share/example/f%03d", i))
	}
	f := newFake(t)
	f.prefix["env LC_ALL=C pacman -Qo "] = reply{[]byte("/usr/share/example/f000 is owned by example-tool 2.1-1\n"), nil}
	got, err := New(f, Options{Root: t.TempDir()}).Owners(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, c := range f.calls {
		sizes = append(sizes, len(c)-4) // minus env LC_ALL=C pacman -Qo
	}
	if !reflect.DeepEqual(sizes, []int{200, 200, 50}) {
		t.Errorf("batch sizes = %v, want [200 200 50]", sizes)
	}
	if got["/usr/share/example/f000"] != "example-tool" || len(got) != 1 {
		t.Errorf("Owners = %v", got)
	}
}

func TestModifiedConfigs(t *testing.T) {
	for _, file := range []string{"qii.txt", "qii-tabs.txt"} {
		t.Run(file, func(t *testing.T) {
			f := newFake(t)
			f.file("env LC_ALL=C pacman -Qii", file, nil)
			got, err := New(f, Options{}).ModifiedConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"/etc/ssh/sshd_config", "/etc/pacman.conf"}
			if file == "qii.txt" {
				want = append(want, "/etc/zsh/zprofile")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestParseBackupFiles(t *testing.T) {
	data, err := os.ReadFile("testdata/qii.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := parseBackupFiles(data)
	want := []backupFile{
		{"/etc/pam.d/sshd", "unmodified"}, {"/etc/ssh/ssh_config", "unmodified"}, {"/etc/ssh/sshd_config", "modified"},
		{"/etc/pacman.conf", "modified"}, {"/etc/makepkg.conf", "unmodified"}, {"/etc/makepkg.conf.d/rust.conf", "missing"},
		{"/etc/zsh/zprofile", "modified"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// "Optional Deps : sh: for ... [installed]" isn't a backup file
	for _, b := range got {
		if !strings.HasPrefix(b.path, "/etc/") {
			t.Errorf("not a backup file: %+v", b)
		}
	}
}

func TestEditedFiles(t *testing.T) {
	f := newFake(t)
	f.file("env LC_ALL=C sh -c exec pacman -Qkk 2>&1", "qkk.txt", exitError(1))
	got, err := New(f, Options{}).EditedFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgmgr.EditedFile{
		{Path: "/usr/lib/locale/locale-archive", Package: "glibc",
			Reasons: []string{"Size mismatch", "MD5 checksum mismatch", "SHA256 checksum mismatch"}},
		{Path: "/usr/share/vim/vim91/defaults.vim", Package: "vim",
			Reasons: []string{"MD5 checksum mismatch", "SHA256 checksum mismatch"}},
		{Path: "/usr/share/vim/vim91/doc/my notes (draft).txt", Package: "vim", Reasons: []string{"Size mismatch"}},
		{Path: "/usr/share/zsh/functions/Completion/Unix/_ls", Package: "zsh",
			Reasons: []string{"Size mismatch", "SHA256 checksum mismatch"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EditedFiles:\n got %+v\nwant %+v", got, want)
	}
}

func TestEditedFilesNotRun(t *testing.T) {
	f := newFake(t)
	f.replies["env LC_ALL=C sh -c exec pacman -Qkk 2>&1"] = reply{nil, errors.New("killed")}
	if _, err := New(f, Options{}).EditedFiles(ctx); err == nil {
		t.Error("want an error when the check didn't run")
	}
}

func TestParseCheckLines(t *testing.T) {
	tests := []struct {
		line string
		want []pkgmgr.EditedFile
	}{
		{"warning: a: /usr/x (Size mismatch)", []pkgmgr.EditedFile{{Path: "/usr/x", Package: "a", Reasons: []string{"Size mismatch"}}}},
		{"warning: a: /usr/x (Modification time mismatch)", nil},
		{"warning: a: /usr/x (Permissions mismatch)", nil},
		{"warning: a: /usr/x (No such file or directory)", nil},
		{"backup file: a: /etc/x (Size mismatch)", nil},
		{"a: 12 total files, 1 altered file", nil},
		{"warning: a: no mtree file", nil},
		{"warning: path too long: /usr/x", nil},
		{"error: could not open file", nil},
	}
	for _, tt := range tests {
		if got := parseCheck([]byte(tt.line + "\n")); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseCheck(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}

func TestParseGPGKeys(t *testing.T) {
	data, err := os.ReadFile("testdata/gpg-list-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := parseGPGKeys(data)
	want := []pkgmgr.Key{
		{Fingerprint: "0000F00DF00DF00DF00DF00DF00DF00DF00DF00D", UID: "Pacman Keyring Master Key <pacman@localhost>"},
		{Fingerprint: "AB12AB12AB12AB12AB12AB12AB12AB12AB12AB12", UID: "Example Arch Master Key <master@archlinux.example>"},
		{Fingerprint: "5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F", UID: "Example Repo <repo@example.com>"},
		{Fingerprint: "CAFE0000CAFE0000CAFE0000CAFE0000C0DEC0DE", UID: "Alice Example <alice@example.com>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// like the prototype: a key without a user ID isn't listed, and short lines don't break it
	if got := parseGPGKeys([]byte("pub:u:4096\nfpr:::::::::ABCD:\nsub:u\npub\nuid\n")); got != nil {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestExtraKeys(t *testing.T) {
	root, err := filepath.Abs("testdata/root")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	f := newFake(t)
	home := filepath.Join(root, "etc/pacman.d/gnupg")
	f.file("gpg --homedir "+home+" --no-permission-warning --batch --no-auto-check-trustdb --with-colons --list-keys",
		"gpg-list-keys.txt", nil)
	data, err := os.ReadFile("testdata/gpg-show-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.prefix["gpg --homedir "+filepath.Join(tmp, "carryon-gnupg-")] = reply{data, nil}

	p := New(f, Options{Root: root, TempDir: tmp})
	got, err := p.ExtraKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgmgr.Key{
		{Fingerprint: "5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F5E1F", UID: "Example Repo <repo@example.com>"},
		{Fingerprint: "CAFE0000CAFE0000CAFE0000CAFE0000C0DEC0DE", UID: "Alice Example <alice@example.com>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtraKeys:\n got %+v\nwant %+v", got, want)
	}
	var shown []string
	for _, c := range f.calls[1:] {
		shown = append(shown, c[len(c)-1])
	}
	wantShown := []string{filepath.Join(root, "usr/share/pacman/keyrings/archlinux.gpg"),
		filepath.Join(root, "usr/share/pacman/keyrings/example-distro.gpg")}
	if !reflect.DeepEqual(shown, wantShown) {
		t.Errorf("keyrings read = %v, want %v", shown, wantShown)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the scratch keyring folder wasn't removed: %v", left)
	}

	f.replies["gpg --homedir "+home+" --no-permission-warning --batch --no-auto-check-trustdb --armor --export 5E1F CAFE"] =
		reply{[]byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"), nil}
	if out, err := p.ExportKeys(ctx, []string{"5E1F", "CAFE"}); err != nil || !strings.HasPrefix(string(out), "-----BEGIN") {
		t.Errorf("ExportKeys = %q, %v", out, err)
	}
	if out, err := p.ExportKeys(ctx, nil); out != nil || err != nil {
		t.Errorf("ExportKeys(nil) = %q, %v", out, err)
	}
}

func TestTrustKeys(t *testing.T) {
	f := newFake(t)
	f.failRun["pacman-key --lsign-key CAFE"] = true
	failed, err := New(f, Options{}).TrustKeys(ctx, "/tmp/keys.asc", []string{"5E1F", "CAFE"})
	if err != nil || !reflect.DeepEqual(failed, []string{"CAFE"}) {
		t.Errorf("TrustKeys = %v, %v", failed, err)
	}
	want := [][]string{{"pacman-key", "--add", "/tmp/keys.asc"}, {"pacman-key", "--lsign-key", "5E1F"},
		{"pacman-key", "--lsign-key", "CAFE"}}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("ran %v, want %v", f.runs, want)
	}

	f = newFake(t)
	f.failRun["pacman-key --add /tmp/keys.asc"] = true
	failed, err = New(f, Options{}).TrustKeys(ctx, "/tmp/keys.asc", []string{"5E1F"})
	if err == nil || !reflect.DeepEqual(failed, []string{"5E1F"}) || len(f.runs) != 1 {
		t.Errorf("TrustKeys with a bad key file = %v, %v after %v", failed, err, f.runs)
	}
}

func TestInstall(t *testing.T) {
	tests := []struct {
		name string
		pkgs []string
		opt  pkgmgr.InstallOptions
		want []string // nil: nothing runs
	}{
		{"upgrade and install", []string{"htop", "vim"}, pkgmgr.InstallOptions{Upgrade: true},
			[]string{"pacman", "-Syu", "--needed", "htop", "vim"}},
		{"without asking", []string{"htop"}, pkgmgr.InstallOptions{Upgrade: true, NoConfirm: true},
			[]string{"pacman", "-Syu", "--needed", "--noconfirm", "htop"}},
		{"upgrade only", nil, pkgmgr.InstallOptions{Upgrade: true}, []string{"pacman", "-Syu", "--needed"}},
		{"as dependencies", []string{"mpv"}, pkgmgr.InstallOptions{AsDeps: true, NoConfirm: true},
			[]string{"pacman", "-S", "--needed", "--asdeps", "--noconfirm", "mpv"}},
		{"plain", []string{"paru", "base-devel"}, pkgmgr.InstallOptions{},
			[]string{"pacman", "-S", "--needed", "paru", "base-devel"}},
		{"nothing to do", nil, pkgmgr.InstallOptions{NoConfirm: true}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if err := New(f, Options{}).Install(ctx, tt.pkgs, tt.opt); err != nil {
				t.Fatal(err)
			}
			var want [][]string
			if tt.want != nil {
				want = [][]string{tt.want}
			}
			if !reflect.DeepEqual(f.runs, want) {
				t.Errorf("ran %v, want %v", f.runs, want)
			}
		})
	}

	f := newFake(t)
	err := New(f, Options{}).Install(ctx, []string{"mpv"}, pkgmgr.InstallOptions{Upgrade: true, AsDeps: true})
	if !errors.Is(err, pkgmgr.ErrUpgradeAsDeps) || len(f.runs) != 0 {
		t.Errorf("Upgrade with AsDeps: err = %v, ran %v", err, f.runs)
	}
	f.failRun["pacman -S --needed htop"] = true
	if err := New(f, Options{}).Install(ctx, []string{"htop"}, pkgmgr.InstallOptions{}); err == nil {
		t.Error("a failed install should return an error")
	}
}

func TestInstallEach(t *testing.T) {
	f := newFake(t)
	f.failRun["pacman -S --needed --noconfirm --asdeps broken"] = true
	failed := New(f, Options{}).InstallEach(ctx, []string{"mpv", "broken", "htop"}, true)
	if !reflect.DeepEqual(failed, []string{"broken"}) {
		t.Errorf("failed = %v", failed)
	}
	want := [][]string{
		{"pacman", "-S", "--needed", "--noconfirm", "--asdeps", "mpv"},
		{"pacman", "-S", "--needed", "--noconfirm", "--asdeps", "broken"},
		{"pacman", "-S", "--needed", "--noconfirm", "--asdeps", "htop"},
	}
	if !reflect.DeepEqual(f.runs, want) {
		t.Errorf("ran %v", f.runs)
	}
	f = newFake(t)
	New(f, Options{}).InstallEach(ctx, []string{"htop"}, false)
	if want := [][]string{{"pacman", "-S", "--needed", "--noconfirm", "htop"}}; !reflect.DeepEqual(f.runs, want) {
		t.Errorf("ran %v", f.runs)
	}
}

func TestPresent(t *testing.T) {
	f := newFake(t)
	p := New(f, Options{})
	if p.Present() || p.Name() != "pacman" {
		t.Error("pacman isn't installed in the fake")
	}
	f.have["pacman"] = true
	if !p.Present() {
		t.Error("pacman is installed in the fake")
	}
}

func TestRealpath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	p := New(nil, Options{Root: root})
	for in, want := range map[string]string{
		"/lib":              "/usr/lib",
		"/lib/missing.so":   "/usr/lib/missing.so", // a missing file keeps its name
		"/usr/lib/":         "/usr/lib",
		"/nowhere/at/all":   "/nowhere/at/all",
		"/etc/../usr/lib/x": "/usr/lib/x",
	} {
		if got := p.realpath(in); got != want {
			t.Errorf("realpath(%q) = %q, want %q", in, got, want)
		}
	}
	if !slices.Equal([]string{p.realpath("/")}, []string{"/"}) {
		t.Errorf("realpath(/) = %q", p.realpath("/"))
	}
}
