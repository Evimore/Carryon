package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every command a user sees in --help has a section in the man page, and hidden ones don't.
func TestManCoversEveryCommand(t *testing.T) {
	root := newRoot()
	var b bytes.Buffer
	if err := writeMan(&b, root, "1.2.3", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	if !strings.HasPrefix(page, `.TH CARRYON 1 "2026-01-02" "carryon 1.2.3"`) {
		t.Errorf("header: %q", strings.SplitN(page, "\n", 2)[0])
	}
	for _, c := range root.Commands() {
		section := ".SS carryon " + text(c.Name())
		if c.IsAvailableCommand() && !strings.Contains(page, section) {
			t.Errorf("no section for carryon %s", c.Name())
		}
		if c.Hidden && strings.Contains(page, section+" ") {
			t.Errorf("hidden command carryon %s is in the man page", c.Name())
		}
	}
	for _, want := range []string{".SS carryon config set", `\fB\-\-deep\fR`, "/var/lib/carryon/record"} {
		if !strings.Contains(page, want) {
			t.Errorf("man page lacks %q", want)
		}
	}
	for i, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "'") {
			t.Errorf("line %d starts with a quote, which roff reads as a request: %q", i+1, line)
		}
	}
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"--deep":       `\-\-deep`,
		`a\b`:          `a\eb`,
		".hidden file": `\&.hidden file`,
		"'quoted'":     `\&'quoted'`,
		"plain":        "plain",
	} {
		if got := text(in); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunWritesEveryFile(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1767312000")
	dir := t.TempDir()
	if err := run(dir, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"man/carryon.1", "completions/carryon.bash", "completions/_carryon", "completions/carryon.fish"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("carryon")) {
			t.Errorf("%s doesn't mention carryon", f)
		}
	}
	page, _ := os.ReadFile(filepath.Join(dir, "man/carryon.1"))
	if !bytes.Contains(page, []byte(`"2026-01-02"`)) {
		t.Errorf("man page date ignores SOURCE_DATE_EPOCH: %s", bytes.SplitN(page, []byte("\n"), 2)[0])
	}
}
