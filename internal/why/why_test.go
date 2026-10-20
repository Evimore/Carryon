package why

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/state"
)

// machine is a small desktop: alice's home, a few system folders, a network share in /srv and a
// btrfs subvolume in the home.
type machine struct {
	root string
	in   Input
}

func newMachine(t *testing.T) machine {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"etc", "srv/nas", "srv/repo", "root", "opt/x", "opt/other", "var/lib/bluetooth", "usr/bin",
		"home/alice/Documents", "home/alice/.cache/keep", "home/alice/.local/share/Trash", "home/alice/.local/share/Steam",
		"home/alice/.rustup/toolchains/stable", "home/alice/Cloud", "home/alice/vm", "home/alice/build/cache",
		"home/alice/.config/htop", "home/alice/.config/mpv", "home/bob",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for f, text := range map[string]string{
		"etc/hostname":                        "desktop\n",
		"home/alice/Documents/notes.txt":      "x",
		"home/alice/build/cache/CACHEDIR.TAG": "Signature: 8a477f597d28d172789f06886806bc55\n# a cache\n",
	} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	conf, problems, err := rules.ParseBundleRules(strings.NewReader("put games path ~/.local/share/Steam\nwhen games chassis:laptop\n"), "bundles.conf")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	items, bundles := bundle.Assign(context.Background(), bundle.Input{Rules: conf, Host: "desktop", Items: []record.Item{
		{Type: "file", ID: "/etc/hostname", Kind: "unowned"},
		{Type: "pkg", ID: "htop", Repo: "extra"},
		{Type: "pkg", ID: "steam", Repo: "multilib"},
		{Type: "path", ID: "/var/lib/bluetooth"},
		{Type: "path", ID: "/home/alice/Documents", Home: "Documents", User: "alice"},
		{Type: "path", ID: "/home/alice/.config/htop", Home: ".config/htop", User: "alice"},
		{Type: "path", ID: "/home/alice/.config/mpv", Home: ".config/mpv", User: "alice"},
		{Type: "path", ID: "/home/alice/.local/share/Steam", Home: ".local/share/Steam", User: "alice"},
	}})
	for i := range bundles {
		if when, ok := conf.Whens[bundles[i].ID]; ok {
			bundles[i].When = when
		}
	}
	rec := &record.Record{Meta: record.Meta{Format: record.FormatCurrent, Host: "desktop", Chassis: "desktop"}, Items: items, Bundles: bundles}

	cfg := config.Default()
	cfg.Backup.ExtraPaths = []string{"/opt/x"}
	sys := backup.System{Root: root}
	users := []capture.User{{Name: "alice", UID: 1000, Home: "/home/alice"}}
	mounts := dest.MountTable{
		{ID: 1, MountPoint: "/", FSType: "ext4", Source: "/dev/sda1"},
		{ID: 2, MountPoint: "/srv/nas", FSType: "nfs4", Source: "server:/export"},
		{ID: 3, MountPoint: "/home", FSType: "btrfs", Source: "/dev/sda2"},
		{ID: 4, MountPoint: "/home/alice/vm", FSType: "btrfs", Source: "/dev/sda2"},
		{ID: 5, MountPoint: "/home/alice/Cloud", FSType: "fuse.rclone", Source: "drive:"},
	}
	excludes := catalog.ExcludeInputs{
		Defaults:  []string{"/home/*/.cache", "/home/*/.local/share/Trash", "/home/*/.thumbnails"},
		Skip:      []string{"/home/*/.thumbnails"},
		Catalog:   []string{"/home/*/.rustup/toolchains"},
		Automatic: []string{"/home/alice/Cloud", "/srv/repo"},
		User:      []string{"*.iso", "!/home/alice/.local/share/Trash", "!/home/alice/.cache/keep"},
		From: map[string]string{
			"/home/*/.rustup/toolchains":      "the app catalog's entry for Rust (rust.toml)",
			"/home/alice/Cloud":               "a fuse.rclone mount of drive: inside a home folder",
			"/srv/repo":                       "the backup's own folder (repository.url)",
			"*.iso":                           "your excludes in /etc/carryon/excludes.txt",
			"!/home/alice/.local/share/Trash": "your excludes in /etc/carryon/excludes.txt",
			"!/home/alice/.cache/keep":        "alice's own settings in ~/.config/carryon/config.toml",
		},
	}
	rules2 := conf
	rules2.Puts = append(rules2.Puts, rules.Put{Bundle: "media", Match: rules.ItemMatch{Type: "path", Glob: rules.CompileGlob("~/.config/mpv")}, Line: 3})
	return machine{root: root, in: Input{
		Root:     root,
		Targets:  backup.Targets(sys, cfg, items, users),
		Excludes: excludes.List(true),
		Hints:    []catalog.Hint{{Patterns: []string{"/home/*/.rustup/toolchains"}, Label: "Rust toolchains", How: "rustup installs them again"}},
		Crossing: func(target, p string) backup.Crossing {
			return backup.CrossingTo(sys, mounts, target, p)
		},
		Record:     rec,
		Rules:      rules2,
		Here:       &rules.Facts{Host: "desktop", Chassis: "desktop"},
		OtherUsers: map[string]string{"bob": "/home/bob"},
		Ignored:    []string{"/opt/other/junk"},
		Inbox:      []state.Entry{state.NewEntry(state.KindPath, "/var/lib/jellyfin"), state.NewEntry(state.KindRestore, "app-htop")},
	}}
}

