package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"sort"
	"strconv"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/sysexec"
)

// Desktop is the plain desktop notification root sends when no user agent runs: one notification
// in the session of every user who
//
//   - has a graphical session open (found through loginctl),
//   - is covered by backup.users, and
//   - hasn't switched desktop notifications off in their own settings.
//
// It runs notify-send as that user, through sysexec.AsUser, so the notification goes over the
// user's own session bus; root never connects to a user's bus as root. Without notify-send it calls
// the notification service with busctl instead. When Carryon doesn't run as root it can only
// reach its own user's session, so other users are skipped.
type Desktop struct {
	Runner sysexec.Runner // runs loginctl as root, and is wrapped with AsUser for each user
	Users  []string       // backup.users: names, or "*" for every human user
	Euid   int            // Carryon's effective user ID
	Path   string         // root's PATH, which commands run as a user extend; empty means /usr/bin
	// LookupUser finds a user's home folder. Nil means the system's user database.
	LookupUser func(name string) (sysexec.User, error)
	// Settings reads a user's own notification settings. Nil means UserSettings.
	Settings func(u sysexec.User) (config.UserNotify, error)
	// AgentRuns reports whether u's agent runs and will show this message itself, so it isn't
	// shown twice. Nil means nobody's agent will.
	AgentRuns func(ctx context.Context, u sysexec.User) bool
	// Warn reports a problem that didn't stop the notification (a user it couldn't reach while
	// others got it, a broken settings file). May be nil.
	Warn func(format string, a ...any)
}

// Name is "desktop".
func (d Desktop) Name() string { return "desktop" }

// Session is a login session, as loginctl describes it.
type Session struct {
	ID, Name, Type, Class, State, Remote string
	UID                                  int
}

// Graphical reports whether the session is a local user's open desktop.
func (s Session) Graphical() bool {
	switch s.Type {
	case "x11", "wayland", "mir":
	default:
		return false
	}
	return s.Class == "user" && s.Remote != "yes" && (s.State == "active" || s.State == "online")
}

// Send shows m to every user who should see it. It succeeds when at least one user got it, from
// root or from their agent.
func (d Desktop) Send(ctx context.Context, m Message) (string, error) {
	users, agents, err := d.recipients(ctx)
	if err != nil {
		return "", err
	}
	if len(users) == 0 && len(agents) == 0 {
		return "", errors.New("nobody whose home is backed up has a desktop session open")
	}
	var sent []string
	var errs []error
	for _, u := range users {
		if err := d.sendTo(ctx, u, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.Name, err))
			continue
		}
		sent = append(sent, u.Name)
	}
	for _, u := range agents {
		sent = append(sent, u.Name+" (left to the agent)")
	}
	sort.Strings(sent)
	if len(sent) == 0 {
		return "", errors.Join(errs...)
	}
	for _, err := range errs {
		d.warn("desktop notification not shown to %v", err)
	}
	return strings.Join(sent, ", "), nil
}

// Recipients returns the users who should get a desktop notification from root now, sorted by
// name. Users whose agent shows it instead aren't among them.
func (d Desktop) Recipients(ctx context.Context) ([]sysexec.User, error) {
	users, _, err := d.recipients(ctx)
	return users, err
}

// recipients returns the users root notifies and the users whose agent does, each sorted by name.
func (d Desktop) recipients(ctx context.Context) (users, agents []sysexec.User, err error) {
	sessions, err := Sessions(ctx, d.Runner)
	if err != nil {
		return nil, nil, err
	}
	lookup, settings := d.LookupUser, d.Settings
	if lookup == nil {
		lookup = LookupUser
	}
	if settings == nil {
		settings = UserSettings
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		if !s.Graphical() || seen[s.Name] || !Covered(d.Users, s.Name, s.UID) {
			continue
		}
		if d.Euid != 0 && s.UID != d.Euid {
			continue // only root may run programs as another user
		}
		seen[s.Name] = true
		u, err := lookup(s.Name)
		if err != nil {
			d.warn("not notifying %s: %v", s.Name, err)
			continue
		}
		u.UID = s.UID
		n, err := settings(u)
		if err != nil {
			d.warn("%v (notifying %s anyway)", err, u.Name)
		}
		if !n.Desktop {
			continue
		}
		if d.AgentRuns != nil && d.AgentRuns(ctx, u) {
			agents = append(agents, u)
			continue
		}
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return users, agents, nil
}

// sendTo shows m in u's session with notify-send, or with busctl when notify-send isn't installed.
func (d Desktop) sendTo(ctx context.Context, u sysexec.User, m Message) error {
	r := sysexec.AsUser(d.Runner, u, d.Euid == 0, d.Path)
	urgency, level := "normal", "1"
	if m.Urgency == Critical {
		urgency, level = "critical", "2"
	}
	if d.Runner.Exists("notify-send") {
		_, err := r.Output(ctx, "notify-send", "-u", urgency, "-a", "Carryon", "-i", "dialog-warning", "--", m.Title, m.Body)
		if !errors.Is(err, exec.ErrNotFound) {
			return err
		}
	}
	// Notify(app_name, replaces_id, app_icon, summary, body, actions, hints, expire_timeout)
	_, err := r.Output(ctx, "busctl", "--user", "--", "call",
		"org.freedesktop.Notifications", "/org/freedesktop/Notifications", "org.freedesktop.Notifications",
		"Notify", "susssasa{sv}i", "Carryon", "0", "dialog-warning", m.Title, m.Body, "0", "1", "urgency", "y", level, "-1")
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("neither notify-send (libnotify) nor busctl (systemd) is installed")
	}
	return err
}

