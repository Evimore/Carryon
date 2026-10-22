package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/sysexec"
)

// Details is the last line of every failure message: where to read more.
const Details = "Details: carryon status"

// maxErrorText is the most of a run's error text a notification shows, like the prototype.
const maxErrorText = 300

// errorLines is how many error lines of a run a notification shows.
const errorLines = 3

// ErrorSource gives the error lines of a unit's latest run. Journal reads them from the systemd
// journal; RunHistory reads Carryon's own run history.
type ErrorSource interface {
	// LastError returns the last error lines of unit's latest run, or "" if it logged none.
	LastError(ctx context.Context, unit string) (string, error)
}

// Journal reads a unit's errors from the systemd journal, like the prototype's last_error.
type Journal struct {
	Runner sysexec.Runner
}

// errorLineRE picks the error lines of runs that logged without priorities.
var errorLineRE = regexp.MustCompile(`^xx |Fatal|unable to|error`)

// xxRE is Carryon's error marker at the start of a line.
var xxRE = regexp.MustCompile(`^xx\s*`)

// LastError finds the unit's latest run by its invocation ID and returns its last error lines:
// the lines logged at error priority, or, when there are none (older runs logged without
// priorities), the lines that look like errors.
func (j Journal) LastError(ctx context.Context, unit string) (string, error) {
	out, err := j.Runner.Output(ctx, "systemctl", "show", "-p", "InvocationID", "--value", unit)
	if err != nil {
		return "", fmt.Errorf("finding the latest run of %s: %w", unit, err)
	}
	inv := strings.TrimSpace(string(out))
	if inv == "" {
		return "", nil
	}
	match := "_SYSTEMD_INVOCATION_ID=" + inv
	out, err = j.Runner.Output(ctx, "journalctl", match, "-p", "err", "-o", "cat", "--no-pager")
	if err != nil {
		return "", fmt.Errorf("reading the journal of %s: %w", unit, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		out, err = j.Runner.Output(ctx, "journalctl", match, "-o", "cat", "--no-pager")
		if err != nil {
			return "", fmt.Errorf("reading the journal of %s: %w", unit, err)
		}
		var picked []string
		for _, ln := range strings.Split(string(out), "\n") {
			if errorLineRE.MatchString(ln) {
				picked = append(picked, ln)
			}
		}
		text = strings.Join(picked, "\n")
	}
	return lastLines(text, errorLines), nil
}

// lastLines returns the last n non-empty lines of s, trimmed and without Carryon's "xx" marker.
func lastLines(s string, n int) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, xxRE.ReplaceAllString(ln, ""))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// RemoteTimeout is how long the rclone remote check may take.
const RemoteTimeout = 60 * time.Second

// RemoteCheck checks whether the rclone remote behind an rclone: repository answers, as the user
// whose rclone login it is (repository.rclone_user). It generalizes the prototype's iCloud check to
// any rclone remote.
type RemoteCheck struct {
	Runner sysexec.Runner // runs rclone, wrapped with AsUser for rclone_user
	Euid   int
	Path   string // root's PATH, which commands run as a user extend
	// LookupUser finds rclone_user. Nil means the system's user database.
	LookupUser func(name string) (sysexec.User, error)
}

// RemoteProblem is a failure explained by an rclone remote that doesn't answer.
type RemoteProblem struct {
	Remote string // such as "my-cloud:"
	Cause  string // rclone's own last line
	Fix    string // what to do, naming the remote and user
}

