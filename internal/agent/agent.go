// Package agent is the per-user agent, `carryon agent`, which carryon-agent.service runs in each
// user's desktop session. It watches status.json and the inbox, which anyone may read, and shows
// desktop notifications with buttons over D-Bus (org.freedesktop.Notifications): a failed backup
// or check, a suggested folder, an offer to restore an app's saved settings. A button starts one of
// root's units with systemctl start, which polkit allows; the agent itself never runs as root and
// writes nothing outside the user's home. See docs/design.md, "Interfaces" and "Who runs what".
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/status"
	"github.com/Evimore/carryon/internal/sysexec"
)

// How often the agent looks at the files, and how often it decides again even when nothing
// changed, since some problems (the newest backup getting too old) only appear with time.
const (
	DefaultPoll    = 5 * time.Second
	DefaultRecheck = 10 * time.Minute
	// retryDelay is how soon the agent tries again when the notification service didn't answer.
	retryDelay = time.Minute
	// startTimeout bounds systemctl start, which waits while polkit asks for a password.
	startTimeout = 5 * time.Minute
)

// Desktop is the notification service in the user's session. Bus is the real one.
type Desktop interface {
	// Capabilities lists what the service can do ("actions", "body-markup"...).
	Capabilities(ctx context.Context) ([]string, error)
	// Notify shows n and returns its ID.
	Notify(ctx context.Context, n Notification) (uint32, error)
	// Signals delivers buttons pressed and notifications closed. It is closed when the
	// connection is lost.
	Signals() <-chan Signal
}

// Signal is something the user did with a notification.
type Signal struct {
	ID     uint32
	Action string // the key of the button pressed; "" when the notification closed
	Closed bool
}

// Notification is one desktop notification.
type Notification struct {
	Title   string
	Body    string
	Icon    string
	Urgency byte // 0 low, 1 normal, 2 critical
	Actions []Action
}

// Action is a button.
type Action struct {
	Key   string
	Label string
	// Unit is the unit the button starts; "" for a button that only closes the notification.
	Unit string
	// Command is how to do the same by hand, for a service that shows no buttons and for when
	// starting the unit fails.
	Command string
	// App, for a restore, is the app that must not be running.
	App string
}

// Agent shows one user's notifications.
type Agent struct {
	Dirs    paths.Dirs
	User    sysexec.User
	Desktop Desktop
	// Runner runs systemctl start for the buttons.
	Runner sysexec.Runner
	// StateDir is where the agent remembers what it showed, inside the user's home: see StateDir.
	StateDir string
	// Running reports whether the user runs app. Nil means never.
	Running func(ctx context.Context, app string) bool
	// Settings reads the user's own notification settings. Nil means notify.UserSettings.
	Settings func(u sysexec.User) (config.UserNotify, error)
	Now      func() time.Time // nil means time.Now
	Poll     time.Duration    // 0 means DefaultPoll
	Recheck  time.Duration    // 0 means DefaultRecheck
	// Warn reports problems that don't stop the agent. May be nil.
	Warn func(format string, a ...any)

	afterEval func() // tests: called after each look at the files

	shown  shown
	live   map[uint32]Notification // what is on screen, to answer its buttons
	starts chan startResult
	wg     sync.WaitGroup
}

// startResult is how starting a button's unit went.
type startResult struct {
	action Action
	err    error
}

// StateFile is the file in StateDir that remembers what was shown.
const StateFile = "agent.json"

// StateDir is where the agent keeps its state: $XDG_STATE_HOME/carryon when that is inside home,
// else ~/.local/state/carryon, so it never writes outside the user's home.
func StateDir(home, xdgStateHome string) string {
	home = filepath.Clean(home)
	if filepath.IsAbs(xdgStateHome) {
		if dir := filepath.Clean(xdgStateHome); strings.HasPrefix(dir, home+string(filepath.Separator)) {
			return filepath.Join(dir, "carryon")
		}
	}
	return filepath.Join(home, ".local", "state", "carryon")
}

