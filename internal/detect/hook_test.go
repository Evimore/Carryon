package detect

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/state"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestHookHappyPath(t *testing.T) {
	var out syncBuf
	var recorded []state.Run
	pending := 0
	Hook(context.Background(), HookOptions{
		Refresh:  func(context.Context) ([]string, error) { return []string{"+ package vim [extra]"}, nil },
		Pending:  func() error { pending++; return nil },
		Record:   func(r state.Run) { recorded = append(recorded, r) },
		Packages: []string{"vim"},
		Delay:    "1h",
		Out:      &out,
	})
	if pending != 1 {
		t.Errorf("pending marked %d times", pending)
	}
	if !strings.Contains(out.String(), "1 change(s): + package vim") || !strings.Contains(out.String(), "nothing has changed for 1h") {
		t.Errorf("hints:\n%s", out.String())
	}
	if len(recorded) != 1 || recorded[0].Result != state.ResultOK || recorded[0].Changes != 1 || recorded[0].Packages[0] != "vim" {
		t.Errorf("recorded %+v", recorded)
	}
}

// Every step fails in every way it can: the hook still returns, on time, without panicking.
func TestHookNeverFailsOrBlocks(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	cases := map[string]HookOptions{
		"refresh fails": {
			Refresh: func(context.Context) ([]string, error) { return nil, errors.New("pacman database locked") },
			Pending: func() error { return errors.New("read-only file system") },
		},
		"refresh panics": {
			Refresh: func(context.Context) ([]string, error) { panic("nil map") },
		},
		"record panics": {
			Refresh: func(context.Context) ([]string, error) { return nil, nil },
			Record:  func(state.Run) { panic("disk on fire") },
		},
		"pending panics": {
			Pending: func() error { panic("boom") },
		},
		"refresh hangs, ignoring its context": {
			Refresh: func(context.Context) ([]string, error) { <-block; return nil, nil },
		},
		"refresh hangs until cancelled": {
			Refresh: func(ctx context.Context) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() },
		},
		"output broken": {
			Refresh: func(context.Context) ([]string, error) { return nil, errors.New("x") },
		},
		"scan hangs, ignoring its context": {
			Scan:     func(context.Context, []string) (Outcome, error) { <-block; return Outcome{}, nil },
			Packages: []string{"jellyfin-server"},
		},
		"scan is slow": {
			Scan: func(ctx context.Context, _ []string) (Outcome, error) {
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
				return Outcome{New: []state.Entry{state.NewEntry(state.KindPath, "/var/lib/late")}}, nil
			},
			Packages: []string{"jellyfin-server"},
		},
		"scan panics": {
			Scan:     func(context.Context, []string) (Outcome, error) { panic("nil map") },
			Packages: []string{"jellyfin-server"},
		},
		"scan fails": {
			Scan: func(context.Context, []string) (Outcome, error) {
				return Outcome{}, errors.New("pacman database locked")
			},
			Packages: []string{"jellyfin-server"},
		},
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			var out syncBuf
			opt.Out = &out
			if name == "output broken" {
				opt.Out = failingWriter{}
			}
			opt.Timeout = 200 * time.Millisecond
			start := time.Now()
			Hook(context.Background(), opt) // must not panic
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("took %v", d)
			}
			if name != "output broken" && strings.TrimSpace(out.String()) == "" {
				t.Error("no hint said what went wrong")
			}
		})
	}
}

func TestHookAfterChangesOff(t *testing.T) {
	var out syncBuf
	Hook(context.Background(), HookOptions{
		Refresh: func(context.Context) ([]string, error) { return nil, nil },
		Out:     &out,
	})
	if strings.Contains(out.String(), "backs up") {
		t.Errorf("promised a backup that won't come:\n%s", out.String())
	}
}

func TestHookPrintsALinePerNewFolder(t *testing.T) {
	var out syncBuf
	var recorded []state.Run
	jelly := state.NewEntry(state.KindPath, "/var/lib/jellyfin")
	jelly.App, jelly.Size = "jellyfin-server", 3<<20
	db := state.NewEntry(state.KindPath, "/var/lib/postgres")
	var asked []string
	Hook(context.Background(), HookOptions{
		Refresh: func(context.Context) ([]string, error) { return nil, nil },
		Scan: func(_ context.Context, pkgs []string) (Outcome, error) {
			asked = pkgs
			return Outcome{New: []state.Entry{jelly}, Added: []state.Entry{db}}, nil
		},
		Record:   func(r state.Run) { recorded = append(recorded, r) },
		Packages: []string{"jellyfin-server", "postgresql"},
		Out:      &out,
	})
	if strings.Join(asked, " ") != "jellyfin-server postgresql" {
		t.Errorf("scanned the folders of %q", asked)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(l, "/var/lib/") {
			lines = append(lines, l)
		}
	}
	want := []string{
		"Carryon: /var/lib/jellyfin holds new data (probably jellyfin-server, 3.0 MiB). Back it up with: sudo carryon add " + jelly.ID,
		`Carryon: now backing up /var/lib/postgres, found holding new data (detect.new_paths = "auto")`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(recorded) != 1 || len(recorded[0].Notes) != 1 || !strings.Contains(recorded[0].Notes[0], "/var/lib/postgres") {
		t.Errorf("recorded %+v", recorded)
	}
}

// Without packages there are no folders to look at: the hook doesn't scan everything.
func TestHookScansNothingWithoutPackages(t *testing.T) {
	Hook(context.Background(), HookOptions{
		Scan: func(context.Context, []string) (Outcome, error) { t.Error("scanned"); return Outcome{}, nil },
		Out:  &syncBuf{},
	})
}

// Another writer holding the inbox makes the hook skip its suggestion, quietly and quickly.
func TestHookSkipsABusyInbox(t *testing.T) {
	d := testDirs(t)
	release := holdInbox(t, d)
	defer release()
	var out syncBuf
	start := time.Now()
	Hook(context.Background(), HookOptions{
		Scan: func(context.Context, []string) (Outcome, error) {
			found := Findings{Found: []state.Entry{state.NewEntry(state.KindPath, "/var/lib/jellyfin")}}
			return Suggest(found, SuggestOptions{Dirs: d, Mode: "ask", By: state.ByHook, Wait: 50 * time.Millisecond})
		},
		Packages: []string{"jellyfin-server"},
		Out:      &out,
		Timeout:  5 * time.Second,
	})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s", took)
	}
	if strings.Contains(out.String(), "jellyfin") || strings.Contains(out.String(), "couldn't") {
		t.Errorf("hints:\n%s", out.String())
	}
}

// A scan that takes too long is left behind, and the hook's run is still recorded.
func TestHookRecordsTheRunWhenTheScanIsSlow(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var out syncBuf
	var recorded []state.Run
	var mu sync.Mutex
	Hook(context.Background(), HookOptions{
		Refresh:  func(context.Context) ([]string, error) { return nil, nil },
		Scan:     func(context.Context, []string) (Outcome, error) { <-block; return Outcome{}, nil },
		Record:   func(r state.Run) { mu.Lock(); defer mu.Unlock(); recorded = append(recorded, r) },
		Packages: []string{"jellyfin-server"},
		Out:      &out,
		Timeout:  400 * time.Millisecond,
	})
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 || recorded[0].Result != state.ResultOK {
		t.Errorf("recorded %+v", recorded)
	}
	if !strings.Contains(out.String(), "stopped looking for new data folders") {
		t.Errorf("hints:\n%s", out.String())
	}
}