func TestExplain(t *testing.T) {
	m := newMachine(t)
	cases := []struct {
		name     string
		path     string
		owner    string
		noRecord bool
		backedUp bool
		check    func(t *testing.T, a Answer)
		text     []string
	}{
		{
			name: "system folder", path: "/etc/hostname", backedUp: true,
			text: []string{"/etc/hostname is backed up.", "It's in /etc, which is always backed up.",
				`bundle identity, "Machine identity: hostname desktop, SSH host keys, Tailscale node", because it's one of the well-known places that go there`,
				"when-rule: samehost, so it starts ticked when restoring here."},
		},
		{
			name: "record folder", path: "/var/lib/bluetooth/aa:bb/info", backedUp: true,
			text: []string{"It's in /var/lib/bluetooth, which the system record backs up.", "bundle bluetooth"},
		},
		{
			name: "extra_paths", path: "/opt/x/data.db", backedUp: true,
			text: []string{"It's in /opt/x, which backup.extra_paths adds to the backup.", "No item in the system record holds it."},
		},
		{
			name: "home folder", path: "/home/alice/Documents/notes.txt", backedUp: true,
			text: []string{"It's in alice's home folder, /home/alice.", "backup ~/Documents (alice)", "bundle home-files"},
		},
		{
			name: "settings folder named like a package", path: "/home/alice/.config/htop/htoprc", backedUp: true,
			text: []string{"bundle app-htop", "because the settings folder is named like the installed package htop"},
		},
		{
			name: "moved by a put rule, unticked here", path: "/home/alice/.local/share/Steam/config.vdf", backedUp: true,
			check: func(t *testing.T, a Answer) {
				if len(a.Items) != 1 || a.Items[0].Reason.Step != bundle.StepPut || a.Items[0].Ticked == nil || *a.Items[0].Ticked {
					t.Errorf("items = %+v", a.Items)
				}
			},
			text: []string{"bundle games", "because line 1 of bundles.conf puts it there (put games path ~/.local/share/Steam)",
				"when-rule: chassis:laptop, so it starts unticked here: laptops only."},
		},
		{
			name: "grouped differently now", path: "/home/alice/.config/mpv/mpv.conf", backedUp: true,
			text: []string{"bundle home-apps", "Grouped again now, it goes to media, because line 3 of bundles.conf puts it there"},
		},
		{
			name: "default exclude", path: "/home/alice/.cache/thumbnails/x.png",
			check: func(t *testing.T, a Answer) {
				if a.Excluded == nil || a.Excluded.Kind != catalog.FromDefaults || a.Excluded.At != "/home/alice/.cache" {
					t.Errorf("excluded = %+v", a.Excluded)
				}
			},
			text: []string{"is not backed up.", "The pattern /home/*/.cache, from the default excludes in /usr/share/carryon/excludes.default, leaves /home/alice/.cache out.",
				"add !/home/alice/.cache to excludes.txt"},
		},
		{
			name: "default exclude taken back by !", path: "/home/alice/.local/share/Trash/files/old.txt", backedUp: true,
			check: func(t *testing.T, a Answer) {
				if a.Excluded != nil || a.TakenBack == nil || !a.TakenBack.Helps || a.TakenBack.Over != "/home/*/.local/share/Trash" {
					t.Errorf("excluded = %+v, taken back = %+v", a.Excluded, a.TakenBack)
				}
			},
			text: []string{"The pattern /home/*/.local/share/Trash, from the default excludes in /usr/share/carryon/excludes.default, would leave it out, " +
				"but !/home/alice/.local/share/Trash, from your excludes in /etc/carryon/excludes.txt, takes it back."},
		},
		{
			name: "! inside a left-out folder", path: "/home/alice/.cache/keep/x",
			check: func(t *testing.T, a Answer) {
				if a.Excluded == nil || a.TakenBack == nil || a.TakenBack.Helps {
					t.Errorf("excluded = %+v, taken back = %+v", a.Excluded, a.TakenBack)
				}
			},
			text: []string{"!/home/alice/.cache/keep, from alice's own settings in ~/.config/carryon/config.toml, matches it, but it can't bring back something inside a folder that is left out."},
		},
		{
			name: "switched-off default", path: "/home/alice/.thumbnails/x.png", backedUp: true,
		},
		{
			name: "catalog app, downloads again", path: "/home/alice/.rustup/toolchains/stable/bin/cargo",
			check: func(t *testing.T, a Answer) {
				if a.Redownload == nil || a.Excluded.Kind != catalog.FromCatalog {
					t.Errorf("answer = %+v", a)
				}
			},
			text: []string{"from the app catalog's entry for Rust (rust.toml)", "Left out on purpose: Rust toolchains. Rustup installs them again."},
		},
		{
			name: "excludes.txt", path: "/home/alice/Documents/disk.iso",
			text: []string{"The pattern *.iso, from your excludes in /etc/carryon/excludes.txt, leaves it out.",
				"take the pattern out of your excludes in /etc/carryon/excludes.txt"},
		},
		{
			name: "mount in a home folder", path: "/home/alice/Cloud/report.pdf",
			text: []string{"from a fuse.rclone mount of drive: inside a home folder", "Mounts inside home folders"},
		},
		{
			name: "the repository's own folder", path: "/srv/repo/config",
			text: []string{"from the backup's own folder (repository.url)", "never copies the backup into itself"},
		},
		{
			name: "another filesystem", path: "/srv/nas/film.mkv",
			check: func(t *testing.T, a Answer) {
				if a.LeftOut == nil || a.LeftOut.Path != "/srv/nas" {
					t.Errorf("left out = %+v", a.LeftOut)
				}
			},
			text: []string{"/srv/nas is a separate nfs4 filesystem (server:/export) and isn't backed up.\n", "To include it: sudo carryon add /srv/nas"},
		},
		{
			name: "nested subvolume", path: "/home/alice/vm/disk.img", backedUp: true,
			text: []string{"It's on the btrfs subvolume /home/alice/vm, which is backed up as a folder of its own."},
		},
		{
			name: "cache folder", path: "/home/alice/build/cache/obj.o",
			text: []string{"/home/alice/build/cache holds a CACHEDIR.TAG file"},
		},
		{
			name: "package file outside the backup", path: "/usr/bin/htop", owner: "htop",
			check: func(t *testing.T, a Answer) {
				if a.Package == nil || !a.Package.InRecord || a.Package.Bundle != "app-htop" {
					t.Errorf("package = %+v", a.Package)
				}
			},
			text: []string{"isn't in the backup itself, but it comes back with its package.",
				"It belongs to the package htop, which the record lists, so reinstalling the package brings it back.",
				"package htop [extra]", "the settings folder ~/.config/htop is named like it"},
		},
		{
			name: "unchanged package file in /etc", path: "/etc/htoprc", owner: "htop", backedUp: true,
			text: []string{"the record has no changes to it, so a restore leaves it to the package."},
		},
		{
			name: "not in any backed-up folder", path: "/opt/other/thing",
			text: []string{"It isn't in any backed-up folder.", "To back it up: sudo carryon add /opt/other/thing"},
		},
		{
			name: "ignored", path: "/opt/other/junk/x",
			check: func(t *testing.T, a Answer) {
				if a.Ignored != "/opt/other/junk" || a.Waiting != nil {
					t.Errorf("answer = %+v", a)
				}
			},
			text: []string{"detect.ignore has /opt/other/junk, so Carryon doesn't suggest backing it up.",
				"To back it up anyway: sudo carryon add /opt/other/junk/x"},
		},
		{
			name: "waiting in the inbox", path: "/var/lib/jellyfin/data/library.db",
			check: func(t *testing.T, a Answer) {
				if a.Waiting == nil || a.Waiting.ID != state.EntryID(state.KindPath, "/var/lib/jellyfin") {
					t.Errorf("waiting = %+v", a.Waiting)
				}
			},
			text: []string{"Carryon suggests backing up /var/lib/jellyfin: it's waiting in carryon inbox as path-",
				"To back it up: sudo carryon add path-"},
		},
		{
			name: "a user left out", path: "/home/bob/notes.txt",
			text: []string{"It's in bob's home folder, but backup.users leaves bob out."},
		},
		{
			name: "without root", path: "/home/alice/Documents/notes.txt", noRecord: true, backedUp: true,
			check: func(t *testing.T, a Answer) {
				if a.RecordRead || len(a.Items) != 0 {
					t.Errorf("answer = %+v", a)
				}
			},
			text: []string{"Without root", "sudo carryon why /home/alice/Documents/notes.txt"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := m.in
			in.Path, in.Owner = c.path, c.owner
			if c.noRecord {
				in.Record = nil
			}
			a := Explain(context.Background(), in)
			if a.BackedUp != c.backedUp {
				t.Errorf("backed up = %v, want %v", a.BackedUp, c.backedUp)
			}
			if c.check != nil {
				c.check(t, a)
			}
			var buf bytes.Buffer
			if err := WriteText(&buf, a); err != nil {
				t.Fatal(err)
			}
			for _, want := range c.text {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("text doesn't say %q:\n%s", want, buf.String())
				}
			}
			if _, err := json.Marshal(a); err != nil {
				t.Error(err)
			}
		})
	}
}
