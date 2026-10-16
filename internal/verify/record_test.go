package verify

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
)

func TestRecordChecks(t *testing.T) {
	tests := []struct {
		name   string
		change func(env *testEnv)
		level  Level
		text   string
	}{
		{"complete", nil, OK, "The record is complete"},
		{"fresh", nil, OK, "Last captured"},
		{"matches the machine", nil, OK, "matches the running system"},
		{"incomplete", func(env *testEnv) {
			rec, _ := record.Load(env.dirs.RecordDir())
			rec.Meta.Complete = false
			if err := rec.Save(env.dirs.RecordDir()); err != nil {
				t.Fatal(err)
			}
		}, Problem, "The record is incomplete"},
		{"stale", func(env *testEnv) { env.setCaptured(env.now.Add(-72 * time.Hour)) }, Problem, "Last captured"},
		{"no capture time", func(env *testEnv) {
			os.Remove(env.dirs.RecordDir() + "/" + record.LastCaptureFile)
		}, Problem, "doesn't say when it was captured"},
		{"restore in progress", func(env *testEnv) {
			write(t, env.dirs.RestoringFlag(), "2026-09-30T10:00:00Z\n")
		}, Problem, "a restore started 2026-09-30T10:00:00Z never finished"},
		{"drift", func(env *testEnv) {
			env.pm.pkgs = append(env.pm.pkgs, pkgmgr.Package{Name: "ripgrep", Repo: "extra"})
		}, Warning, "1 change since the last capture"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEnv(t)
			env.capture()
			if tt.change != nil {
				tt.change(env)
			}
			v := env.verifier(env.options())
			if !v.checkRecord(context.Background()) {
				t.Fatalf("checkRecord stopped:\n%s", levels(v.rep))
			}
			l := want(t, v.rep, tt.level, tt.text)
			if tt.name == "drift" && !strings.Contains(strings.Join(l.Details, "\n"), "+ package    ripgrep") {
				t.Errorf("details = %q", l.Details)
			}
		})
	}
}

func TestNoRecordStops(t *testing.T) {
	env := newEnv(t)
	v := env.verifier(env.options())
	if v.checkRecord(context.Background()) {
		t.Fatal("went on without a record")
	}
	want(t, v.rep, Problem, "There is no system record")
}

func TestDriftIgnoresEditedFiles(t *testing.T) {
	env := newEnv(t)
	env.capture()
	// The nightly capture found an edited package file; the quick capture verify makes doesn't
	// look for those, so it isn't drift.
	rec, _ := record.Load(env.dirs.RecordDir())
	rec.Items = append(rec.Items, record.Item{Type: record.TypeFile, ID: "/usr/lib/example.so", Kind: record.KindEdited, Pkg: "example"})
	if err := rec.Save(env.dirs.RecordDir()); err != nil {
		t.Fatal(err)
	}
	v := env.verifier(env.options())
	v.checkRecord(context.Background())
	want(t, v.rep, OK, "matches the running system")
}
