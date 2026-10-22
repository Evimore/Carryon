package restore

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

var treeHeader = regexp.MustCompile(`^-- (.+?) --$`)

// buildTree makes a fake system tree in a temp folder from a description in testdata (see
// testdata/here-desktop.txt for the format). Trees are described in text rather than checked in,
// since they hold symlinks and colons, which Go module zips don't allow.
func buildTree(t *testing.T, desc string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", desc))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var cur string
	var body strings.Builder
	flush := func() {
		if cur == "" {
			return
		}
		content := strings.TrimRight(body.String(), "\n")
		if content != "" {
			content += "\n"
		}
		writeFile(t, filepath.Join(root, cur), content)
		cur = ""
		body.Reset()
	}
	for _, line := range strings.SplitAfter(string(b), "\n") {
		m := treeHeader.FindStringSubmatch(strings.TrimRight(line, "\n"))
		if m == nil {
			if cur != "" {
				body.WriteString(line)
			}
			continue
		}
		flush()
		name := m[1]
		if from, to, ok := strings.Cut(name, " -> "); ok {
			p := filepath.Join(root, from)
			mkdirAll(t, filepath.Dir(p))
			if err := os.Symlink(to, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if strings.HasSuffix(name, "/") {
			mkdirAll(t, filepath.Join(root, name))
			continue
		}
		cur = name
	}
	flush()
	return root
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakePackages is a package manager with fixed lists.
type fakePackages struct {
	installed, sync, repos []string
	err                    error
}

func (f fakePackages) Installed(context.Context) ([]string, error)       { return f.installed, f.err }
func (f fakePackages) SyncPackages(context.Context) ([]string, error)    { return f.sync, nil }
func (f fakePackages) ConfiguredRepos(context.Context) ([]string, error) { return f.repos, nil }

// scenario is a machine to restore onto, with what its programs say.
type scenario struct {
	tree   string
	pkgs   fakePackages
	run    map[string]sysexec.Response
	pick   func([]Tick) Picked // nil: the default ticks
	answer []any               // answers to questions (the user mapping)
}

var scenarios = map[string]scenario{
	// The same desktop, reinstalled: what's already back is left out of the plan.
	"same-machine": {
		tree: "here-desktop.txt",
		pkgs: fakePackages{
			installed: []string{"base", "htop", "openssh", "paru-bin", "systemd"},
			sync:      []string{"base", "fuse3", "htop", "openssh", "systemd"},
			repos:     []string{"core", "extra", "multilib"},
		},
		run: map[string]sysexec.Response{
			"systemctl is-enabled -- sshd.service":                         {Stdout: "enabled\n"},
			"systemctl is-enabled -- systemd-networkd-wait-online.service": {Stdout: "enabled\n"},
			"flatpak list --app --system --columns=application,origin":     {Stdout: "org.example.App\tflathub\n"},
			"flatpak list --app --user --columns=application,origin":       {},
			"cargo install --list":                                         {Stdout: "ripgrep v14.1.0:\n    rg\n"},
		},
	},
	// Another machine for the same user, with everything ticked but one package: machine-specific
	// files are shown for review, and the initramfs config would drop the encrypt hook.
	"different-machine": {
		tree: "here-laptop.txt",
		pkgs: fakePackages{
			installed: []string{"base"},
			sync:      []string{"base", "fuse3", "htop"},
			repos:     []string{"core", "extra"},
		},
		run: map[string]sysexec.Response{
			"systemctl is-enabled -- sshd.service":                         {Stdout: "disabled\n", Code: 1},
			"systemctl is-enabled -- systemd-networkd-wait-online.service": {Stdout: "enabled\n"},
			"npm ls -g --depth=0 --parseable":                              {Stdout: "/usr/lib/node_modules\n/usr/lib/node_modules/typescript\n"},
		},
		pick: func(ticks []Tick) Picked {
			p := Picked{Deselected: []string{"pkg:htop"}}
			for _, t := range ticks {
				p.Bundles = append(p.Bundles, t.Bundle.ID)
			}
			return p
		},
	},
	// bob's machine: alice has no account, so her files go to bob, and paths in them are adapted.
	"other-user": {
		tree: "here-bob.txt",
		pkgs: fakePackages{
			installed: []string{"base", "example-git", "fuse3", "htop", "mypkg", "paru-bin"},
			sync:      []string{"base", "fuse3", "htop", "mypkg"},
			repos:     []string{"core", "extra", "myrepo"},
		},
		run: map[string]sysexec.Response{
			"systemctl is-enabled -- sshd.service":                         {Stdout: "enabled\n"},
			"systemctl is-enabled -- systemd-networkd-wait-online.service": {Stdout: "masked\n", Code: 1},
			"flatpak list --app --system --columns=application,origin":     {},
			"flatpak list --app --user --columns=application,origin":       {Stdout: "org.example.Notes\tflathub\n"},
			"pipx list --short": {Stdout: "black 24.1.0\n"},
		},
		answer: []any{ui.UseDefault},
	},
}

// planFor makes the plan for restoring the record in dir onto the scenario's machine, and returns
// it with the ticks.
func planFor(t *testing.T, dir string, sc scenario) (*Plan, []Tick) {
	t.Helper()
	plan, ticks, _ := planOn(t, dir, sc)
	return plan, ticks
}

// planned is a plan with the machine it was made for.
type planned struct {
	src   *Source
	root  string
	local []LocalUser
	host  string
}

// planOn is planFor, also returning the source and the fake system tree the plan was made on.
func planOn(t *testing.T, dir string, sc scenario) (*Plan, []Tick, planned) {
	t.Helper()
	ctx := context.Background()
	root := buildTree(t, sc.tree)
	src, err := OpenRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	here, err := facts.Collect(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticks, warnings := DefaultTicks(src.Record, here)
	if len(warnings) > 0 {
		t.Errorf("warnings: %v", warnings)
	}
	picked := DefaultPick(ticks)
	if sc.pick != nil {
		picked = sc.pick(ticks)
	}
	local, err := ReadUsers(root)
	if err != nil {
		t.Fatal(err)
	}
	ask := &ui.Script{Answers: sc.answer}
	users, err := MapUsers(src.Record.Meta, local, ask, MapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ask.Answers) > 0 {
		t.Errorf("unused answers: %v", ask.Answers)
	}
	run := &sysexec.Fake{Responses: sc.run}
	pl := &Planner{Source: src, Users: users, Here: &Machine{
		Root: root, Hostname: here.Host, Run: run, Packages: sc.pkgs, Users: local,
	}}
	plan, err := pl.Make(ctx, picked)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range run.Commands() {
		if _, ok := sc.run[c]; !ok {
			t.Errorf("unexpected command: %s", c)
		}
	}
	return plan, ticks, planned{src: src, root: root, local: local, host: here.Host}
}

// render is everything a golden file pins: the ticks, the summary and the full plan.
func render(plan *Plan, ticks []Tick) string {
	var b strings.Builder
	b.WriteString("== ticks\n")
	for _, t := range ticks {
		mark := " "
		if t.On {
			mark = "x"
		}
		fmt.Fprintf(&b, "[%s] %s (%d)", mark, t.Bundle.ID, len(t.Items))
		if t.Reason != "" {
			fmt.Fprintf(&b, "  - %s", t.Reason)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n== summary")
	plan.Summary(&b, "/var/lib/carryon/restore-plan.txt", time.UTC)
	b.WriteString("\n== restore-plan.txt\n")
	b.WriteString(plan.Full(time.UTC))
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		mkdirAll(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/restore -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (go test ./internal/restore -update rewrites it):\n%s",
			path, UnifiedDiff("want", "got", want, []byte(got)))
	}
}

func TestPlanGolden(t *testing.T) {
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			plan, ticks := planFor(t, "testdata/desktop", sc)
			checkGolden(t, name, render(plan, ticks))
		})
	}
}

// The record package's fixture: its files aren't in the fixture, so files that exist here count
// as replaced, with a note instead of a diff.
func TestPlanRecordFixture(t *testing.T) {
	plan, ticks := planFor(t, "../record/testdata/format2", scenarios["different-machine"])
	checkGolden(t, "format2-different-machine", render(plan, ticks))
}

func TestPlanReadsNothingItDoesntNeed(t *testing.T) {
	// Picking nothing asks the package manager, but runs no program and plans nothing.
	sc := scenarios["same-machine"]
	sc.pick = func([]Tick) Picked { return Picked{} }
	sc.run = nil
	plan, _ := planFor(t, "testdata/desktop", sc)
	if plan.Todo() != 0 || plan.Picked != 0 || plan.Done() != 0 {
		t.Errorf("plan = %+v", plan)
	}
}

func TestPlanPackageManagerFails(t *testing.T) {
	src, err := OpenRecord("testdata/desktop")
	if err != nil {
		t.Fatal(err)
	}
	pl := &Planner{Source: src, Here: &Machine{Root: t.TempDir(), Run: &sysexec.Fake{},
		Packages: fakePackages{err: fmt.Errorf("pacman isn't installed")}}}
	if _, err := pl.Make(context.Background(), Picked{Bundles: []string{"packages"}}); err == nil ||
		!strings.Contains(err.Error(), "pacman isn't installed") {
		t.Fatalf("err = %v", err)
	}
}

func TestSelect(t *testing.T) {
	items := []record.Item{
		{Type: "pkg", ID: "a", Bundle: "x"},
		{Type: "group", ID: "wheel", Bundle: "y", User: "alice"},
		{Type: "group", ID: "wheel", Bundle: "y", User: "bob"},
		{Type: "pkg", ID: "b", Bundle: "z"},
	}
	p := Picked{Bundles: []string{"x", "y"}, Deselected: []string{"group:wheel@bob"}}
	got := p.Select(items)
	if len(got) != 2 || got[0].ID != "a" || got[1].User != "alice" {
		t.Errorf("Select = %+v", got)
	}
}
