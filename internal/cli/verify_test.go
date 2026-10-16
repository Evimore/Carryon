package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/verify"
)

func TestVerifyNeedsRoot(t *testing.T) {
	dirs := fakeMachine(t, true, nil)
	asUser(t, 1000)
	_, err := run(t, dirs, "verify")
	if err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyDeepAndFullDontGoTogether(t *testing.T) {
	asUser(t, 0)
	_, err := run(t, fakeMachine(t, true, nil), "verify", "--deep", "--full")
	if err == nil || !strings.Contains(err.Error(), "--full already includes") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyReportsAProblemWithExitStatus1(t *testing.T) {
	asUser(t, 0)
	dirs := fakeMachine(t, true, nil)
	out, err := run(t, dirs, "verify")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("err = %v\n%s", err, out)
	}
	for _, want := range []string{"The system record", "FAIL  There is no system record", "Make one with: sudo carryon backup", "1 problem, 0 warnings."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	runs, _ := state.ReadRuns(dirs)
	if len(runs) != 1 || runs[0].Kind != state.KindVerify || runs[0].Result != state.ResultFailed {
		t.Errorf("runs = %+v", runs)
	}
}

func TestVerifyJSON(t *testing.T) {
	asUser(t, 0)
	out, err := run(t, fakeMachine(t, true, nil), "verify", "--json", "--deep")
	if err == nil {
		t.Fatal("want a failing exit")
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in:\n%s", out)
	}
	var rep verify.Report
	if err := json.Unmarshal([]byte(out[start:]), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Problems != 1 || !rep.Deep || rep.Sections[0].Lines[0].Level != verify.Problem {
		t.Errorf("report = %+v", rep)
	}
}
