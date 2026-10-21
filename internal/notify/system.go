package notify

import (
	"context"
	"net/http"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/sysexec"
)

// System is what notifications need from the machine. Tests replace each part.
type System struct {
	Runner sysexec.Runner
	Euid   int
	Path   string // root's PATH, which commands run as a user extend
	// LookupUser and Settings default to LookupUser and UserSettings when nil.
	LookupUser func(name string) (sysexec.User, error)
	Settings   func(u sysexec.User) (config.UserNotify, error)
	// AgentRuns, when set, says whose agent shows the message itself; see Desktop.AgentRuns.
	AgentRuns func(ctx context.Context, u sysexec.User) bool
	Client    *http.Client // nil means a client with HTTPTimeout
	Secrets   []string     // exact strings never to send, such as the repository password
	Warn      func(format string, a ...any)
}
