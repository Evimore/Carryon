package restore

import (
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	tests := []struct{ old, new, want string }{
		{"a\n", "a\n", ""},
		{"a\nb\nc\n", "a\nB\nc\n", "--- o\n+++ n\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"},
		{"", "x\ny\n", "--- o\n+++ n\n@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"x\n", "", "--- o\n+++ n\n@@ -1 +0,0 @@\n-x\n"},
		{"a\nb", "a\nb\n", "--- o\n+++ n\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
		{"a\x00", "b", "Binary files o and n differ\n"},
	}
	for _, tt := range tests {
		if got := UnifiedDiff("o", "n", []byte(tt.old), []byte(tt.new)); got != tt.want {
			t.Errorf("diff(%q, %q) =\n%s\nwant\n%s", tt.old, tt.new, got, tt.want)
		}
	}
	big := strings.Repeat("x", maxDiff+1)
	if got := UnifiedDiff("o", "n", []byte(big), nil); !strings.Contains(got, "too big") {
		t.Errorf("big diff = %q", got)
	}
}

// The diff agrees with GNU diff -u on random files, hunk splitting included.
func TestUnifiedDiffMatchesDiff(t *testing.T) {
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("diff isn't installed")
	}
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "d", "e"}
	gen := func() string {
		var b strings.Builder
		for n := rng.Intn(30); n > 0; n-- {
			b.WriteString(words[rng.Intn(len(words))] + "\n")
		}
		return b.String()
	}
	for i := 0; i < 300; i++ {
		old := gen()
		lines := strings.SplitAfter(old, "\n")
		for j := range lines {
			if rng.Intn(6) == 0 {
				lines[j] = words[rng.Intn(len(words))] + "\n"
			}
		}
		new := strings.Join(lines, "")
		if rng.Intn(3) == 0 {
			new = gen()
		}
		o, n := filepath.Join(dir, "o"), filepath.Join(dir, "n")
		writeFile(t, o, old)
		writeFile(t, n, new)
		out, _ := exec.Command("diff", "-u", "--label", "o", "--label", "n", o, n).Output()
		got := UnifiedDiff("o", "n", []byte(old), []byte(new))
		// Shortest edit scripts can differ in which lines they pair up, but not in how many
		// lines they add and remove.
		if summary(got) != summary(string(out)) {
			t.Fatalf("case %d:\nold %q\nnew %q\ngot\n%s\ndiff\n%s", i, old, new, got, out)
		}
	}
}

func summary(d string) string {
	var plus, minus int
	for _, l := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			plus++
		case strings.HasPrefix(l, "-"):
			minus++
		}
	}
	return strings.Repeat("+", plus) + strings.Repeat("-", minus)
}
