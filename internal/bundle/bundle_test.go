package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fakeMachine answers from testdata/machine.json.
type fakeMachine struct {
	Host               string              `json:"host"`
	RepoOf             map[string]string   `json:"repo_of"`
	FingerprintReaders []string            `json:"fingerprint_readers"`
	BluetoothMACs      []string            `json:"bluetooth_macs"`
	ModulesExist       []string            `json:"modules_exist"`
	Loaded             []string            `json:"loaded_modules"`
	Firmware           map[string][]string `json:"firmware"`
	Holders            map[string][]string `json:"holders"`
	Devices            map[string][]string `json:"devices"`
	DeviceNames        map[string]string   `json:"device_names"`
	USBPresent         []string            `json:"usb_present"`
	PackageFiles       map[string][]string `json:"package_files"`
	Files              map[string]string   `json:"files"`
	Which              map[string]string   `json:"which"`
	Owned              map[string]string   `json:"owners"`
	RecordFiles        map[string]string   `json:"record_files"`
}

func (m *fakeMachine) ReadFile(p string) ([]byte, error) {
	if s, ok := m.Files[p]; ok {
		return []byte(s), nil
	}
	return nil, os.ErrNotExist
}
func (m *fakeMachine) LookPath(prog string) string { return m.Which[prog] }
func (m *fakeMachine) Owners(_ context.Context, paths []string) map[string]string {
	out := map[string]string{}
	for _, p := range paths {
		if o, ok := m.Owned[p]; ok {
			out[p] = o
		}
	}
	return out
}
func (m *fakeMachine) FilesOf(_ context.Context, pkgs []string) []string {
	var out []string
	for _, p := range pkgs {
		out = append(out, m.PackageFiles[p]...)
	}
	return out
}
func (m *fakeMachine) ModuleExists(_ context.Context, mod string) bool {
	return slices.Contains(m.ModulesExist, mod)
}
func (m *fakeMachine) LoadedModules() []string { return m.Loaded }
func (m *fakeMachine) ModuleFirmware(_ context.Context, mod string) []string {
	return m.Firmware[mod]
}
func (m *fakeMachine) ModuleHolders(mod string) []string    { return m.Holders[mod] }
func (m *fakeMachine) DevicesForModule(mod string) []string { return m.Devices[mod] }
func (m *fakeMachine) DeviceName(_ context.Context, id string) string {
	if n, ok := m.DeviceNames[id]; ok {
		return n
	}
	return id
}
func (m *fakeMachine) USBDevices() []string { return m.USBPresent }

