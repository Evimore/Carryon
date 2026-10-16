package notify

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

type stubSource string

func (s stubSource) LastError(context.Context, string) (string, error) { return string(s), nil }

func TestRunHistoryLastError(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, r := range []state.Run{
		{Kind: state.KindBackup, Unit: "carryon-backup.service", Invocation: "old", Start: t0, End: t0, Result: state.ResultFailed, Error: "old failure"},
		{Kind: state.KindBackup, Unit: "carryon-settle.service", Invocation: "s1", Start: t0, End: t0, Result: state.ResultOK},
		{Kind: state.KindBackup, Unit: "carryon-backup.service", Invocation: "new", Start: t0, End: t0, Result: state.ResultFailed,
			Error: "Fatal: wrong password or no key found", Fix: "Check the password in /etc/carryon/repository-password."},
	} {
		if err := state.AppendRun(d, r); err != nil {
			t.Fatal(err)
		}
	}
	journal := stubSource("from the journal")
	for _, tc := range []struct {
		name, unit, invocation, want string
	}{
		{"latest run failed", "carryon-backup.service", "new", "Fatal: wrong password or no key found\nCheck the password in /etc/carryon/repository-password."},
		{"no invocation check", "carryon-backup.service", "", "Fatal: wrong password or no key found\nCheck the password in /etc/carryon/repository-password."},
		{"the failed run recorded nothing", "carryon-backup.service", "newer", "from the journal"},
		{"latest run succeeded", "carryon-settle.service", "s1", "from the journal"},
		{"not Carryon's run", "carryon-verify.service", "v1", "from the journal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sysexec.Fake{}
			f.On("systemctl show -p InvocationID --value "+tc.unit, sysexec.Response{Stdout: tc.invocation + "\n"})
			got, err := RunHistory{Dirs: d, Runner: f, Fallback: journal}.LastError(context.Background(), tc.unit)
			if err != nil || got != tc.want {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
