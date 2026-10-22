package detect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeSystem is a machine's /var/lib, /var/opt and /srv, with the app data a scan should find
// and everything it should leave alone.
func fakeSystem(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		// Suggested:
		"var/lib/jellyfin/data/library.db": strings.Repeat("x", 4096), // named like jellyfin-server
		"var/lib/jellyfin/cache/media/a":   "",                        // a mount: neither walked nor counted
		"var/lib/postgres/data/PG_VERSION": "16\n",                    // the folder belongs to postgresql
		"var/lib/private/hass/config.yaml": "x\n",                     // home-assistant.service's StateDirectory=
		"var/lib/library/books.db":         "x\n",                     // bookworm.service's User= owns it, when known
		"var/opt/mystery/data":             "x\n",                     // nobody knows whose
		// Left alone:
		"var/lib/pacman/local/ALPM_DB_VERSION": "9\n", // a known system folder
		"var/lib/foo-tool/defaults.conf":       "x\n", // every file is a package's
		"var/lib/scratch/tmp":                  "x\n", // detect.ignore
		"var/lib/backedup/x":                   "x\n", // backup.extra_paths
		"var/lib/docker-cache/x":               "x\n", // the excludes
		"var/lib/nas/x":                        "x\n", // another filesystem
		"srv/www/index.html":                   "x\n", // /srv is backed up
		"etc/passwd":                           "root:x:0:0::/root:/usr/bin/bash\n",
	} {
		write(t, filepath.Join(root, name), body)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/lib/empty/sub"), 0o755); err != nil { // no data
		t.Fatal(err)
	}
	if err := os.Symlink("private/hass", filepath.Join(root, "var/lib/hass")); err != nil { // DynamicUser's link
		t.Fatal(err)
	}
	return root
}

var testItems = []record.Item{
	{Type: record.TypePkg, ID: "jellyfin-server", Bundle: "jellyfin"},
	{Type: record.TypePkg, ID: "jellyfin-web", AsDeps: true},
	{Type: record.TypePkg, ID: "postgresql", Bundle: "packages"},
	{Type: record.TypePkg, ID: "home-assistant", Bundle: "packages"},
	{Type: record.TypeAUR, ID: "bookworm-server", Bundle: "aur"},
	{Type: record.TypeSvc, ID: "jellyfin.service", Pkg: "jellyfin-server"},
	{Type: record.TypeSvc, ID: "home-assistant.service", Pkg: "home-assistant"},
	{Type: record.TypeSvc, ID: "bookworm.service", Pkg: "bookworm-server"},
	{Type: record.TypeSvc, ID: "my-own.service"}, // hand-made: no package to name
}

var testKnown = []string{"/var/lib/pacman", "/var/lib/system*"}

