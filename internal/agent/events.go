package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/status"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// The units behind the buttons (docs/roadmap.md, "Names the tasks share"). An instance name is an
// inbox entry ID or a bundle ID, never a path.
const BackupUnit = "carryon-backup.service"

// RestoreUnit restores one bundle's saved settings. Characters a unit name can't hold, such as
// "+", are escaped as systemd-escape does ("\x2b"); carryon-restore@.service turns them back.
func RestoreUnit(bundle string) string {
	return "carryon-restore@" + escapeInstance(bundle) + ".service"
}

// escapeInstance escapes every byte of s that a unit's instance name can't hold as \xNN.
func escapeInstance(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(":_.-", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", c)
	}
	return b.String()
}

// AddUnit backs up the folder an inbox entry suggests.
func AddUnit(entry string) string { return "carryon-add@" + entry + ".service" }

// IgnoreUnit ignores the folder an inbox entry suggests.
func IgnoreUnit(entry string) string { return "carryon-ignore@" + entry + ".service" }

// Button keys, as the notification service sends them back.
const (
	keyBackup  = "backup"
	keyAdd     = "add"
	keyIgnore  = "ignore"
	keyRestore = "restore"
	keyLater   = "later"
)

// Urgencies of org.freedesktop.Notifications.
const (
	urgencyNormal   byte = 1
	urgencyCritical byte = 2
)

// event is something to show once. key says what it is about, so the agent remembers it was
// shown: a run's end time, an inbox entry's ID.
type event struct {
	key string
	n   Notification
}

