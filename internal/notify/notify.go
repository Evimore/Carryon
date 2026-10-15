// Package notify tells people that something went wrong: a plain desktop notification in each
// signed-in user's session, a push through ntfy, or a webhook. It ports the prototype's cmd_notify
// and last_error (legacy/sysbackup.py).
//
// The root service sends the desktop fallback; the v0.2 user agent will show richer notifications
// with buttons over D-Bus. Every sender has short timeouts, so a notification never hangs the
// systemd unit that sends it, and text that leaves the machine is redacted first (see Redact).
package notify

import (
	"context"
	"errors"
	"time"
)

// Urgency is how loudly a message asks for attention.
type Urgency int

const (
	// Normal is for information, such as a test message.
	Normal Urgency = iota
	// Critical is for a failure the user has to act on.
	Critical
)

func (u Urgency) String() string {
	if u == Critical {
		return "critical"
	}
	return "normal"
}

// Message is one notification.
type Message struct {
	Title   string
	Body    string
	Urgency Urgency
	Unit    string    // the systemd unit it is about, if any
	Host    string    // the machine it comes from
	Time    time.Time // when it was made; zero means now
}

func (m Message) when() time.Time {
	if m.Time.IsZero() {
		return time.Now()
	}
	return m.Time
}

// Sender delivers messages through one channel.
type Sender interface {
	// Name says which channel this is, for the user ("desktop", "ntfy (ntfy.sh)"). It never
	// contains a secret, such as an ntfy topic or a webhook's path.
	Name() string
	// Send delivers m. to says who received it, in a few words ("alice", "ntfy.sh").
	Send(ctx context.Context, m Message) (to string, err error)
}

// Result is how sending through one channel went.
type Result struct {
	Channel string
	To      string // who received it, when Err is nil
	Err     error
}

// SendAll sends m through every sender, one after the other, and reports each result.
func SendAll(ctx context.Context, m Message, senders []Sender) []Result {
	results := make([]Result, 0, len(senders))
	for _, s := range senders {
		to, err := s.Send(ctx, m)
		results = append(results, Result{Channel: s.Name(), To: to, Err: err})
	}
	return results
}

// Delivered reports whether at least one channel delivered the message.
func Delivered(results []Result) bool {
	for _, r := range results {
		if r.Err == nil {
			return true
		}
	}
	return false
}

// ErrNoChannel is returned when no notification channel is set up.
var ErrNoChannel = errors.New("no notification channel is set up: turn on notify.desktop, or set notify.ntfy_url or notify.webhook_url in the config")
