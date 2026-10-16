package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/paths"
)

// The units and the pacman hook in dist/ run commands that exist, and their conditions name the
// flags Carryon writes.
func TestDistRunsRealCommands(t *testing.T) {
	files, _ := filepath.Glob("../../dist/*/*")
	if len(files) == 0 {
		t.Fatal("no files in dist/")
	}
	def := paths.Dirs{State: "/var/lib/carryon"}
	root := NewRoot(nil, paths.Default())
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			switch key {
			case "ExecStart", "Exec":
				words := strings.Fields(val)
				if words[0] != "/usr/bin/carryon" || len(words) < 2 {
					t.Errorf("%s: %s runs %q", f, key, val)
					continue
				}
				if words[1] == "verify" {
					continue // comes with T13
				}
				if cmd, _, err := root.Find(words[1:2]); err != nil || cmd == root {
					t.Errorf("%s: carryon has no %q command", f, words[1])
				}
			case "ConditionPathExists":
				p := strings.TrimPrefix(val, "!")
				if p != def.PendingFlag() && p != def.RestoringFlag() {
					t.Errorf("%s: condition on %s, which Carryon doesn't write", f, p)
				}
			}
		}
	}
}
