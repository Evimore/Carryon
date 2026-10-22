package ui

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// lockedWriter lets a prompt left waiting write while the test reads nothing.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Plain prompts read whole lines, so Ctrl+C reaches carryon as a signal, which ends the context.
// A question waiting for its line then ends, and so does every question after it.
func TestPlainPromptsEndWithTheContext(t *testing.T) {
	for name, ask := range map[string]func(*Terminal) error{
		"confirm": func(t *Terminal) error {
			_, err := t.Confirm(Confirm{Title: "Apply this plan?"})
			return err
		},
		"input": func(t *Terminal) error {
			_, err := t.Input(Input{Title: "Machine name", Default: "desktop"})
			return err
		},
		"checklist": func(t *Terminal) error {
			_, err := t.Checklist(sampleChecklist())
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			in, w := io.Pipe() // nobody types
			t.Cleanup(func() { w.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			term := NewTerminal(in, &lockedWriter{w: io.Discard}, true).WithContext(ctx)
			done := make(chan error, 1)
			go func() { done <- ask(term) }()
			time.Sleep(50 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, ErrCancelled) {
					t.Errorf("err = %v, want ErrCancelled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the question still waits after the context ended")
			}
			if err := ask(term); !errors.Is(err, ErrCancelled) {
				t.Errorf("the next question: err = %v, want ErrCancelled at once", err)
			}
		})
	}
}
