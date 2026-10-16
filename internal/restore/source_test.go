package restore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/ui"
)

// fakeRepo lists fixed snapshots and records the restic commands it's asked to run.
type fakeRepo struct {
	snaps []engine.Snapshot
	ran   [][]string
}

func (f *fakeRepo) Snapshots(_ context.Context, host string) ([]engine.Snapshot, error) {
	if host != "" {
		return nil, errors.New("only all snapshots are listed")
	}
	return f.snaps, nil
}

func (f *fakeRepo) Run(_ context.Context, args ...string) (engine.Result, error) {
	f.ran = append(f.ran, args)
	return engine.Result{Args: args, Code: 0}, nil
}

func snap(id, host, at string) engine.Snapshot {
	t, _ := time.Parse(time.RFC3339, at)
	return engine.Snapshot{ID: id, ShortID: id[:8], Hostname: host, Time: t, Tags: []string{"carryon"}}
}

var twoMachines = []engine.Snapshot{
	snap("aaaaaaaa11111111", "desktop", "2026-10-01T03:00:00Z"),
	snap("bbbbbbbb22222222", "laptop", "2026-10-02T03:00:00Z"),
	snap("cccccccc33333333", "desktop", "2026-10-03T03:00:00Z"),
}

func TestChooseSource(t *testing.T) {
	tests := []struct {
		name    string
		snaps   []engine.Snapshot
		opt     FetchOptions
		answers []any
		asked   []string // menu titles
		want    string   // snapshot ID restored from
		wantErr string
	}{
		{name: "one machine, latest", snaps: twoMachines[:1], want: "aaaaaaaa11111111"},
		{name: "from, latest", snaps: twoMachines, opt: FetchOptions{From: "desktop"}, want: "cccccccc33333333"},
		{name: "from, by ID", snaps: twoMachines, opt: FetchOptions{From: "desktop", Snapshot: "aaaa"}, want: "aaaaaaaa11111111"},
		{name: "ID of another machine", snaps: twoMachines, opt: FetchOptions{From: "desktop", Snapshot: "bbbb"},
			wantErr: "no snapshot bbbb of desktop"},
		{name: "unknown machine", snaps: twoMachines, opt: FetchOptions{From: "server"},
			wantErr: "the backup has no snapshots of server; machines in it: desktop, laptop"},
		{name: "ask for the machine", snaps: twoMachines, answers: []any{"laptop"},
			asked: []string{"Restore from which machine?"}, want: "bbbbbbbb22222222"},
		{name: "this machine is the default", snaps: twoMachines, opt: FetchOptions{Here: "desktop"},
			answers: []any{ui.UseDefault}, asked: []string{"Restore from which machine?"}, want: "cccccccc33333333"},
		{name: "no terminal, several machines", snaps: twoMachines, answers: []any{ui.UseDefault},
			asked: []string{"Restore from which machine?"}, wantErr: "the backup holds several machines (desktop, laptop): choose one with --from"},
		{name: "pick a snapshot", snaps: twoMachines, opt: FetchOptions{From: "desktop", Snapshot: "pick"},
			answers: []any{"aaaaaaaa11111111"}, asked: []string{"Which backup of desktop?"}, want: "aaaaaaaa11111111"},
		{name: "pick takes the newest by default", snaps: twoMachines, opt: FetchOptions{From: "desktop", Snapshot: "pick"},
			answers: []any{ui.UseDefault}, asked: []string{"Which backup of desktop?"}, want: "cccccccc33333333"},
		{name: "empty repository", wantErr: "the backup has no snapshots yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{snaps: tt.snaps}
			ask := &ui.Script{Answers: tt.answers}
			tt.opt.TempDir = t.TempDir()
			tt.opt.Location = time.UTC
			_, err := Fetch(context.Background(), repo, ask, tt.opt)
			var titles []string
			for _, a := range ask.Asked {
				titles = append(titles, a.Title)
			}
			if strings.Join(titles, "|") != strings.Join(tt.asked, "|") {
				t.Errorf("asked %q, want %q", titles, tt.asked)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			// The fake restores nothing, so the fetch fails after choosing; the restic command
			// shows which snapshot was chosen.
			if err == nil || !strings.Contains(err.Error(), "has no system record") {
				t.Fatalf("err = %v", err)
			}
			if len(repo.ran) != 1 || repo.ran[0][1] != tt.want {
				t.Fatalf("ran %q, want a restore of %s", repo.ran, tt.want)
			}
			want := []string{"restore", tt.want, "--target", "", "--include", RecordPath, "--include", PrototypeRecordPath}
			got := append([]string(nil), repo.ran[0]...)
			got[3] = ""
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("ran %q, want %q", got, want)
			}
			if entries, _ := os.ReadDir(tt.opt.TempDir); len(entries) != 0 {
				t.Errorf("the temp folder wasn't removed: %v", entries)
			}
		})
	}
}