func fullScanOptions(root string, f *sysexec.Fake) ScanOptions {
	return ScanOptions{
		Root:     root,
		Runner:   f,
		Manager:  pacman.New(f, pacman.Options{Root: root}),
		Items:    testItems,
		Covered:  []string{"/etc", "/srv", "/var/lib/backedup"},
		Excluded: func(p string) bool { return p == "/var/lib/docker-cache" },
		Mounts: dest.MountTable{
			{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda2"},
			{ID: 2, MountPoint: "/var/lib/nas", FSType: "nfs4", Source: "server:/export"},
			{ID: 3, MountPoint: "/var/lib/jellyfin/cache", FSType: "tmpfs", Source: "tmpfs"},
		},
		Ignore:  []string{"/var/lib/scratch"},
		Known:   testKnown,
		LargeGB: 2048.0 / gib, // 2 KiB
		All:     true,
	}
}

func fullScanFake(t *testing.T) *sysexec.Fake {
	t.Helper()
	f := &sysexec.Fake{}
	f.On("pacman -Qlq", sysexec.Response{Stdout: testdata(t, "qlq.txt")})
	f.On("env LC_ALL=C pacman -Qo /var/lib/jellyfin /var/lib/library /var/lib/postgres /var/lib/private/hass /var/opt/mystery",
		sysexec.Response{Stdout: testdata(t, "qo-folders.txt"), Code: 1})
	f.On("systemctl show -p User -p DynamicUser -p StateDirectory -- bookworm.service home-assistant.service jellyfin.service",
		sysexec.Response{Stdout: testdata(t, "systemctl-show.txt")})
	return f
}

type found struct{ path, app, bundle string }

func summary(es []state.Entry) []found {
	var out []found
	for _, e := range es {
		out = append(out, found{e.Path, e.App, e.Bundle})
	}
	return out
}

// The nightly scan of a fake system suggests exactly its unowned data folders, each with its app.
func TestScanFindsUnownedDataFolders(t *testing.T) {
	root := fakeSystem(t)
	f := fullScanFake(t)
	res, err := Scan(context.Background(), fullScanOptions(root, f))
	if err != nil {
		t.Fatal(err)
	}
	want := []found{
		{"/var/lib/jellyfin", "jellyfin-server", "jellyfin"},              // by name, into its app bundle
		{"/var/lib/library", "", ""},                                      // its owner isn't a known account
		{"/var/lib/postgres", "postgresql", "app-postgresql"},             // owns the folder
		{"/var/lib/private/hass", "home-assistant", "app-home-assistant"}, // the unit's state folder
		{"/var/opt/mystery", "", ""},
	}
	if got := summary(res.Found); !reflect.DeepEqual(got, want) {
		t.Errorf("found\n %v\nwant\n %v", got, want)
	}
	if !res.All {
		t.Error("a scan of everything isn't marked so")
	}
	for _, e := range res.Found {
		if e.ID != state.EntryID(state.KindPath, e.Path) || e.Kind != state.KindPath {
			t.Errorf("%s: id %q kind %q", e.Path, e.ID, e.Kind)
		}
		switch e.Path {
		case "/var/lib/jellyfin":
			if e.Size != 4096 || !e.Large {
				t.Errorf("jellyfin: size %d large %v; the mount inside isn't counted", e.Size, e.Large)
			}
		default:
			if e.Size == 0 || e.Large {
				t.Errorf("%s: size %d large %v", e.Path, e.Size, e.Large)
			}
		}
	}
}

// A folder owned by a service's User= goes with that service's package.
func TestScanNamesTheAppByTheUnitsUser(t *testing.T) {
	root := fakeSystem(t)
	// Every file of the tree belongs to whoever runs the test: make that bookworm.service's user.
	write(t, filepath.Join(root, "etc/passwd"), fmt.Sprintf("root:x:0:0::/root:/usr/bin/bash\nbookworm:x:%d:%d::/var/lib/library:/usr/bin/nologin\n", os.Getuid(), os.Getgid()))
	if os.Getuid() == 0 {
		t.Skip("running as root: every file belongs to root, which says nothing of its app")
	}
	res, err := Scan(context.Background(), fullScanOptions(root, fullScanFake(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Found {
		if e.Path == "/var/lib/library" && (e.App != "bookworm-server" || e.Bundle != "app-bookworm-server") {
			t.Errorf("library: app %q bundle %q", e.App, e.Bundle)
		}
	}
}

// The pacman hook only looks at the folders of the installed packages it was given.
func TestScanOnlyLooksAtThePackagesFolders(t *testing.T) {
	root := fakeSystem(t)
	f := &sysexec.Fake{}
	f.On("pacman -Qq", sysexec.Response{Stdout: "jellyfin-server\njellyfin-web\npostgresql\n"})
	f.On("pacman -Ql jellyfin-server", sysexec.Response{Stdout: testdata(t, "ql-jellyfin.txt")})
	f.On("env LC_ALL=C pacman -Qo /var/lib/jellyfin/data/library.db", sysexec.Response{Stderr: "error: No package owns /var/lib/jellyfin/data/library.db", Code: 1})
	f.On("env LC_ALL=C pacman -Qo /var/lib/jellyfin", sysexec.Response{Stdout: testdata(t, "qo-jellyfin.txt")})
	f.On("systemctl show -p User -p DynamicUser -p StateDirectory -- bookworm.service home-assistant.service jellyfin.service",
		sysexec.Response{Stdout: testdata(t, "systemctl-show.txt")})
	opt := fullScanOptions(root, f)
	opt.All, opt.Only = false, []string{"jellyfin-server", "removed-app"}
	res, err := Scan(context.Background(), opt)
	if err != nil {
		t.Fatalf("%v\n%q", err, f.Commands())
	}
	if got, want := summary(res.Found), []found{{"/var/lib/jellyfin", "jellyfin-server", "jellyfin"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	if res.All {
		t.Error("the hook's scan is marked as a scan of everything")
	}
}

// Files of other packages in a package's folder are asked about; when they're owned, the folder
// holds no new data.
func TestScanAsksAboutOtherPackagesFiles(t *testing.T) {
	root := fakeSystem(t)
	f := &sysexec.Fake{}
	f.On("pacman -Qq", sysexec.Response{Stdout: "foo-tool\n"})
	f.On("pacman -Ql foo-tool", sysexec.Response{Stdout: "foo-tool /usr/bin/foo-tool\n"})
	f.On("env LC_ALL=C pacman -Qo /var/lib/foo-tool/defaults.conf", sysexec.Response{Stdout: "/var/lib/foo-tool/defaults.conf is owned by foo-tool-data 1-1\n"})
	opt := fullScanOptions(root, f)
	opt.All, opt.Only = false, []string{"foo-tool"}
	res, err := Scan(context.Background(), opt)
	if err != nil || len(res.Found) != 0 {
		t.Errorf("found %v, %v", summary(res.Found), err)
	}
}

func TestScanStopsWithItsContext(t *testing.T) {
	root := fakeSystem(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, fullScanOptions(root, fullScanFake(t))); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestReadKnownFolders(t *testing.T) {
	got, err := ReadKnownFolders("../../catalog/system-folders.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/var/lib/pacman", "/var/lib/systemd", "/var/lib/dbus", "/var/lib/carryon"} {
		found := false
		for _, k := range got {
			found = found || k == want
		}
		if !found {
			t.Errorf("the shipped list lacks %s", want)
		}
	}
	for _, k := range got {
		if !strings.HasPrefix(k, "/") || strings.Contains(k, "#") {
			t.Errorf("odd entry %q", k)
		}
	}
	if _, err := ReadKnownFolders(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing list isn't an error")
	}
}

// Folders the system makes again by itself are never suggested, with the shipped list.
func TestScanLeavesTheShippedSystemFoldersAlone(t *testing.T) {
	known, err := ReadKnownFolders("../../catalog/system-folders.txt")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{
		"var/lib/lastlog/lastlog2.db",
		"var/lib/urandom/random-seed",
		"var/lib/plocate/plocate.db",
		"var/lib/mlocate/mlocate.db",
		"var/lib/passim/data/firmware.cab",
		"var/lib/plasmalogin/state.conf",
		"var/lib/sysbackup/state.json",
		"var/lib/chwd/local/pci/nvidia-open-dkms/profiles.toml",
		"var/opt/mystery/data", // not a system folder: suggested
	} {
		write(t, filepath.Join(root, name), "x\n")
	}
	f := &sysexec.Fake{}
	f.On("pacman -Qlq", sysexec.Response{})
	f.On("env LC_ALL=C pacman -Qo /var/opt/mystery", sysexec.Response{Stderr: "error: No package owns /var/opt/mystery\n", Code: 1})
	opt := ScanOptions{
		Root:    root,
		Runner:  f,
		Manager: pacman.New(f, pacman.Options{Root: root}),
		Mounts:  dest.MountTable{{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda2"}},
		Known:   known,
		LargeGB: 10,
		All:     true,
	}
	res, err := Scan(context.Background(), opt)
	if err != nil {
		t.Fatalf("%v\n%q", err, f.Commands())
	}
	if got, want := summary(res.Found), []found{{"/var/opt/mystery", "", ""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
}

func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	tmp := t.TempDir()
	d := paths.Dirs{Conf: filepath.Join(tmp, "conf"), State: filepath.Join(tmp, "state"), Share: filepath.Join(tmp, "share")}
	if err := os.MkdirAll(d.Conf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(d.ConfigFile(), config.Default()); err != nil {
		t.Fatal(err)
	}
	return d
}

func entry(p, app, bundle string) state.Entry {
	e := state.NewEntry(state.KindPath, p)
	e.App, e.Bundle, e.Size = app, bundle, 10
	return e
}

func TestSuggestAsk(t *testing.T) {
	d := testDirs(t)
	day1 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	jelly, gone := entry("/var/lib/jellyfin", "jellyfin-server", "jellyfin"), entry("/var/lib/old", "", "")
	if _, err := state.UpdateInbox(d, time.Second, func(in *state.Inbox) error {
		in.Put(state.NewEntry(state.KindRestore, "app-obs-studio")) // not a folder: stays
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Suggest(Findings{Found: []state.Entry{jelly, gone}}, SuggestOptions{Dirs: d, Mode: "ask", By: state.ByHook, Wait: time.Second, Now: func() time.Time { return day1 }})
	if err != nil || len(res.New) != 2 {
		t.Fatalf("new %v, %v", summary(res.New), err)
	}
	// Found again by a scan of everything: kept as first found, sized anew; the other is stale.
	jelly.Size = 20
	res, err = Suggest(Findings{Found: []state.Entry{jelly}, All: true}, SuggestOptions{Dirs: d, Mode: "ask", By: state.ByBackup, Wait: time.Second, Now: func() time.Time { return day2 }})
	if err != nil || len(res.New) != 0 || res.Stale != 1 {
		t.Fatalf("new %v stale %d, %v", summary(res.New), res.Stale, err)
	}
	in, _ := state.ReadInbox(d)
	if len(in.Entries) != 2 {
		t.Fatalf("inbox %+v", in.Entries)
	}
	e, ok := in.Find("/var/lib/jellyfin")
	if !ok || !e.Found.Equal(day1) || e.By != state.ByHook || e.Size != 20 || e.App != "jellyfin-server" {
		t.Errorf("entry %+v", e)
	}
	if _, ok := in.Find(state.EntryID(state.KindRestore, "app-obs-studio")); !ok {
		t.Error("a scan took out a restore offer")
	}
}

func TestSuggestAutoBacksItUp(t *testing.T) {
	d := testDirs(t)
	var out bytes.Buffer
	jelly := entry("/var/lib/jellyfin", "jellyfin-server", "app-jellyfin-server")
	res, err := Suggest(Findings{Found: []state.Entry{jelly}, All: true}, SuggestOptions{Dirs: d, Mode: "auto", By: state.ByBackup, Wait: time.Second, Out: &out})
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("added %v, %v", summary(res.Added), err)
	}
	cfg, _, err := config.Load(d.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg.Backup.ExtraPaths, " "), "/var/lib/jellyfin") {
		t.Errorf("extra_paths = %q", cfg.Backup.ExtraPaths)
	}
	rules, _ := os.ReadFile(d.BundlesFile())
	if !strings.Contains(string(rules), "put app-jellyfin-server path /var/lib/jellyfin") {
		t.Errorf("bundles.conf:\n%s", rules)
	}
	if in, _ := state.ReadInbox(d); len(in.Entries) != 0 {
		t.Errorf("auto left suggestions: %+v", in.Entries)
	}
	if !strings.Contains(out.String(), "Added /var/lib/jellyfin to backup.extra_paths") {
		t.Errorf("out:\n%s", out.String())
	}
}

func TestSuggestOff(t *testing.T) {
	d := testDirs(t)
	res, err := Suggest(Findings{Found: []state.Entry{entry("/var/lib/x", "", "")}, All: true}, SuggestOptions{Dirs: d, Mode: "off", Wait: time.Second})
	if err != nil || len(res.New)+len(res.Added) != 0 {
		t.Errorf("%+v, %v", res, err)
	}
	if _, err := os.Stat(d.InboxFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("off wrote the inbox: %v", err)
	}
}

// A busy inbox makes a short-waiting writer give up, not wait.
func TestSuggestGivesUpOnABusyInbox(t *testing.T) {
	d := testDirs(t)
	release := holdInbox(t, d)
	defer release()
	start := time.Now()
	_, err := Suggest(Findings{Found: []state.Entry{entry("/var/lib/x", "", "")}}, SuggestOptions{Dirs: d, Mode: "ask", Wait: 50 * time.Millisecond})
	if !errors.Is(err, state.ErrInboxBusy) {
		t.Errorf("err = %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %s", took)
	}
}

// holdInbox takes the inbox lock until release is called.
func holdInbox(t *testing.T, d paths.Dirs) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = state.UpdateInbox(d, time.Second, func(*state.Inbox) error {
			close(held)
			<-done
			return nil
		})
	}()
	<-held
	return func() { close(done); <-finished }
}
