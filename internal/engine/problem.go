package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/config"
)

// restic's exit codes (restic 0.17 and newer; see "Exit codes" in restic's scripting docs). New codes
// may appear; any code other than 0 is a failure.
const (
	ExitOK          = 0
	ExitFatal       = 1
	ExitGoRuntime   = 2
	ExitPartial     = 3 // backup couldn't read some files; forget couldn't remove some snapshots
	ExitNoRepo      = 10
	ExitLockFailed  = 11
	ExitBadPassword = 12 // since restic 0.17.1; 0.17.0 returns 1
	ExitInterrupted = 130
)

// Problem is what went wrong, in plain words, and what to do about it.
type Problem struct {
	Cause string // restic's own line that says what went wrong, or a plain description
	Fix   string // what the user can do
}

func (p Problem) String() string {
	if p.Fix == "" {
		return p.Cause
	}
	return p.Cause + "\n   " + p.Fix
}

// Error is a failed restic run.
type Error struct {
	Problem
	Code int      // restic's exit code, -1 if it couldn't be started
	Args []string // the restic command, without the engine's default options
}

func (e *Error) Error() string { return e.Problem.String() }

var (
	causeRE    = regexp.MustCompile(`(?i)fatal|error|unable|fail|denied|wrong password|locked|no space|could not`)
	fatalRE    = regexp.MustCompile(`^(?i)fatal:\s*`)
	authRE     = regexp.MustCompile(`(?i)unauthori[sz]ed|\b401\b|\b403\b|forbidden|invalid_grant|token (has )?expired|expired token|reauth|re-auth|sign(ed)? ?in again|login (required|expired)|couldn't fetch token|trust token|authentication failed|access denied`)
	networkRE  = regexp.MustCompile(`(?i)no such host|network is unreachable|connection refused|connection reset|i/o timeout|no route to host|temporary failure in name resolution|tls handshake timeout|dial tcp|server misbehaving`)
	noRepoRE   = regexp.MustCompile(`(?i)repository does not exist|is there a repository at|config file does not exist|unable to open config file`)
	missingRE  = regexp.MustCompile(`(?i)rclone.*(executable file not found|no such file or directory)|exec: "rclone"`)
	passwordRE = regexp.MustCompile(`(?i)wrong password|no key found`)
	lockedRE   = regexp.MustCompile(`(?i)already locked|unable to create lock|repo already locked`)
)

// Classify explains a failed restic run from its exit code and stderr. repo makes the fix exact
// (the password file to compare, the rclone remote to reconnect).
func Classify(code int, stderr string, repo config.Repository) Problem {
	cause := causeLine(stderr)
	if code == ExitInterrupted {
		return Problem{Cause: orDefault(cause, "restic was interrupted"), Fix: "nothing is lost: the next run starts over"}
	}
	if cause == "" {
		cause = fmt.Sprintf("restic failed without saying why (exit code %d)", code)
	}
	s := stderr
	rclone := strings.HasPrefix(repo.URL, "rclone:")
	switch {
	case code == ExitBadPassword || passwordRE.MatchString(s):
		where := "the password command (repository.password_command)"
		if repo.PasswordCommand == "" {
			where = orDefault(repo.PasswordFile, "the password file")
		}
		return Problem{Cause: cause, Fix: fmt.Sprintf("the backup password doesn't match this repository: compare %s with the copy in your password manager or recovery kit", where)}
	case code == ExitLockFailed || lockedRE.MatchString(s):
		return Problem{Cause: cause, Fix: "another backup or check is still using the repository; wait for it to finish. If none is running, remove the stale lock with: sudo carryon restic unlock"}
	case rclone && missingRE.MatchString(s):
		return Problem{Cause: cause, Fix: "rclone isn't installed: install it (on Arch: sudo pacman -S rclone)"}
	case rclone && authRE.MatchString(s):
		return Problem{Cause: cause, Fix: reconnectFix(repo)}
	case strings.Contains(strings.ToLower(s), "no space left on device"):
		return Problem{Cause: cause, Fix: "a disk is full: free up space on the backup destination, or on the system disk, where restic keeps its cache"}
	case code == ExitNoRepo || noRepoRE.MatchString(s):
		return Problem{Cause: cause, Fix: fmt.Sprintf("there's no backup repository at %s: if it's on a removable drive, plug it in and mount it; otherwise check repository.url in the config", orDefault(repo.URL, "the configured location"))}
	case networkRE.MatchString(s):
		return Problem{Cause: cause, Fix: "the backup destination can't be reached: check the network connection; the backup runs again once it's back"}
	case rclone && strings.Contains(strings.ToLower(s), "rclone"):
		return Problem{Cause: cause, Fix: "rclone failed: check that the remote works, as the user whose login it is, with: rclone lsd " + remoteName(repo.URL) + ":"}
	case code == ExitPartial:
		return Problem{Cause: orDefault(cause, "some files couldn't be read"), Fix: "the snapshot was saved without the files listed above; they may have changed or been removed while the backup ran"}
	}
	return Problem{Cause: cause, Fix: "see restic's full output above or in the journal"}
}

// reconnectFix is the command that renews an rclone login, naming the real remote and user.
func reconnectFix(repo config.Repository) string {
	cmd := "rclone reconnect " + remoteName(repo.URL) + ":"
	if repo.RcloneUser != "" {
		return fmt.Sprintf("the login for %s: may have expired or been revoked: as %s (not root), run: %s", remoteName(repo.URL), repo.RcloneUser, cmd)
	}
	return fmt.Sprintf("the login for %s: may have expired or been revoked: run: %s", remoteName(repo.URL), cmd)
}

// remoteName is the rclone remote of an rclone: repository URL ("rclone:my-cloud:backups" ->
// "my-cloud").
func remoteName(url string) string {
	rest := strings.TrimPrefix(url, "rclone:")
	name, _, _ := strings.Cut(rest, ":")
	return orDefault(name, "REMOTE")
}

// causeLine picks the line of restic's stderr that says what went wrong: the message of a JSON
// exit_error, else the last line that looks like an error, else the last line.
func causeLine(stderr string) string {
	var lines []string
	for _, ln := range strings.Split(stderr, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var msg struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
		}
		if strings.HasPrefix(lines[i], "{") && json.Unmarshal([]byte(lines[i]), &msg) == nil && msg.Type == "exit_error" && msg.Message != "" {
			first, _, _ := strings.Cut(strings.TrimSpace(msg.Message), "\n")
			return fatalRE.ReplaceAllString(strings.TrimSpace(first), "")
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if causeRE.MatchString(lines[i]) {
			return fatalRE.ReplaceAllString(lines[i], "")
		}
	}
	if len(lines) > 0 {
		return fatalRE.ReplaceAllString(lines[len(lines)-1], "")
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// IsNoRepo reports whether a failed restic run found no repository at the URL, as opposed to one
// it couldn't open.
func IsNoRepo(code int, stderr string) bool {
	return code == ExitNoRepo || (code != 0 && noRepoRE.MatchString(stderr) && !passwordRE.MatchString(stderr))
}

// IsBadPassword reports whether a failed restic run found a repository its password doesn't open.
func IsBadPassword(code int, stderr string) bool {
	return code == ExitBadPassword || (code != 0 && passwordRE.MatchString(stderr))
}
