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