func (d Desktop) warn(format string, a ...any) {
	if d.Warn != nil {
		d.Warn(format, a...)
	}
}

// AgentUnit is the user unit that runs carryon agent.
const AgentUnit = "carryon-agent.service"

// AgentActive reports whether u's agent runs, asking u's own service manager as u (systemctl
// --user is-active), so root never connects to a user's bus as root. The answer only ever decides
// whether u's own desktop notification comes from the agent or from root: a user can't make root
// read or do anything more by faking it.
func AgentActive(r sysexec.Runner, euid int, path string) func(ctx context.Context, u sysexec.User) bool {
	return func(ctx context.Context, u sysexec.User) bool {
		if euid != 0 && u.UID != euid {
			return false
		}
		_, err := sysexec.AsUser(r, u, euid == 0, path).Output(ctx, "systemctl", "--user", "is-active", "--quiet", AgentUnit)
		return err == nil
	}
}

// Sessions lists the login sessions on this machine with loginctl.
func Sessions(ctx context.Context, r sysexec.Runner) ([]Session, error) {
	out, err := r.Output(ctx, "loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return nil, fmt.Errorf("listing who is signed in: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 && !strings.HasPrefix(f[0], "-") {
			ids = append(ids, f[0])
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := []string{"show-session", "-p", "Id", "-p", "Name", "-p", "User", "-p", "Type", "-p", "Class", "-p", "State", "-p", "Remote"}
	out, err = r.Output(ctx, "loginctl", append(args, ids...)...)
	if err != nil {
		return nil, fmt.Errorf("reading the login sessions: %w", err)
	}
	return parseSessions(string(out)), nil
}

// parseSessions reads loginctl show-session output: KEY=value lines, one block per session,
// blocks separated by blank lines.
func parseSessions(out string) []Session {
	var sessions []Session
	var cur Session
	have := false
	flush := func() {
		if have && cur.Name != "" {
			sessions = append(sessions, cur)
		}
		cur, have = Session{UID: -1}, false
	}
	cur.UID = -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k == "Id" && cur.ID != "" {
			flush()
		}
		have = true
		switch k {
		case "Id":
			cur.ID = v
		case "Name":
			cur.Name = v
		case "User":
			if uid, err := strconv.Atoi(v); err == nil {
				cur.UID = uid
			}
		case "Type":
			cur.Type = v
		case "Class":
			cur.Class = v
		case "State":
			cur.State = v
		case "Remote":
			cur.Remote = v
		}
	}
	flush()
	return sessions
}

// Covered reports whether backup.users includes the user: by name, or through "*" for every human
// user (user ID 1000 and up, except nobody).
func Covered(users []string, name string, uid int) bool {
	for _, u := range users {
		if u == name || (u == "*" && uid >= 1000 && uid != 65534) {
			return true
		}
	}
	return false
}

// LookupUser finds a user in the system's user database.
func LookupUser(name string) (sysexec.User, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return sysexec.User{}, fmt.Errorf("looking up user %s: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return sysexec.User{}, fmt.Errorf("user %s has a non-numeric user ID %q", name, u.Uid)
	}
	return sysexec.User{Name: u.Username, UID: uid, Home: u.HomeDir}, nil
}

// UserSettings reads u's own notification settings (~/.config/carryon/config.toml). When the file
// can't be read, it returns the defaults (notifications on) with the error, so a broken file
// never silences an alert.
func UserSettings(u sysexec.User) (config.UserNotify, error) {
	cfg, _, err := config.LoadUser(u.Home, u.UID)
	if err != nil {
		return config.DefaultUser().Notify, err
	}
	return cfg.Notify, nil
}
