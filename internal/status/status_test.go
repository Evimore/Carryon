package status

import (
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/state"
)

func TestAgo(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-90 * time.Minute, "in 1 hour"},
		{10 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{47 * time.Hour, "47 hours ago"},
		{50 * time.Hour, "2 days ago"},
	}
	for _, tt := range tests {
		if got := Ago(tt.d); got != tt.want {
			t.Errorf("Ago(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestByMachine(t *testing.T) {
	at := func(day int) time.Time { return time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC) }
	got := ByMachine([]engine.Snapshot{
		{Hostname: "laptop", Time: at(3)},
		{Hostname: "desktop", Time: at(1)},
		{Hostname: "desktop", Time: at(5)},
	})
	want := []Machine{{Name: "desktop", Count: 2, Oldest: at(1), Newest: at(5)}, {Name: "laptop", Count: 1, Oldest: at(3), Newest: at(3)}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ByMachine = %+v\nwant %+v", got, want)
	}
}

func TestBuildState(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ok := &state.Run{Kind: state.KindBackup, Result: state.ResultOK, End: now.Add(-3 * time.Hour)}
	old := &state.Run{Kind: state.KindBackup, Result: state.ResultOK, End: now.Add(-72 * time.Hour)}
	failed := &state.Run{Kind: state.KindBackup, Result: state.ResultFailed, End: now.Add(-time.Hour)}
	tests := []struct {
		name        string
		in          Input
		state, line string
	}{
		{"not set up", Input{NotSetUp: true}, StateNotSetUp, "Backups aren't set up"},
		{"no status", Input{}, StateAttention, "No backup yet"},
		{"fine", Input{Found: true, Status: state.Status{Updated: now, LastBackup: ok, LastSuccess: ok}}, StateOK, "Backed up 3 hours ago"},
		{"failed", Input{Found: true, Status: state.Status{Updated: now, LastBackup: failed, LastSuccess: ok}}, StateAttention, "Backup failed"},
		{"too old", Input{Found: true, Status: state.Status{Updated: now, LastBackup: old, LastSuccess: old}}, StateAttention, "Backup 3 days old"},
		{"old but allowed", Input{Found: true, MaxAge: 96 * time.Hour, Status: state.Status{Updated: now, LastBackup: old, LastSuccess: old}}, StateOK, "Backed up 3 days ago"},
		{"restoring first", Input{Found: true, Status: state.Status{Updated: now, LastBackup: failed, LastSuccess: ok, Restoring: true}}, StateAttention, "Backups paused for a restore"},
		{"snapshots not read", Input{Found: true, Status: state.Status{Updated: now, LastBackup: ok, LastSuccess: ok}, Snapshots: &Snapshots{Error: "offline"}}, StateAttention, "Snapshots not read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Now = now
			r := Build(tt.in)
			if r.State != tt.state || r.Summary != tt.line {
				t.Errorf("state %q, summary %q; want %q, %q", r.State, r.Summary, tt.state, tt.line)
			}
			if (r.State == StateOK) != (len(r.Problems) == 0) {
				t.Errorf("state %q with problems %+v", r.State, r.Problems)
			}
		})
	}
}
