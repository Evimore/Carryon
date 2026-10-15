package ui

import (
	"bytes"
	"testing"
)

func TestPlainOutput(t *testing.T) {
	var b bytes.Buffer
	l := NewLogger(&b, false, false)
	l.Info("backing up to %s", "/mnt/backup")
	l.Warn("two\nlines")
	want := ":: backing up to /mnt/backup\n!! two\nlines\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestJournalPrefixesEveryLine(t *testing.T) {
	var b bytes.Buffer
	l := NewLogger(&b, true, false)
	l.Error("backup failed\n   the destination isn't mounted")
	want := "<3>xx backup failed\n<3>   the destination isn't mounted\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestColourWrapsOnlyTheMarker(t *testing.T) {
	var b bytes.Buffer
	NewLogger(&b, false, true).Info("done")
	want := "\033[1;34m::\033[0m done\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestJournalStreamMatches(t *testing.T) {
	cases := []struct {
		env      string
		dev, ino uint64
		want     bool
	}{
		{"", 1, 2, false},
		{"8:1234", 8, 1234, true},
		{"8:1234", 8, 1235, false},
		{"garbage", 8, 1234, false},
	}
	for _, c := range cases {
		if got := journalStreamMatches(c.env, c.dev, c.ino); got != c.want {
			t.Errorf("journalStreamMatches(%q, %d, %d) = %v, want %v", c.env, c.dev, c.ino, got, c.want)
		}
	}
}