// Check returns nil if repo isn't an rclone: repository or its remote answers.
func (c RemoteCheck) Check(ctx context.Context, repo config.Repository) *RemoteProblem {
	if !strings.HasPrefix(repo.URL, "rclone:") {
		return nil
	}
	remote, err := dest.RcloneRemote(repo.URL)
	if err != nil {
		return nil // a broken URL is explained by the run's own errors
	}
	r := c.Runner
	if name := repo.RcloneUser; name != "" && name != "root" {
		lookup := c.LookupUser
		if lookup == nil {
			lookup = LookupUser
		}
		u, err := lookup(name)
		if err != nil {
			return &RemoteProblem{
				Remote: remote,
				Cause:  fmt.Sprintf("repository.rclone_user %q isn't a user on this machine", name),
				Fix:    "Set repository.rclone_user to the user whose rclone login holds the remote.",
			}
		}
		r = sysexec.AsUser(c.Runner, u, c.Euid == 0, c.Path)
	}
	ctx, cancel := context.WithTimeout(ctx, RemoteTimeout)
	defer cancel()
	_, err = r.Output(ctx, "rclone", "lsf", "--max-depth", "1", remote)
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &RemoteProblem{Remote: remote, Cause: "rclone isn't installed", Fix: "Install it (on Arch: sudo pacman -S rclone)."}
	}
	stderr := err.Error()
	var ee *sysexec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(ee.Stderr) != "" {
		stderr = ee.Stderr
	}
	return &RemoteProblem{Remote: remote, Cause: lastLines(stderr, 1), Fix: remoteFix(repo, remote, stderr)}
}

// remoteFix says how to get an rclone remote answering again, with the engine's wording for an
// expired login and the destination check's otherwise.
func remoteFix(repo config.Repository, remote, stderr string) string {
	if p := engine.Classify(engine.ExitFatal, stderr, repo); strings.Contains(p.Fix, "rclone reconnect") {
		return sentence(p.Fix)
	}
	as := "as yourself"
	if repo.RcloneUser != "" {
		as = "as " + repo.RcloneUser + ", not root"
	}
	return fmt.Sprintf("Check you're online. If the login expired, run (%s): rclone reconnect %s", as, remote)
}

// sentence starts s with a capital letter.
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// Failure builds the message for a failed unit: what failed, its last error lines (or, for an
// rclone repository whose remote doesn't answer, which remote and how to reconnect it) and where
// to read more. Problems reading the errors are reported through warn, which may be nil; the
// message is still sent.
func Failure(ctx context.Context, unit, host string, repo config.Repository, src ErrorSource, remote *RemoteCheck, warn func(string, ...any)) Message {
	m := Message{Title: "Backup failed", Urgency: Critical, Unit: unit, Host: host, Time: time.Now()}
	if strings.Contains(unit, "verify") {
		m.Title = "Backup check found problems"
	}
	if remote != nil {
		if p := remote.Check(ctx, repo); p != nil {
			m.Title = "Backups can't reach " + p.Remote
			m.Body = fmt.Sprintf("The rclone remote %s didn't answer: %s\n%s\n%s", p.Remote, p.Cause, p.Fix, Details)
			return m
		}
	}
	var text string
	if src != nil {
		var err error
		if text, err = src.LastError(ctx, unit); err != nil && warn != nil {
			warn("%v", err)
		}
	}
	m.Body = Details
	if text != "" {
		m.Body = truncate(text, maxErrorText) + "\n" + Details
	}
	return m
}

// truncate shortens s to at most n bytes, at a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// Test is the message `carryon notify --test` sends.
func Test(host string) Message {
	return Message{
		Title: "Carryon test notification",
		Body:  "If you can read this, Carryon can reach you here when a backup fails.",
		Host:  host,
		Time:  time.Now(),
	}
}

// Channels returns a sender for every channel cfg turns on. sys holds what the desktop sender and
// the redaction need from the system.
func Channels(cfg config.Config, sys System) []Sender {
	var senders []Sender
	if cfg.Notify.Desktop {
		senders = append(senders, Desktop{
			Runner: sys.Runner, Users: cfg.Backup.Users, Euid: sys.Euid, Path: sys.Path,
			LookupUser: sys.LookupUser, Settings: sys.Settings, AgentRuns: sys.AgentRuns, Warn: sys.Warn,
		})
	}
	red := Redactor{Secrets: sys.Secrets}
	if cfg.Notify.NtfyURL != "" {
		senders = append(senders, Ntfy{URL: cfg.Notify.NtfyURL, Redact: red, Client: sys.Client})
	}
	if cfg.Notify.WebhookURL != "" {
		senders = append(senders, Webhook{URL: cfg.Notify.WebhookURL, Redact: red, Client: sys.Client})
	}
	return senders
}