// Run shows notifications until ctx ends. It returns an error when the connection to the
// session is lost, so systemd starts the agent again.
func (a *Agent) Run(ctx context.Context) error {
	file := filepath.Join(a.StateDir, StateFile)
	var err error
	if a.shown, err = loadShown(file); err != nil {
		a.warn("%v; starting afresh", err)
	}
	a.live = map[uint32]Notification{}
	a.starts = make(chan startResult)
	ctx, cancel := context.WithCancel(ctx)
	defer a.wg.Wait()
	defer cancel()

	poll, recheck := a.Poll, a.Recheck
	if poll <= 0 {
		poll = DefaultPoll
	}
	if recheck <= 0 {
		recheck = DefaultRecheck
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	signals := a.Desktop.Signals()
	last, next := "", time.Time{}
	for {
		if fp := a.fingerprint(); fp != last || !a.now().Before(next) {
			last = fp
			wait := recheck
			if !a.evaluate(ctx) {
				wait = min(retryDelay, recheck)
			}
			next = a.now().Add(wait)
			if err := a.shown.save(file); err != nil {
				a.warn("%v", err)
			}
			if a.afterEval != nil {
				a.afterEval()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case sig, ok := <-signals:
			if !ok {
				return errors.New("lost the connection to the desktop session")
			}
			a.handle(ctx, sig)
		case res := <-a.starts:
			a.started(ctx, res)
		}
	}
}

// fingerprint changes whenever one of the files the agent reads changes.
func (a *Agent) fingerprint() string {
	var b strings.Builder
	for _, f := range []string{
		state.RunStatusFile(a.Dirs), a.Dirs.StatusFile(), a.Dirs.InboxFile(), a.Dirs.PendingFlag(),
		a.Dirs.RestoringFlag(), a.Dirs.ConfigFile(), filepath.Join(a.User.Home, config.UserConfigPath),
	} {
		fi, err := os.Stat(f)
		if err != nil {
			b.WriteString("-;")
			continue
		}
		var ino uint64
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino = st.Ino
		}
		fmt.Fprintf(&b, "%d.%d.%d;", ino, fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}

// evaluate shows what is new. It reports false when something couldn't be shown and should be
// tried again soon.
func (a *Agent) evaluate(ctx context.Context) bool {
	settings := a.Settings
	if settings == nil {
		settings = notify.UserSettings
	}
	n, err := settings(a.User)
	if err != nil {
		a.warn("%v (showing notifications anyway)", err)
	}
	if !n.Desktop {
		return true // switched off: nothing is shown, so what is still current shows when it's back on
	}
	if cfg, _, err := config.Load(a.Dirs.ConfigFile()); err == nil && !notify.Covered(cfg.Backup.Users, a.User.Name, a.User.UID) {
		return true // backup.users leaves this user out, as root's notifications do
	}

	var events []event
	complete := true
	in, err := status.ReadInput(a.Dirs, a.now())
	if err != nil {
		a.warn("%v", err)
		complete = false
	} else {
		events = append(events, problemEvents(status.Build(in))...)
	}
	inbox, err := state.ReadInbox(a.Dirs)
	if err != nil {
		a.warn("%v", err)
		complete = false
	} else {
		for _, e := range inbox.Entries {
			if ev, ok := a.entryEvent(ctx, e); ok {
				events = append(events, ev)
			}
		}
	}
	// What is no longer current is forgotten, but only when everything could be read: a file
	// that can't be read for a moment mustn't bring back everything it held.
	if complete {
		current := map[string]bool{}
		for _, ev := range events {
			current[ev.key] = true
		}
		a.shown.keep(current)
	}

	var caps []string
	for _, ev := range events {
		if a.shown.has(ev.key) {
			continue
		}
		if caps == nil {
			if caps, err = a.Desktop.Capabilities(ctx); err != nil {
				a.warn("the desktop's notification service doesn't answer: %v", err)
				return false
			}
		}
		if _, err := a.show(ctx, ev.n, caps); err != nil {
			a.warn("couldn't show %q: %v", ev.n.Title, err)
			return false
		}
		a.shown.add(ev.key, a.now())
	}
	return true
}

// show shows n, fitted to what the service can do: without buttons, the body says what to run
// instead; with markup, the body is escaped.
func (a *Agent) show(ctx context.Context, n Notification, caps []string) (uint32, error) {
	if !has(caps, "actions") {
		var hints []string
		for _, ac := range n.Actions {
			if ac.Command != "" {
				hints = append(hints, ac.Label+": "+ac.Command)
			}
		}
		if len(hints) > 0 {
			n.Body += "\n" + strings.Join(hints, "\n")
		}
		n.Actions = nil
	}
	if has(caps, "body-markup") {
		n.Body = escapeMarkup(n.Body)
	}
	id, err := a.Desktop.Notify(ctx, n)
	if err != nil {
		return 0, err
	}
	if len(n.Actions) > 0 {
		a.live[id] = n
	}
	return id, nil
}

// handle answers a button or forgets a closed notification.
func (a *Agent) handle(ctx context.Context, sig Signal) {
	n, ok := a.live[sig.ID]
	if !ok {
		return
	}
	if sig.Closed {
		delete(a.live, sig.ID)
		return
	}
	var ac Action
	for _, c := range n.Actions {
		if c.Key == sig.Action {
			ac = c
		}
	}
	if ac.Key == "" {
		return // the notification itself was clicked, not a button
	}
	delete(a.live, sig.ID)
	if ac.Unit == "" {
		return
	}
	if ac.App != "" && a.Running != nil && a.Running(ctx, ac.App) {
		a.notifyNow(ctx, runningWarning(ac))
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		sctx, cancel := context.WithTimeout(ctx, startTimeout)
		defer cancel()
		// --no-block: the unit runs on its own; systemctl only waits for polkit's answer.
		_, err := a.Runner.Output(sctx, "systemctl", "start", "--no-block", ac.Unit)
		select {
		case a.starts <- startResult{ac, err}:
		case <-ctx.Done():
		}
	}()
}

// started tells the user when a button's unit couldn't be started.
func (a *Agent) started(ctx context.Context, res startResult) {
	if res.err == nil {
		return
	}
	a.warn("starting %s: %v", res.action.Unit, res.err)
	a.notifyNow(ctx, startFailed(res.action, res.err))
}

// notifyNow shows a notification that isn't an event, such as an answer to a button.
func (a *Agent) notifyNow(ctx context.Context, n Notification) {
	caps, err := a.Desktop.Capabilities(ctx)
	if err == nil {
		_, err = a.show(ctx, n, caps)
	}
	if err != nil {
		a.warn("couldn't show %q: %v", n.Title, err)
	}
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) warn(format string, args ...any) {
	if a.Warn != nil {
		a.Warn(format, args...)
	}
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// escapeMarkup escapes text for a notification body that may hold markup.
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
