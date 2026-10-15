package engine

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseSnapshots(t *testing.T) {
	snaps, err := ParseSnapshots(readTestdata(t, "snapshots.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	// oldest first, whatever order restic printed them in
	old, recent := snaps[0], snaps[1]
	if old.Hostname != "laptop" || old.ShortID != "aaaaaaaa" || len(old.Tags) != 0 {
		t.Errorf("oldest = %+v (short_id should be filled in when restic leaves it out)", old)
	}
	wantTime := time.Date(2026, 10, 2, 1, 0, 12, 123456789, time.UTC)
	if recent.Hostname != "desktop" || !recent.Time.Equal(wantTime) || recent.Tags[0] != "carryon" ||
		strings.Join(recent.Paths, " ") != "/etc /home/alice" || recent.ID[:8] != recent.ShortID {
		t.Errorf("newest = %+v", recent)
	}

	for _, empty := range []string{"", "[]", "\n"} {
		if s, err := ParseSnapshots([]byte(empty)); err != nil || len(s) != 0 {
			t.Errorf("ParseSnapshots(%q) = %v, %v", empty, s, err)
		}
	}
	if _, err := ParseSnapshots([]byte("Fatal: oops")); err == nil {
		t.Error("garbage parsed without error")
	}
}

func TestParseLs(t *testing.T) {
	var nodes []Node
	err := ParseLs(strings.NewReader(string(readTestdata(t, "ls-0.17.jsonl"))), func(n Node) error {
		nodes = append(nodes, n)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (the snapshot line is skipped): %+v", len(nodes), nodes)
	}
	if n := nodes[1]; n.Path != "/etc/hostname" || n.Type != "file" || n.Size != 8 || n.Mode != 0o644 || n.MTime.IsZero() {
		t.Errorf("file node = %+v", n)
	}
	if nodes[0].Type != "dir" || nodes[2].Type != "symlink" {
		t.Errorf("types = %s, %s", nodes[0].Type, nodes[2].Type)
	}

	stop := errors.New("stop")
	calls := 0
	err = ParseLs(strings.NewReader(string(readTestdata(t, "ls-0.17.jsonl"))), func(Node) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("callback error: err %v after %d calls", err, calls)
	}
	if err := ParseLs(strings.NewReader("{\"struct_type\":\"node\"}\nnot json\n"), func(Node) error { return nil }); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("bad line: err = %v", err)
	}
}

func TestParseBackup(t *testing.T) {
	out, err := ParseBackup(strings.NewReader("restic noise that isn't JSON\n" + string(readTestdata(t, "backup.jsonl"))))
	if err != nil {
		t.Fatal(err)
	}
	s := out.Summary
	if s == nil {
		t.Fatal("no summary")
	}
	if s.FilesNew != 2 || s.DirsNew != 2 || s.DataAdded != 1399 || s.TotalBytesProcessed != 5 ||
		s.SnapshotID[:8] != "d5c6d96a" || s.BackupEnd.Before(s.BackupStart) || s.TotalDuration < 0.7 {
		t.Errorf("summary = %+v", s)
	}
	if len(out.Errors) != 1 || out.Errors[0].Item != "/home/alice/.cache/locked" || !strings.Contains(out.Errors[0].Message, "permission denied") {
		t.Errorf("errors = %+v", out.Errors)
	}

	out, err = ParseBackup(strings.NewReader(`{"message_type":"status","percent_done":0.5}` + "\n"))
	if err != nil || out.Summary != nil {
		t.Errorf("unfinished backup: %+v, %v", out, err)
	}
}

func TestParseRepoConfig(t *testing.T) {
	c, err := ParseRepoConfig([]byte(`{
  "version": 2,
  "id": "b34ee502e41e89dfc0977b35b55594500eda40a87c8f14b89f42edf1a6bea5eb",
  "chunker_polynomial": "3516bba540f015"
}`))
	if err != nil || c.Version != 2 || c.ID != "b34ee502e41e89dfc0977b35b55594500eda40a87c8f14b89f42edf1a6bea5eb" {
		t.Errorf("got %+v, %v", c, err)
	}
	if _, err := ParseRepoConfig([]byte(`{"version": 2}`)); err == nil {
		t.Error("a config without id parsed")
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		out     string
		want    [3]int
		wantErr bool
	}{
		{out: "restic 0.19.1 compiled with go1.26.4 on linux/amd64\n", want: [3]int{0, 19, 1}},
		{out: "restic 0.17.0-dev (compiled manually) compiled with go1.22.5 on linux/amd64", want: [3]int{0, 17, 0}},
		{out: "restic 1.2.3", want: [3]int{1, 2, 3}},
		{out: "rustic 0.9.0", wantErr: true},
	}
	for _, tt := range tests {
		a, b, c, err := ParseVersion(tt.out)
		if (err != nil) != tt.wantErr || (!tt.wantErr && [3]int{a, b, c} != tt.want) {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, %v", tt.out, a, b, c, err)
		}
	}
}
