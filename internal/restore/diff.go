package restore

import (
	"bytes"
	"fmt"
	"strings"
)

// maxDiff is the size above which files aren't compared line by line.
const maxDiff = 2 << 20

// diffContext is how many unchanged lines surround each change, as with diff -u.
const diffContext = 3

// UnifiedDiff compares two versions of a file and returns the differences the way diff -u prints
// them, with the given labels for the old and new version. It returns "" when they're the same.
// Binary files and files over 2 MiB get a one-line note instead.
func UnifiedDiff(oldLabel, newLabel string, old, new []byte) string {
	if bytes.Equal(old, new) {
		return ""
	}
	if bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(new, 0) >= 0 {
		return fmt.Sprintf("Binary files %s and %s differ\n", oldLabel, newLabel)
	}
	if len(old) > maxDiff || len(new) > maxDiff {
		return fmt.Sprintf("Files %s and %s differ (too big to show the changes)\n", oldLabel, newLabel)
	}
	a, b := splitLines(old), splitLines(new)
	ops := diffLines(a, b)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldLabel, newLabel)
	for start := 0; start < len(ops); {
		// Find the next change, then extend the hunk while changes are close together.
		first := start
		for first < len(ops) && ops[first].kind == ' ' {
			first++
		}
		if first == len(ops) {
			break
		}
		lo := max(first-diffContext, start)
		hi := first
		for i := first; i < len(ops); i++ {
			if ops[i].kind != ' ' {
				hi = i + 1
			} else if i-hi >= 2*diffContext {
				break
			}
		}
		hi = min(hi+diffContext, len(ops))
		writeHunk(&out, ops[lo:hi])
		start = hi
	}
	return out.String()
}

type diffOp struct {
	kind   byte // ' ', '-' or '+'
	text   string
	oldNum int // line number in the old file (1-based) of this or the next old line
	newNum int
}

func writeHunk(out *strings.Builder, ops []diffOp) {
	var oldN, newN int
	for _, op := range ops {
		if op.kind != '+' {
			oldN++
		}
		if op.kind != '-' {
			newN++
		}
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(ops[0].oldNum, oldN), hunkRange(ops[0].newNum, newN))
	for _, op := range ops {
		out.WriteByte(op.kind)
		if text, ok := strings.CutSuffix(op.text, "\n"); ok {
			out.WriteString(text + "\n")
		} else {
			out.WriteString(op.text + "\n\\ No newline at end of file\n")
		}
	}
}

func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	case 1:
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// splitLines splits text into lines that keep their newline, so a last line without one differs
// from the same line with one, as in diff.
func splitLines(b []byte) []string {
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines returns the shortest edit script turning a into b (Myers' algorithm), with removed
// lines before added ones in each change, as diff prints them.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	limit := n + m
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int // trace[d]: v[-d..d] before step d
	var d int
search:
	for d = 0; d <= limit; d++ {
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				break search
			}
		}
	}

	var rev []diffOp
	x, y := n, m
	for ; d > 0; d-- {
		prev := trace[d]
		at := func(k int) int { return prev[k+d] }
		k := x - y
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		for x > px && y > py {
			rev = append(rev, diffOp{kind: ' ', text: a[x-1]})
			x--
			y--
		}
		if x == px {
			rev = append(rev, diffOp{kind: '+', text: b[y-1]})
			y--
		} else {
			rev = append(rev, diffOp{kind: '-', text: a[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		rev = append(rev, diffOp{kind: ' ', text: a[x-1]})
		x--
		y--
	}

	ops := make([]diffOp, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		ops = append(ops, rev[i])
	}
	// Removed lines first within each change.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].kind != ' ' {
			j++
		}
		var dels, adds []diffOp
		for _, op := range ops[i:j] {
			if op.kind == '-' {
				dels = append(dels, op)
			} else {
				adds = append(adds, op)
			}
		}
		copy(ops[i:], append(dels, adds...))
		i = j
	}
	// Line numbers.
	on, nn := 1, 1
	for i := range ops {
		ops[i].oldNum, ops[i].newNum = on, nn
		if ops[i].kind != '+' {
			on++
		}
		if ops[i].kind != '-' {
			nn++
		}
	}
	return ops
}
