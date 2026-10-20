package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/why"
)

func TestWhy(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	writeFile(t, dirs.DefaultExcludes(), "/home/*/.cache\n")
	writeFile(t, dirs.BundlesFile(), "put notes path ~/a.txt\ntitle notes My notes\n")
	rec := &record.Record{
		Meta:    record.Meta{Format: record.FormatCurrent, Host: "desktop"},
		Items:   []record.Item{{Type: record.TypePath, ID: "/home/alice/a.txt", Home: "a.txt", User: "alice", Bundle: "notes"}},
		Bundles: record.Bundles{{ID: "notes", Title: "My notes", When: "always"}},
	}
	if err := os.MkdirAll(dirs.RecordDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(dirs.RecordDir()); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, dirs, "why", "/home/alice/a.txt")
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{
		"/home/alice/a.txt is backed up.", "It's in alice's home folder, /home/alice.",
		`bundle notes, "My notes", because line 1 of bundles.conf puts it there (put notes path ~/a.txt).`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output doesn't say %q:\n%s", want, out)
		}
	}

	out, err = run(t, dirs, "why", "--json", "/home/alice/.cache/x")
	if err != nil {
		t.Fatal(err, out)
	}
	var a why.Answer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err, out)
	}
	if a.BackedUp || a.Excluded == nil || a.Excluded.Pattern != "/home/*/.cache" || !a.RecordRead {
		t.Errorf("answer = %s", out)
	}

	out, err = run(t, dirs, "why", "--json", "/home/alice/a.txt")
	if err != nil {
		t.Fatal(err, out)
	}
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err, out)
	}
	if len(a.Items) != 1 || a.Items[0].Reason.Step != bundle.StepPut {
		t.Errorf("answer = %s", out)
	}
}

func TestWhyWithoutRoot(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	asRoot(t, false) // and there is no record it could read
	out, err := run(t, dirs, "why", "/home/alice/a.txt")
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{"/home/alice/a.txt is backed up.", "Without root", "sudo carryon why /home/alice/a.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("output doesn't say %q:\n%s", want, out)
		}
	}
}