func TestMachineMenuNotes(t *testing.T) {
	ask := &recordingPrompter{answer: "desktop"}
	_, err := chooseMachine(twoMachines, ask, FetchOptions{Here: "laptop"}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	m := ask.menu
	if m.Default != "laptop" || m.Flag != "--from" || len(m.Options) != 2 ||
		m.Options[0].Note != "newest backup 2026-10-03 03:00" || m.Options[1].Note != "newest backup 2026-10-02 03:00" {
		t.Errorf("menu = %+v", m)
	}
}

// recordingPrompter keeps the menu it was shown.
type recordingPrompter struct {
	ui.Defaults
	answer string
	menu   ui.Menu
}

func (r *recordingPrompter) Menu(m ui.Menu) (string, error) {
	r.menu = m
	return r.answer, nil
}

func TestOpenRecord(t *testing.T) {
	src, err := OpenRecord("testdata/desktop")
	if err != nil {
		t.Fatal(err)
	}
	if src.Machine() != "desktop" || src.Snapshot != nil || len(src.Record.Items) == 0 {
		t.Errorf("source = %+v", src)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("testdata/desktop"); err != nil {
		t.Fatal("Close removed a record on disk")
	}
	if _, err := OpenRecord(t.TempDir()); err == nil || !strings.Contains(err.Error(), "can't read the system record") {
		t.Errorf("err = %v", err)
	}
}

// resticRepo makes a local restic repository in a temp folder and backs up the given trees, each
// as one snapshot of its host, with paths stored as they would be on that machine.
func resticRepo(t *testing.T, backups ...func(root string) (host string)) *engine.Engine {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed")
	}
	tmp := t.TempDir()
	pw := filepath.Join(tmp, "password")
	if err := os.WriteFile(pw, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := engine.New(config.Repository{URL: filepath.Join(tmp, "repo"), PasswordFile: pw},
		paths.Dirs{Conf: filepath.Join(tmp, "conf"), Cache: filepath.Join(tmp, "cache")}, "desktop")
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home")}
	e.Stderr = &strings.Builder{}
	r, err := e.Run(context.Background(), "init")
	if err != nil || r.Err() != nil {
		t.Fatalf("restic init: %v %v\n%s", err, r.Err(), r.Stderr)
	}
	for i, backup := range backups {
		root := filepath.Join(tmp, "src", string(rune('a'+i)))
		mkdirAll(t, root)
		host := backup(root)
		args, err := e.Args("backup", "--host", host, "--tag", "carryon", "var")
		if err != nil {
			t.Fatal(err)
		}
		// Relative paths are stored from the root of the snapshot, as /var/... on a real machine.
		cmd := exec.Command("restic", args...)
		cmd.Dir, cmd.Env = root, e.Env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic backup: %v\n%s", err, out)
		}
	}
	return e
}

// copyDir copies a record folder.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFetchFromRestic(t *testing.T) {
	e := resticRepo(t,
		// A machine backed up by the prototype: its record is at /var/lib/sysbackup/manifest.
		func(root string) string {
			copyDir(t, "testdata/desktop", filepath.Join(root, PrototypeRecordPath))
			writeFile(t, filepath.Join(root, "var/lib/other/data"), "not part of the record\n")
			return "desktop"
		},
		// One backed up by Carryon.
		func(root string) string {
			copyDir(t, "../record/testdata/format1", filepath.Join(root, RecordPath))
			writeFile(t, filepath.Join(root, "var/lib/other/data"), "not part of the record\n")
			return "laptop"
		},
		// Something else in the same repository.
		func(root string) string {
			writeFile(t, filepath.Join(root, "var/lib/other/data"), "no record here\n")
			return "server"
		},
	)
	ctx := context.Background()
	tmp := t.TempDir()
	for _, tt := range []struct{ from, wantFile string }{
		{"desktop", "files/etc/fstab"},
		{"laptop", "items.jsonl"},
	} {
		src, err := Fetch(ctx, e, ui.Defaults{}, FetchOptions{From: tt.from, TempDir: tmp})
		if err != nil {
			t.Fatalf("%s: %v", tt.from, err)
		}
		if src.Snapshot == nil || src.Snapshot.Hostname != tt.from || src.Machine() != "desktop" {
			t.Errorf("%s: source = %+v", tt.from, src)
		}
		if _, err := os.Stat(filepath.Join(src.Dir, tt.wantFile)); err != nil {
			t.Errorf("%s: %v", tt.from, err)
		}
		// Only the record was fetched.
		if _, err := os.Stat(filepath.Join(src.tmp, "var/lib/other")); err == nil {
			t.Errorf("%s: more than the record was fetched", tt.from)
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp folders left behind: %v", entries)
	}

	_, err := Fetch(ctx, e, ui.Defaults{}, FetchOptions{From: "server", TempDir: tmp})
	if err == nil || !strings.Contains(err.Error(), "has no system record") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp folders left behind: %v", entries)
	}
}
