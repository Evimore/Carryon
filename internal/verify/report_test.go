package verify

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestReportText(t *testing.T) {
	r := &Report{}
	r.section("The system record")
	r.ok("The record is complete.")
	var many []string
	for i := range 17 {
		many = append(many, fmt.Sprintf("+ package    tool-%02d", i))
	}
	r.add(Line{Level: Warning, Text: "17 changes since the last capture.", Details: many, Fix: "To save them now: sudo carryon backup"})
	r.section("The backup repository")
	r.add(Line{Level: Problem, Text: "The repository can't be opened.", Fix: "Check the password file.\nThen run carryon verify again."})
	r.info("Other machines in this repository: laptop.")
	var b bytes.Buffer
	r.Write(&b, false)
	want := `The system record
  ok    The record is complete.
  warn  17 changes since the last capture.
          + package    tool-00
`
	if !strings.HasPrefix(b.String(), want) {
		t.Errorf("starts:\n%s\nwant:\n%s", b.String(), want)
	}
	for _, s := range []string{
		"          + package    tool-14\n          ... and 2 more\n          → To save them now: sudo carryon backup\n",
		"\nThe backup repository\n  FAIL  The repository can't be opened.\n          → Check the password file.\n          → Then run carryon verify again.\n  info  Other machines",
		"\n1 problem, 1 warning.\n",
	} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, b.String())
		}
	}
	if strings.Contains(b.String(), "tool-15") {
		t.Errorf("shows more than %d details", maxDetails)
	}
}

func TestReportSummary(t *testing.T) {
	for _, tt := range []struct {
		lines []Level
		want  string
		ok    bool
	}{
		{[]Level{OK, Info}, "All good.\n", true},
		{[]Level{OK, Warning, Warning}, "All good, with 2 warnings.\n", true},
		{[]Level{Problem, Problem, Warning}, "2 problems, 1 warning.\n", false},
	} {
		r := &Report{}
		for _, l := range tt.lines {
			r.add(Line{Level: l, Text: string(l)})
		}
		var b bytes.Buffer
		r.Write(&b, false)
		if !strings.HasSuffix(b.String(), tt.want) || r.OK() != tt.ok {
			t.Errorf("%v: got %q, ok %v", tt.lines, b.String(), r.OK())
		}
	}
}
