package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// now is the time the tests' runs happen around.
var now = time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)

// testDirs are Carryon's folders in a temporary place, set up as after init.
func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{
		Conf: filepath.Join(root, "etc"), State: filepath.Join(root, "state"), Cache: filepath.Join(root, "cache"),
		Run: filepath.Join(root, "run"), Share: filepath.Join(root, "share"),
	}
	for _, dir := range []string{d.Conf, d.State, d.Run} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(state.DestinationPath(d), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// testUser is alice, with a home in a temporary folder.
func testUser(t *testing.T) sysexec.User {
	return sysexec.User{Name: "alice", UID: 1000, Home: t.TempDir()}
}

// failedStatus is a status whose last backup, at end, failed.
func failedStatus(end time.Time) state.Status {
	ok := state.Run{Kind: state.KindBackup, Trigger: state.TriggerTimer, Start: end.Add(-25 * time.Hour), End: end.Add(-24 * time.Hour), Result: state.ResultOK}
	failed := state.Run{
		Kind: state.KindBackup, Trigger: state.TriggerTimer, Start: end.Add(-time.Minute), End: end, Result: state.ResultFailed,
		Error: "unable to open the repository at sftp:alice@example.com:/backups",
		Fix:   "Check that example.com is reachable, then run: sudo carryon backup",
	}
	return state.Status{
		Format: state.StatusFormat, Updated: end, Machine: "desktop",
		LastBackup: &failed, LastSuccess: &ok, Recent: []state.Run{failed, ok},
	}
}

// okStatus is a status whose last backup, at end, worked.
func okStatus(end time.Time) state.Status {
	ok := state.Run{Kind: state.KindBackup, Trigger: state.TriggerTimer, Start: end.Add(-time.Minute), End: end, Result: state.ResultOK}
	return state.Status{Format: state.StatusFormat, Updated: end, Machine: "desktop", LastBackup: &ok, LastSuccess: &ok, Recent: []state.Run{ok}}
}

func writeStatus(t *testing.T, d paths.Dirs, s state.Status) {
	t.Helper()
	if err := state.WriteStatus(d, s); err != nil {
		t.Fatal(err)
	}
}

// putEntries adds entries to the inbox.
func putEntries(t *testing.T, d paths.Dirs, entries ...state.Entry) {
	t.Helper()
	_, err := state.UpdateInbox(d, time.Second, func(in *state.Inbox) error {
		for _, e := range entries {
			in.Put(e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pathEntry(path, app string, size int64, large bool) state.Entry {
	e := state.NewEntry(state.KindPath, path)
	e.App, e.Size, e.Large, e.Found, e.By = app, size, large, now.Add(-time.Hour), state.ByHook
	return e
}

func restoreEntry(bundle, app string) state.Entry {
	e := state.NewEntry(state.KindRestore, bundle)
	e.App, e.Found, e.By = app, now.Add(-time.Hour), state.ByHook
	return e
}

func settingsOn(sysexec.User) (config.UserNotify, error) {
	return config.UserNotify{Desktop: true}, nil
}

// shownNote is a notification a fake service showed.
type shownNote struct {
	ID uint32
	Notification
}

// actionKeys are a notification's button keys and labels, "key=Label".
func (n shownNote) buttons() []string {
	var b []string
	for _, a := range n.Actions {
		b = append(b, a.Key+"="+a.Label)
	}
	return b
}

// fakeDesktop is a notification service in memory.
type fakeDesktop struct {
	caps    []string
	signals chan Signal

	mu    sync.Mutex
	notes []shownNote
	fail  error // Notify fails with it while set
}

func newFakeDesktop(caps ...string) *fakeDesktop {
	return &fakeDesktop{caps: caps, signals: make(chan Signal, 8)}
}

func (f *fakeDesktop) Capabilities(context.Context) ([]string, error) { return f.caps, nil }

func (f *fakeDesktop) Notify(_ context.Context, n Notification) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	id := uint32(len(f.notes) + 1)
	f.notes = append(f.notes, shownNote{id, n})
	return id, nil
}

func (f *fakeDesktop) Signals() <-chan Signal { return f.signals }

func (f *fakeDesktop) shown() []shownNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]shownNote(nil), f.notes...)
}

func (f *fakeDesktop) setFail(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

// running is an agent running in the background, for a test.
type running struct {
	evals chan struct{}
	done  chan struct{} // closed when Run returned, with its error in err
	err   error
	stop  context.CancelFunc
}

// start runs a until the test ends or stop is called. Each look at the files sends on evals.
func start(t *testing.T, a *Agent) *running {
	t.Helper()
	r := &running{evals: make(chan struct{}, 100), done: make(chan struct{})}
	// It looks again at every tick, on a clock that starts at now.
	a.Poll, a.Recheck = 5*time.Millisecond, time.Millisecond
	t0 := time.Now()
	a.Now = func() time.Time { return now.Add(time.Since(t0)) }
	a.afterEval = func() {
		select {
		case r.evals <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = func() {
		cancel()
		<-r.done
	}
	go func() {
		r.err = a.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the agent didn't stop")
		}
	})
	return r
}

// waitEval waits until the agent has looked at the files n more times from now on. Every look but
// the first started after the call, so n = 2 sees what the test wrote before it.
func (r *running) waitEval(t *testing.T, n int) {
	t.Helper()
	for len(r.evals) > 0 {
		<-r.evals
	}
	for i := 0; i < n; i++ {
		select {
		case <-r.evals:
		case <-r.done:
			t.Fatalf("the agent stopped: %v", r.err)
		case <-time.After(10 * time.Second):
			t.Fatal("the agent didn't look at the files")
		}
	}
}

// eventually waits until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errNoService = errors.New("org.freedesktop.Notifications was not provided by any .service files")
