package facts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTree makes a fake system tree in a temp folder from a description in testdata (see
// testdata/desktop.txt for the format). The trees are described in text rather than checked in,
// since sysfs names have colons and symlinks, which Go module zips don't allow.
func buildTree(t *testing.T, desc string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", desc))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var cur *strings.Builder
	var curPath string
	flush := func() {
		if cur == nil {
			return
		}
		write(t, root, curPath, cur.String())
		cur = nil
	}
	for _, line := range strings.SplitAfter(string(b), "\n") {
		trimmed := strings.TrimRight(line, "\n")
		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			flush()
			name := strings.TrimSuffix(strings.TrimPrefix(trimmed, "-- "), " --")
			if from, to, ok := strings.Cut(name, " -> "); ok {
				p := filepath.Join(root, from)
				mkdir(t, filepath.Dir(p))
				if err := os.Symlink(to, p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			cur, curPath = &strings.Builder{}, name
			continue
		}
		if cur == nil {
			continue // comments and blank lines before the first file
		}
		cur.WriteString(line)
	}
	flush()
	return root
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, name)
	mkdir(t, filepath.Dir(p))
	if content = strings.TrimRight(content, "\n"); content != "" {
		content += "\n" // blank lines between files belong to neither
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeRunner returns recorded output for each command line and records what was run.
type fakeRunner struct {
	out   map[string]string // "name arg1 arg2" -> stdout
	fail  map[string]bool   // command lines that exit non-zero
	have  map[string]bool   // installed programs; nil means all of them
	calls []string
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	var err error
	if f.fail[cmd] {
		err = fmt.Errorf("%s: exit status 1", name)
	}
	return []byte(f.out[cmd]), err
}

func (f *fakeRunner) Exists(name string) bool { return f.have == nil || f.have[name] }

func (f *fakeRunner) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
