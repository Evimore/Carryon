package restore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
)

// The plan lists what was installed after the backup only when the whole backup is restored onto
// the machine it's of.
func TestPlanInstalledSince(t *testing.T) {
	recordOnly := scenarios["same-machine"]
	recordOnly.pkgs.chosen = []pkgmgr.Package{{Name: "base"}, {Name: "htop"}, {Name: "paru-bin", Foreign: true}}
	oneBundle := scenarios["same-machine"]
	oneBundle.oneBundle = true
	tests := []struct {
		name string
		sc   scenario
		want *InstalledSince
	}{
		{"same machine", scenarios["same-machine"], &InstalledSince{
			Packages: []string{"steam", "wine"}, Remove: []string{"pacman", "-Rns", "steam", "wine"}, Snapper: true}},
		{"another machine", scenarios["different-machine"], nil},
		{"another user's machine", scenarios["other-user"], nil},
		{"one bundle on its own", oneBundle, nil},
		{"nothing installed since", recordOnly, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, _ := planFor(t, "testdata/desktop", tt.sc)
			if !reflect.DeepEqual(plan.Since, tt.want) {
				t.Errorf("Since = %+v, want %+v", plan.Since, tt.want)
			}
			var sum strings.Builder
			plan.Summary(&sum, "", time.UTC)
			full := plan.Full(time.UTC)
			listed := strings.Contains(sum.String(), sinceHeading) || strings.Contains(full, sinceHeading)
			if listed != (tt.want != nil) {
				t.Errorf("section shown = %v, want %v:\n%s\n%s", listed, tt.want != nil, sum.String(), full)
			}
			if tt.want == nil {
				return
			}
			for _, want := range []string{
				"  Installed since this backup (left as they are): steam, wine\n",
				"  Restoring never removes anything. To remove these by hand: sudo pacman -Rns steam wine\n",
				"  To undo a whole change, roll back to before it with snapper or btrfs-assistant.\n",
			} {
				if !strings.Contains(sum.String(), want) {
					t.Errorf("summary lacks %q:\n%s", want, sum.String())
				}
			}
			if !strings.Contains(full, "\nInstalled since this backup (left as they are):\n  steam\n  wine\n") ||
				!strings.Contains(full, "To remove these by hand:\n  sudo pacman -Rns steam wine\n") {
				t.Errorf("plan file:\n%s", full)
			}
			if plan.Todo() == 0 {
				t.Error("the scenario should still have steps")
			}
		})
	}
}

func TestInstalledSince(t *testing.T) {
	desktop := record.Meta{Host: "desktop", Machine: "268b81f2a8e0a139"}
	items := []record.Item{
		{Type: record.TypePkg, ID: "base", Bundle: "packages"},
		{Type: record.TypePkg, ID: "fuse3", Bundle: "packages", AsDeps: true},
		{Type: record.TypeAUR, ID: "paru-bin", Bundle: "aur"},
		{Type: record.TypeSvc, ID: "steam.service", Bundle: "services"},
	}
	chosen := []pkgmgr.Package{
		{Name: "base", Repo: "core"}, {Name: "fuse3", Repo: "extra"}, {Name: "steam", Repo: "multilib"},
		{Name: "paru-bin", Foreign: true}, {Name: "my-tool-git", Foreign: true},
		{Name: "gamemode", Repo: "extra", AsDeps: true},
	}
	newOnes := []string{"my-tool-git", "steam"}
	tests := []struct {
		name      string
		meta      record.Meta
		items     []record.Item
		here      rules.Facts
		snapper   bool // /etc/snapper/configs/root is there
		oneBundle bool
		want      []string // packages listed; nil: no section
		hint      bool     // the snapper hint
	}{
		{"same hardware", desktop, items, rules.Facts{Host: "desktop", Machine: "268b81f2a8e0a139", RootFS: "btrfs"},
			true, false, newOnes, true},
		{"renamed", desktop, items, rules.Facts{Host: "workstation", Machine: "268b81f2a8e0a139", RootFS: "btrfs"},
			false, false, newOnes, false},
		{"same name, hardware unknown", record.Meta{Host: "desktop"}, items, rules.Facts{Host: "desktop", RootFS: "ext4"},
			true, false, newOnes, false},
		{"other hardware with the same name", desktop, items, rules.Facts{Host: "desktop", Machine: "0123456789abcdef", RootFS: "btrfs"},
			true, false, nil, false},
		{"another machine", desktop, items, rules.Facts{Host: "laptop"}, false, false, nil, false},
		{"nothing known about either", record.Meta{}, items, rules.Facts{}, false, false, nil, false},
		{"one bundle on its own", desktop, items, rules.Facts{Host: "desktop", Machine: "268b81f2a8e0a139"},
			false, true, nil, false},
		{"record without packages", desktop, items[3:], rules.Facts{Host: "desktop", Machine: "268b81f2a8e0a139"},
			false, false, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.snapper {
				writeFile(t, filepath.Join(root, "etc/snapper/configs/root"), "SUBVOLUME=\"/\"\n")
			}
			pl := &Planner{Everything: !tt.oneBundle,
				Source: &Source{Record: &record.Record{Meta: tt.meta, Items: tt.items}},
				Here:   &Machine{Root: root, Facts: tt.here, Packages: fakePackages{chosen: chosen}}}
			got, err := pl.installedSince(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == nil {
				if got != nil {
					t.Errorf("installedSince = %+v, want none", got)
				}
				return
			}
			want := &InstalledSince{Packages: tt.want, Remove: append([]string{"pacman", "-Rns"}, tt.want...), Snapper: tt.hint}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("installedSince = %+v, want %+v", got, want)
			}
		})
	}
}

func TestInstalledSinceFails(t *testing.T) {
	meta := record.Meta{Host: "desktop"}
	pl := &Planner{Everything: true,
		Source: &Source{Record: &record.Record{Meta: meta, Items: []record.Item{{Type: record.TypePkg, ID: "base"}}}},
		Here: &Machine{Root: t.TempDir(), Facts: rules.Facts{Host: "desktop"},
			Packages: fakePackages{chosenErr: errors.New("running pacman -Qqen: exit status 2")}}}
	got, err := pl.installedSince(context.Background())
	if err == nil || got != nil || !strings.Contains(err.Error(), "listing what was installed since the backup: running pacman -Qqen") {
		t.Errorf("installedSince = %+v, %v; want the error", got, err)
	}
}

func TestInstalledSinceShown(t *testing.T) {
	// Without the snapper hint or a command to give, the section still says nothing is removed.
	s := &InstalledSince{Packages: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}
	var sum strings.Builder
	s.summary(&sum)
	want := "\n  Installed since this backup (left as they are): a, b, c, d, e, f, g, h ... (+1 more)\n" +
		"  Restoring never removes anything.\n"
	if sum.String() != want {
		t.Errorf("summary:\n%q\nwant\n%q", sum.String(), want)
	}
	var full strings.Builder
	s.full(&full)
	if !strings.Contains(full.String(), "  h\n  i\nRestoring never removes anything.\n") || strings.Contains(full.String(), "snapper") {
		t.Errorf("plan file:\n%s", full.String())
	}
}
