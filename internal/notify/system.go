package notify

import (
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
	Client     *http.Client // nil means a client with HTTPTimeout
	Secrets    []string     // exact strings never to send, such as the repository password
	Warn       func(format string, a ...any)
}
