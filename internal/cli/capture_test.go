package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
)

func TestCaptureNeedsOut(t *testing.T) {
	_, err := run(t, paths.Dirs{State: "/var/lib/carryon"}, "capture")
	if err == nil || !strings.Contains(err.Error(), "--out DIR") || !strings.Contains(err.Error(), "T11") {
		t.Fatalf("err = %v", err)
	}
}

func TestCaptureRefusesAFullFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep-me"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, paths.Dirs{}, "capture", "--out", dir)
	if err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep-me")); string(b) != "x" {
		t.Error("the folder's contents were touched")
	}
}

func TestCaptureSummary(t *testing.T) {
	res := &capture.Result{
		Dir:   "/tmp/record",
		Users: []capture.User{{Name: "alice"}, {Name: "bob"}},
		Items: []record.Item{
			{Type: record.TypePkg, ID: "base"}, {Type: record.TypePkg, ID: "htop"},
			{Type: record.TypePath, ID: "/home/alice/.zshrc"},
		},
		Skipped:  []string{"/etc/sudoers.d/10-wheel"},
		Warnings: []string{"not running as root"},
	}
	var out bytes.Buffer
	printCaptureSummary(&out, res)
	want := `System record written to /tmp/record (3 items, users: alice, bob)
  path     1
  pkg      2
1 path couldn't be read and was skipped (listed in /tmp/record/skipped.txt), so the record is incomplete.
1 warning (shown above).
`
	if out.String() != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out.String(), want)
	}
}