// bundleRE is what a bundle ID may look like, as carryon-restore@.service accepts it: a word that
// can't be a path or an option.
var bundleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+-]{0,199}$`)

func backupNow() Action {
	return Action{Key: keyBackup, Label: "Back up now", Unit: BackupUnit, Command: "sudo carryon backup"}
}

// problemEvents are the problems in r that the user should hear about on the desktop, by the same
// rules as carryon status: a failed backup or check, and the newest backup getting too old.
func problemEvents(r status.Report) []event {
	var events []event
	failed := false
	for _, p := range r.Problems {
		switch p.Kind {
		case status.ProblemBackupFailed:
			if r.LastBackup == nil {
				continue
			}
			failed = true
			events = append(events, event{
				key: "backup-failed:" + stamp(r.LastBackup.End),
				n: Notification{
					Title: "Backup failed", Body: lines(p.Text, p.Fix), Icon: "dialog-warning",
					Urgency: urgencyCritical, Actions: []Action{backupNow()},
				},
			})
		case status.ProblemCheckFailed:
			if r.LastCheck == nil {
				continue
			}
			events = append(events, event{
				key: "check-failed:" + stamp(r.LastCheck.End),
				n: Notification{
					Title: "Backup check found problems", Body: lines(p.Text, p.Fix), Icon: "dialog-warning",
					Urgency: urgencyCritical,
				},
			})
		case status.ProblemBackupOld:
			if r.LastSuccess == nil {
				continue
			}
			events = append(events, event{
				key: "backup-old:" + stamp(r.LastSuccess.End),
				n: Notification{
					Title: "Your backup is getting old", Body: lines(p.Text, p.Fix), Icon: "dialog-warning",
					Urgency: urgencyNormal, Actions: []Action{backupNow()},
				},
			})
		}
	}
	if failed {
		// A failing backup already says it all; "too old" would only repeat it.
		kept := events[:0]
		for _, ev := range events {
			if !strings.HasPrefix(ev.key, "backup-old:") {
				kept = append(kept, ev)
			}
		}
		events = kept
	}
	return events
}

// entryEvent is the notification for an inbox entry. ok is false for an entry the agent can't
// offer, such as one whose ID isn't safe as a unit's instance name.
func (a *Agent) entryEvent(ctx context.Context, e state.Entry) (event, bool) {
	if !state.ValidEntryID(e.ID) {
		return event{}, false
	}
	key := "entry:" + e.ID
	switch e.Kind {
	case state.KindPath:
		if e.Path == "" {
			return event{}, false
		}
		body := e.Path + " holds data that isn't backed up yet."
		if d := entryDetails(e); d != "" {
			body += " " + d
		}
		return event{key: key, n: Notification{
			Title: "Back up " + e.Path + "?", Body: body, Icon: "dialog-question", Urgency: urgencyNormal,
			Actions: []Action{
				{Key: keyAdd, Label: "Back it up", Unit: AddUnit(e.ID), Command: "sudo carryon add " + e.ID},
				{Key: keyIgnore, Label: "Ignore", Unit: IgnoreUnit(e.ID), Command: "sudo carryon ignore " + e.ID},
			},
		}}, true
	case state.KindRestore:
		if !bundleRE.MatchString(e.Bundle) {
			return event{}, false
		}
		app := appName(e)
		body := []string{fmt.Sprintf("Your backup has saved settings for %s (%s).", app, e.Bundle)}
		if len(e.Changes) > 0 {
			body = append(body, "Restoring them would change:")
			for _, c := range e.Changes {
				body = append(body, "• "+c)
			}
		}
		if a.Running != nil && e.App != "" && a.Running(ctx, e.App) {
			body = append(body, fmt.Sprintf("%s is running: close it before you restore.", app))
		}
		body = append(body, "Restore them here?")
		// A renewed offer, whose plan changed, is a new event: the user sees what it does now.
		if len(e.Changes) > 0 {
			sum := sha256.Sum256([]byte(strings.Join(e.Changes, "\n")))
			key += ":" + hex.EncodeToString(sum[:5])
		}
		return event{key: key, n: Notification{
			Title: "Saved settings for " + app, Body: strings.Join(body, "\n"), Icon: "document-revert", Urgency: urgencyNormal,
			Actions: []Action{restoreAction(e), {Key: keyLater, Label: "Not now"}},
		}}, true
	}
	return event{}, false
}

func restoreAction(e state.Entry) Action {
	return Action{
		Key: keyRestore, Label: "Restore", Unit: RestoreUnit(e.Bundle),
		Command: "sudo carryon restore --bundle " + e.Bundle, App: e.App,
	}
}

// runningWarning is shown instead of restoring while the app runs. Its Restore button checks again.
func runningWarning(ac Action) Notification {
	return Notification{
		Title: "Close " + ac.App + " first",
		Body: fmt.Sprintf("%s is running. It could write its old settings back over the restored ones. "+
			"Close it, then choose Restore.", ac.App),
		Icon: "dialog-warning", Urgency: urgencyNormal,
		Actions: []Action{ac, {Key: keyLater, Label: "Not now"}},
	}
}

// startFailed says that a button's unit couldn't be started, and how to do it by hand.
func startFailed(ac Action, err error) Notification {
	why := lastLine(err)
	switch {
	case strings.Contains(why, "not found") || strings.Contains(why, "does not exist"):
		why = ac.Unit + " isn't installed. Update Carryon to get it."
	case strings.Contains(why, "Access denied") || strings.Contains(why, "authentication"):
		why = "You weren't allowed to start it."
	}
	return Notification{
		Title: "Couldn't " + strings.ToLower(ac.Label[:1]) + ac.Label[1:],
		Body:  lines(why, "Run instead: "+ac.Command),
		Icon:  "dialog-error", Urgency: urgencyNormal,
	}
}

// entryDetails says what Carryon knows about a suggested folder: "Probably jellyfin, 12.0 GiB
// (large)."
func entryDetails(e state.Entry) string {
	var parts []string
	if e.App != "" {
		parts = append(parts, "Probably "+e.App)
	}
	switch {
	case e.Size > 0 && e.Large:
		parts = append(parts, ui.Bytes(uint64(e.Size))+" (large)")
	case e.Size > 0:
		parts = append(parts, ui.Bytes(uint64(e.Size)))
	case e.Large:
		parts = append(parts, "large")
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// appName is what a restore offer calls the app: its package, or else its bundle.
func appName(e state.Entry) string {
	if e.App != "" {
		return e.App
	}
	return e.Bundle
}

// stamp is a time as an event key holds it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// lines joins the parts that aren't empty, one per line.
func lines(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

// lastLine is the last line of a program's error output, which usually says what went wrong.
func lastLine(err error) string {
	text := err.Error()
	var ee *sysexec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
		text = ee.Stderr
	}
	ls := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}