// desktopInput is testdata/desktop.items.jsonl with the fake machine and bundles.conf. The record
// folder holds the copies of the PAM files. With keep set, only the items it accepts are given.
func desktopInput(t *testing.T, keep func(record.Item) bool) Input {
	t.Helper()
	b, err := os.ReadFile("testdata/machine.json")
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMachine{}
	if err := json.Unmarshal(b, m); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/desktop.items.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	all, err := record.ReadItems(f)
	if err != nil {
		t.Fatal(err)
	}
	var items []record.Item
	for _, it := range all {
		if keep == nil || keep(it) {
			items = append(items, it)
		}
	}
	conf, err := os.Open("testdata/bundles.conf")
	if err != nil {
		t.Fatal(err)
	}
	defer conf.Close()
	r, problems, err := rules.ParseBundleRules(conf, "bundles.conf")
	if err != nil || len(problems) > 0 {
		t.Fatalf("bundles.conf: %v %v", err, problems)
	}
	dir := t.TempDir()
	for p, text := range m.RecordFiles {
		full := filepath.Join(dir, "files", p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Input{
		Items: items, Rules: r, Host: m.Host, RepoOf: m.RepoOf, FingerprintReaders: m.FingerprintReaders,
		BluetoothMACs: m.BluetoothMACs, RecordDir: dir, Machine: m,
	}
}

// render lists the bundles and then the items, one per line, like testdata/prototype.py does.
// withUser adds each item's user.
func render(items []record.Item, bundles record.Bundles, withUser bool) string {
	var b strings.Builder
	for _, x := range bundles {
		fmt.Fprintf(&b, "bundle %s | %s | %s\n", x.ID, x.When, x.Title)
	}
	for _, it := range items {
		fmt.Fprintf(&b, "item %s %s", it.Bundle, it.Key())
		if withUser && it.User != "" {
			fmt.Fprintf(&b, " (%s)", it.User)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Deliberate differences from the prototype (see docs/design.md).
var (
	changedTitles = map[string]string{
		"packages": "Packages from your distribution's repositories",
		"aur":      "AUR packages (rebuilt with paru or yay)",
	}
	// files with disk UUIDs always go to disk-layout
	movedItems = map[string]string{"file:/etc/default/limine": "disk-layout"}
)

// TestMatchesPrototype groups alice's part of the desktop like the prototype did. Its expected
// output, testdata/prototype.golden.txt, is written by the prototype's own assign_bundles: see
// testdata/prototype.py.
func TestMatchesPrototype(t *testing.T) {
	in := desktopInput(t, func(it record.Item) bool { return it.User == "" || it.User == "alice" })
	items, bundles := Assign(context.Background(), in)
	SortItems(items)
	got := render(items, bundles, false)

	b, err := os.ReadFile("testdata/prototype.golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := applyChanges(t, string(b))
	if got != want {
		t.Errorf("grouping differs from the prototype's:\n%s", lineDiff(want, got))
	}
}

// applyChanges applies the deliberate differences to the prototype's output.
func applyChanges(t *testing.T, golden string) string {
	var bundleLines []string
	var items []record.Item
	for _, line := range strings.Split(strings.TrimSpace(golden), "\n") {
		switch {
		case strings.HasPrefix(line, "bundle "):
			f := strings.SplitN(strings.TrimPrefix(line, "bundle "), " | ", 3)
			if title, ok := changedTitles[f[0]]; ok {
				f[2] = title
			}
			bundleLines = append(bundleLines, "bundle "+strings.Join(f, " | "))
		case strings.HasPrefix(line, "item "):
			f := strings.SplitN(strings.TrimPrefix(line, "item "), " ", 2)
			typ, id, _ := strings.Cut(f[1], ":")
			it := record.Item{Bundle: f[0], Type: typ, ID: id}
			if b, ok := movedItems[it.Key()]; ok {
				it.Bundle = b
			}
			items = append(items, it)
		default:
			t.Fatalf("prototype.golden.txt: can't read %q", line)
		}
	}
	SortItems(items)
	var b strings.Builder
	for _, l := range bundleLines {
		b.WriteString(l + "\n")
	}
	for _, it := range items {
		fmt.Fprintf(&b, "item %s %s\n", it.Bundle, it.Key())
	}
	return b.String()
}

// TestDesktop groups the whole desktop, alice's and bob's items.
func TestDesktop(t *testing.T) {
	in := desktopInput(t, nil)
	before := slices.Clone(in.Items)
	items, bundles := Assign(context.Background(), in)
	if !reflect.DeepEqual(in.Items, before) {
		t.Error("Assign changed its input")
	}
	SortItems(items)
	golden(t, "desktop.golden.txt", []byte(render(items, bundles, true)))

	byBundle := map[string]int{}
	for _, it := range items {
		if it.Bundle == "" {
			t.Errorf("%s has no bundle", it.Key())
		}
		if _, ok := bundles.Get(it.Bundle); !ok {
			t.Errorf("%s is in bundle %q, which isn't listed", it.Key(), it.Bundle)
		}
		byBundle[it.Bundle]++
	}
	for _, b := range bundles {
		if byBundle[b.ID] == 0 {
			t.Errorf("bundle %s is empty", b.ID)
		}
		if err := rules.Check(b.When); err != nil {
			t.Errorf("bundle %s: %v", b.ID, err)
		}
	}
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
		t.Fatalf("%v (run go test ./internal/bundle -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run go test ./internal/bundle -update after checking the change):\n%s",
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
	if len(out) == 0 {
		return "(same lines, different order)"
	}
	return strings.Join(out, "\n")
}

// assign groups items without a machine and returns each item's bundle by key and user.
func assign(t *testing.T, in Input) (map[string]string, record.Bundles) {
	t.Helper()
	items, bundles := Assign(context.Background(), in)
	got := map[string]string{}
	for _, it := range items {
		k := it.Key()
		if it.User != "" {
			k += "/" + it.User
		}
		got[k] = it.Bundle
	}
	return got, bundles
}

func TestSeveralUsers(t *testing.T) {
	got, _ := assign(t, Input{Items: []record.Item{
		{Type: "pkg", ID: "kitty", Repo: "extra"},
		{Type: "path", ID: "/home/alice/.config/kitty", Home: ".config/kitty", User: "alice"},
		{Type: "path", ID: "/home/bob/.config/kitty", Home: ".config/kitty", User: "bob"},
		{Type: "path", ID: "/home/bob/.ssh", Home: ".ssh", User: "bob"},
		{Type: "path", ID: "/home/bob/Documents", Home: "Documents", User: "bob"},
		{Type: "path", ID: "/home/bob/.config/monitors.xml", Home: ".config/monitors.xml", User: "bob"},
		// each user's unit needs follow the unit
		{Type: "uunit", ID: "sync.service", User: "alice"},
		{Type: "uunit", ID: "sync.service", User: "bob"},
		{Type: "mkdir", ID: "/home/alice/Sync", Rel: "Sync", ForUnit: "sync.service", User: "alice"},
		{Type: "mkdir", ID: "/home/bob/Sync", Rel: "Sync", ForUnit: "sync.service", User: "bob"},
		{Type: "pkg", ID: "unison", AsDeps: true, ForUnit: "sync.service"},
	}, Rules: rules.BundleRules{Puts: []rules.Put{
		{Bundle: "my-sync", Match: rules.ItemMatch{Type: "uunit", Glob: rules.CompileGlob("sync.service")}},
	}}})
	want := map[string]string{
		"pkg:kitty":                               "app-kitty",
		"path:/home/alice/.config/kitty/alice":    "app-kitty",
		"path:/home/bob/.config/kitty/bob":        "app-kitty",
		"path:/home/bob/.ssh/bob":                 "home-keys",
		"path:/home/bob/Documents/bob":            "home-files",
		"path:/home/bob/.config/monitors.xml/bob": "home-display",
		"uunit:sync.service/alice":                "my-sync",
		"uunit:sync.service/bob":                  "my-sync",
		"mkdir:/home/alice/Sync/alice":            "my-sync",
		"mkdir:/home/bob/Sync/bob":                "my-sync",
		"pkg:unison":                              "my-sync",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}

	// one user's unit names an app bundle for everyone's settings folders named like it
	got, _ = assign(t, Input{Items: []record.Item{
		{Type: "uunit", ID: "notes-sync.service", User: "bob"},
		{Type: "mkdir", ID: "/home/bob/Notes", Rel: "Notes", ForUnit: "notes-sync.service", User: "bob"},
		{Type: "path", ID: "/home/alice/.config/notes", Home: ".config/notes", User: "alice"},
	}})
	want = map[string]string{
		"uunit:notes-sync.service/bob":         "notes",
		"mkdir:/home/bob/Notes/bob":            "notes",
		"path:/home/alice/.config/notes/alice": "notes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}
}

func TestChanges(t *testing.T) {
	got, _ := assign(t, Input{Items: []record.Item{
		// files with disk UUIDs always go to disk-layout, even when an app or a rule names them
		{Type: "aur", ID: "grub-customizer"},
		{Type: "pkg", ID: "grub", Repo: "core"},
		{Type: "file", ID: "/etc/default/grub", Kind: "modified", Pkg: "grub"},
		{Type: "file", ID: "/etc/fstab", Kind: "modified", Pkg: "filesystem"},
		// edited package files are package tweaks
		{Type: "file", ID: "/usr/share/applications/htop.desktop", Kind: "edited", Pkg: "htop"},
		// firmware right in the firmware folder
		{Type: "file", ID: "/usr/lib/firmware/blob.bin", Kind: "unowned"},
	}, Rules: rules.BundleRules{Puts: []rules.Put{
		{Bundle: "mine", Match: rules.ItemMatch{Type: "file", Glob: rules.CompileGlob("/etc/fstab")}},
	}}})
	want := map[string]string{
		"aur:grub-customizer":    "grub",
		"pkg:grub":               "grub",
		"file:/etc/default/grub": "disk-layout",
		"file:/etc/fstab":        "disk-layout",
		"file:/usr/share/applications/htop.desktop": "configs",
		"file:/usr/lib/firmware/blob.bin":           "firmware-files",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}
}

func TestUserRules(t *testing.T) {
	r, problems, err := rules.ParseBundleRules(strings.NewReader(`
title games My games
when games chassis:desktop
when locale never
title kitty-setup kitty
put games path ~/.local/share/Steam
put games * *steam*
put first pkg kitty
put second pkg kitty
drop path ~/.cache
drop pkg *-debug
`), "bundles.conf")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	items, bundles := Assign(context.Background(), Input{Rules: r, Items: []record.Item{
		{Type: "path", ID: "/home/bob/.local/share/Steam", Home: ".local/share/Steam", User: "bob"},
		{Type: "pkg", ID: "steam", Repo: "multilib"},
		{Type: "pkg", ID: "kitty", Repo: "extra"},
		{Type: "pkg", ID: "kitty-debug", Repo: "extra"},
		{Type: "path", ID: "/home/alice/.cache", Home: ".cache", User: "alice"},
		{Type: "file", ID: "/etc/locale.conf", Kind: "unowned"},
	}})
	var got []string
	for _, it := range items {
		got = append(got, it.Bundle+" "+it.Key())
	}
	sort.Strings(got)
	want := []string{"first pkg:kitty", "games path:/home/bob/.local/share/Steam", "games pkg:steam", "locale file:/etc/locale.conf"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	wantBundles := record.Bundles{
		{ID: "first", Title: "First (auto-grouped)", When: "always"},
		{ID: "games", Title: "My games", When: "chassis:desktop"},
		{ID: "locale", Title: "Locale, keyboard & timezone", When: "never"},
	}
	if !reflect.DeepEqual(bundles, wantBundles) {
		t.Errorf("bundles = %+v", bundles)
	}
}

func TestWithoutMachine(t *testing.T) {
	got, bundles := assign(t, Input{Host: "desktop", Items: []record.Item{
		{Type: "kparam", ID: "nvidia-drm.modeset=1"},
		{Type: "kparam", ID: "vfio-pci.ids=1234:abcd"},
		{Type: "file", ID: "/usr/lib/firmware/acme/fw.bin", Kind: "unowned"},
		{Type: "file", ID: "/etc/pam.d/sudo", Kind: "modified", Pkg: "sudo"},
		{Type: "path", ID: "/home/alice/.config/autostart/x.desktop", Home: ".config/autostart/x.desktop", User: "alice"},
	}})
	want := map[string]string{
		"kparam:nvidia-drm.modeset=1":                        "kparams",
		"kparam:vfio-pci.ids=1234:abcd":                      "kparam-vfio-pci.ids",
		"file:/usr/lib/firmware/acme/fw.bin":                 "firmware-acme",
		"file:/etc/pam.d/sudo":                               "configs",
		"path:/home/alice/.config/autostart/x.desktop/alice": "home-apps",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}
	for _, b := range bundles {
		switch b.ID {
		case "kparam-vfio-pci.ids":
			if b.Title != "Kernel param vfio-pci.ids=1234:abcd (hides pci:1234:abcd)" || b.When != "hw:pci:1234:abcd" {
				t.Errorf("%+v", b)
			}
		case "firmware-acme":
			if b.Title != "Firmware acme" || b.When != "samehost" {
				t.Errorf("%+v", b)
			}
		}
	}
}

func TestFirmwareWhen(t *testing.T) {
	m := &fakeMachine{
		Loaded:   []string{"wifi", "generic", "btmtk"},
		Firmware: map[string][]string{"wifi": {"acme/w.bin"}, "generic": {"acme/g.bin"}, "btmtk": {"acme/bt.bin"}},
		Holders:  map[string][]string{"btmtk": {"btusb"}},
		Devices: map[string][]string{
			"wifi":    {"pci:1234:0001"},
			"generic": {"pci:1234:0010", "pci:1234:0011", "pci:1234:0012", "pci:1234:0013"},
			"btusb":   {"usb:1234:0b01"},
		},
	}
	g := &grouper{ctx: context.Background(), m: m}
	tests := []struct {
		paths []string
		want  string
	}{
		{[]string{"/usr/lib/firmware/acme/w.bin.zst"}, "hw:pci:1234:0001"},
		{[]string{"/usr/lib/firmware/acme/g.bin"}, "samehost"}, // a driver of 4 devices says nothing
		{[]string{"/usr/lib/firmware/acme/bt.bin.xz"}, "hw:usb:1234:0b01"},
		{[]string{"/usr/lib/firmware/acme/w.bin", "/usr/lib/firmware/acme/bt.bin"}, "hw:pci:1234:0001,usb:1234:0b01"},
		{[]string{"/usr/lib/firmware/acme/unused.bin"}, "samehost"},
	}
	for _, tt := range tests {
		if got, _ := g.firmwareWhen(tt.paths); got != tt.want {
			t.Errorf("firmwareWhen(%v) = %q, want %q", tt.paths, got, tt.want)
		}
	}
}

func TestStemOf(t *testing.T) {
	for name, want := range map[string]string{
		"rclone-mount.service": "rclone",
		"rclone-backup.timer":  "rclone",
		"old-helper":           "old-helper",
		"paru-bin":             "paru",
		"Foo-BIN":              "foo-bin", // -bin is cut before lowering, so only in lower case
		"openlinkhub":          "openlinkhub",
		"obs.service":          "obs",
		"yay-git":              "yay",
	} {
		if got := stemOf(name); got != want {
			t.Errorf("stemOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestStemMatches(t *testing.T) {
	repoOf := map[string]string{"example-tool": "example"}
	tests := []struct {
		it   record.Item
		stem string
		want bool
	}{
		{record.Item{Type: "pkg", ID: "rclone"}, "rclone", true},
		{record.Item{Type: "pkg", ID: "rclone-browser"}, "rclone", true},
		{record.Item{Type: "pkg", ID: "rclonex"}, "rclone", false},
		{record.Item{Type: "pkg", ID: "example-tool"}, "example", true}, // from that repository
		{record.Item{Type: "repo", ID: "example"}, "example", true},
		{record.Item{Type: "usvc", ID: "rclone@x.service"}, "rclone", true},
		{record.Item{Type: "file", ID: "/etc/Rclone/x.conf"}, "rclone", true},
		{record.Item{Type: "path", ID: "/home/alice/.config/rclone", Home: ".config/rclone"}, "rclone", true},
		{record.Item{Type: "path", ID: "/home/alice/rclone/x", Home: "rclone/x"}, "rclone", false}, // only the name counts
		{record.Item{Type: "path", ID: "/var/lib/rclone", Extra: true}, "rclone", true},
		{record.Item{Type: "path", ID: "/var/lib/rclone"}, "rclone", false}, // a built-in path
		{record.Item{Type: "mkdir", ID: "/var/log/rclone"}, "rclone", false},
	}
	for _, tt := range tests {
		if got := stemMatches(tt.it, tt.stem, repoOf); got != tt.want {
			t.Errorf("stemMatches(%s, %q) = %v", tt.it.Key(), tt.stem, got)
		}
	}
}

func TestAppStems(t *testing.T) {
	got, _ := assign(t, Input{Items: []record.Item{
		// an AUR package alone makes no bundle
		{Type: "aur", ID: "paru-bin"},
		// an AUR package and its settings do
		{Type: "aur", ID: "brave-bin"},
		{Type: "path", ID: "/home/alice/.config/BraveSoftware", Home: ".config/BraveSoftware", User: "alice"},
		// a hand-made unit makes one alone; the longest name wins
		{Type: "svc", ID: "backup.service"},
		{Type: "svc", ID: "backupctl.service"},
		{Type: "file", ID: "/etc/backupctl.conf", Kind: "unowned"},
		// short names don't count
		{Type: "svc", ID: "foo.service"},
		{Type: "pkg", ID: "foo"},
	}})
	want := map[string]string{
		"aur:paru-bin": "aur", "aur:brave-bin": "brave", "path:/home/alice/.config/BraveSoftware/alice": "brave",
		"svc:backup.service": "backup", "svc:backupctl.service": "backupctl", "file:/etc/backupctl.conf": "backupctl",
		"svc:foo.service": "services", "pkg:foo": "packages",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}
}

func TestEditedConfigs(t *testing.T) {
	got, bundles := assign(t, Input{Items: []record.Item{
		{Type: "file", ID: "/etc/ssh/sshd_config", Kind: "modified", Pkg: "openssh"},
		{Type: "svc", ID: "sshd.service", Pkg: "openssh"},
		{Type: "file", ID: "/etc/docker/daemon.json", Kind: "modified", Pkg: "docker"},
		{Type: "file", ID: "/etc/systemd/system/docker.service.d/x.conf", Kind: "unowned"},
		{Type: "file", ID: "/etc/dockerish.conf", Kind: "unowned"}, // not named after docker
		{Type: "file", ID: "/etc/makepkg.conf", Kind: "modified", Pkg: "pacman"},
	}})
	want := map[string]string{
		"file:/etc/ssh/sshd_config": "cfg-openssh", "svc:sshd.service": "cfg-openssh",
		"file:/etc/docker/daemon.json": "cfg-docker", "file:/etc/systemd/system/docker.service.d/x.conf": "cfg-docker",
		"file:/etc/dockerish.conf": "other-files", "file:/etc/makepkg.conf": "configs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bundles:\n got %v\nwant %v", got, want)
	}
	titles := map[string]string{}
	for _, b := range bundles {
		titles[b.ID] = b.Title
	}
	if titles["cfg-openssh"] != "openssh settings + service" || titles["cfg-docker"] != "docker settings" {
		t.Errorf("titles = %v", titles)
	}
}

func TestNamedAfter(t *testing.T) {
	for _, tt := range []struct {
		name, pkg string
		want      bool
	}{
		{"/etc/docker/daemon.json", "docker", true},
		{"/etc/systemd/system/docker.service", "docker", true},
		{"/etc/dockerish", "docker", false},
		{"/etc/my-docker", "docker", true},
		{"/etc/xdocker/docker_x", "docker", true}, // the second match counts
		{"docker", "docker", true},
		{"/etc/x", "", false},
	} {
		if got := namedAfter(tt.name, tt.pkg); got != tt.want {
			t.Errorf("namedAfter(%q, %q) = %v", tt.name, tt.pkg, got)
		}
	}
}

func TestAppKey(t *testing.T) {
	for name, want := range map[string]string{
		"OpenRGB": "openrgb", "openrgb-bin": "openrgb", "obs-studio": "obsstudio", "lutris-git": "lutris",
		"heroic-games-launcher": "heroicgames", "com.example.App": "comexampleapp",
	} {
		if got := appKey(name); got != want {
			t.Errorf("appKey(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestShellSplit(t *testing.T) {
	tests := []struct {
		in   string
		want []string
		ok   bool
	}{
		{`env A=1 prog --x "%U"`, []string{"env", "A=1", "prog", "--x", "%U"}, true},
		{`'/opt/My App/run' arg`, []string{"/opt/My App/run", "arg"}, true},
		{`a\ b "c \"d\" \$e"`, []string{"a b", `c "d" \$e`}, true},
		{`a"b"c`, []string{"abc"}, true},
		{`"open`, nil, false},
		{``, nil, true},
	}
	for _, tt := range tests {
		got, ok := shellSplit(tt.in)
		if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("shellSplit(%q) = %q, %v", tt.in, got, ok)
		}
	}
}

func TestAutostartOwner(t *testing.T) {
	m := &fakeMachine{
		Files: map[string]string{
			"/a.desktop": "[Desktop Entry]\nExec=env X=1 chat %U\n",
			"/b.desktop": "Exec=/opt/tool/run\n",
			"/c.desktop": "Name=no exec\n",
			"/d.desktop": "Exec=\"broken\n",
			"/e.desktop": "Exec=unknown\n",
		},
		Which: map[string]string{"chat": "/usr/bin/chat"},
		Owned: map[string]string{"/usr/bin/chat": "chat-app", "/opt/tool/run": "tool"},
	}
	for file, want := range map[string]string{
		"/a.desktop": "chat-app", "/b.desktop": "tool", "/c.desktop": "", "/d.desktop": "", "/e.desktop": "", "/none": "",
	} {
		if got := autostartOwner(context.Background(), m, file); got != want {
			t.Errorf("autostartOwner(%s) = %q, want %q", file, got, want)
		}
	}
}

func TestRankAndSort(t *testing.T) {
	ids := []string{"home-apps", "app-x", "kparam-x", "repos", "firmware-y", "zz", "disk-layout", "packages", "services"}
	sort.SliceStable(ids, func(i, j int) bool { return less(ids[i], ids[j]) })
	want := []string{"repos", "packages", "app-x", "zz", "services", "firmware-y", "kparam-x", "home-apps", "disk-layout"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("order = %v", ids)
	}
	items := []record.Item{
		{Bundle: "services", Type: "svc", ID: "b"}, {Bundle: "account", Type: "group", ID: "audio", User: "bob"},
		{Bundle: "repos", Type: "repo", ID: "x"}, {Bundle: "account", Type: "group", ID: "audio", User: "alice"},
		{Bundle: "services", Type: "mask", ID: "c"},
	}
	SortItems(items)
	var got []string
	for _, it := range items {
		got = append(got, it.Bundle+" "+it.Key()+" "+it.User)
	}
	wantItems := []string{"repos repo:x ", "services mask:c ", "services svc:b ", "account group:audio alice", "account group:audio bob"}
	if !reflect.DeepEqual(got, wantItems) {
		t.Errorf("items = %q", got)
	}
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{"openRGB": "Openrgb", "": "", "ébc": "Ébc", "x": "X"} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q", in, got)
		}
	}
}

func TestBuiltinsAreOrdered(t *testing.T) {
	for id := range Builtin {
		if !slices.Contains(Order, id) {
			t.Errorf("built-in bundle %s isn't in Order", id)
		}
		if err := rules.Check(Builtin[id].When); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	for _, id := range Order {
		if _, ok := Builtin[id]; !ok && !strings.HasPrefix(id, "*") {
			t.Errorf("%s is in Order but not built in", id)
		}
	}
}
