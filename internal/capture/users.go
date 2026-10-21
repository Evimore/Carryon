package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
)

// User is a user whose home, settings and units are captured.
type User struct {
	Name  string
	UID   int
	GID   int
	Home  string // absolute, on the real system
	Shell string
}

// FirstHumanUID is the lowest user ID given to people rather than system services.
const FirstHumanUID = 1000

// nobodyUID is the overflow user, which isn't a person whatever its shell says.
const nobodyUID = 65534

// group is one line of /etc/group.
type group struct {
	Name    string
	GID     int
	Members []string
}

// Accounts reads every account in <root>/etc/passwd, in file order.
func Accounts(root string) ([]User, error) { return readPasswd(root) }

// readPasswd reads <root>/etc/passwd in file order. Malformed lines are skipped.
func readPasswd(root string) ([]User, error) {
	data, err := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if err != nil {
		return nil, fmt.Errorf("reading the user list: %w", err)
	}
	var out []User
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 || f[0] == "" || strings.HasPrefix(f[0], "#") {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, User{Name: f[0], UID: uid, GID: gid, Home: f[5], Shell: f[6]})
	}
	return out, nil
}

// readGroups reads <root>/etc/group in file order. A missing file has no groups.
func readGroups(root string) ([]group, error) {
	data, err := os.ReadFile(filepath.Join(root, "etc/group"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the group list: %w", err)
	}
	var out []group
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] == "" || strings.HasPrefix(f[0], "#") {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		var members []string
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				members = append(members, m)
			}
		}
		out = append(out, group{Name: f[0], GID: gid, Members: members})
	}
	return out, nil
}

// loginShell reports whether shell lets someone log in.
func loginShell(shell string) bool {
	base := filepath.Base(shell)
	return shell != "" && base != "nologin" && base != "false" && base != "sync" && base != "shutdown" && base != "halt"
}

// human reports whether an account belongs to a person: user ID 1000 and up, with a login shell.
func human(a User) bool {
	return a.UID >= FirstHumanUID && a.UID != nobodyUID && loginShell(a.Shell)
}

// SelectUsers returns the users to capture, from <root>/etc/passwd: every human user (user ID 1000
// and up with a login shell) when names holds "*", else the named users, in that order. Named
// users that don't exist are left out, with a warning each.
func SelectUsers(root string, names []string) ([]User, []string, error) {
	accounts, err := readPasswd(root)
	if err != nil {
		return nil, nil, err
	}
	var users []User
	var warnings []string
	seen := map[string]bool{}
	addUser := func(a User) {
		if !seen[a.Name] && a.Home != "" && filepath.IsAbs(a.Home) {
			seen[a.Name] = true
			users = append(users, a)
		}
	}
	if slices.Contains(names, "*") {
		for _, a := range accounts {
			if human(a) {
				addUser(a)
			}
		}
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "*" || seen[n] {
			continue
		}
		i := slices.IndexFunc(accounts, func(a User) bool { return a.Name == n })
		switch {
		case i < 0:
			warnings = append(warnings, fmt.Sprintf("backup.users names %q, but there is no such user here: leaving them out", n))
		case accounts[i].UID == 0:
			warnings = append(warnings, "backup.users names root: root's home is backed up as /root, so it's left out of the users")
		default:
			addUser(accounts[i])
		}
	}
	return users, warnings, nil
}

// idNames turns numeric owners into names, from the passwd and group files under the root.
type idNames struct {
	users  map[int]string
	groups map[int]string
	list   []group
}

func readIDNames(root string) (*idNames, error) {
	accounts, err := readPasswd(root)
	if err != nil {
		return nil, err
	}
	groups, err := readGroups(root)
	if err != nil {
		return nil, err
	}
	n := &idNames{users: map[int]string{}, groups: map[int]string{}, list: groups}
	for _, a := range accounts {
		if _, ok := n.users[a.UID]; !ok {
			n.users[a.UID] = a.Name
		}
	}
	for _, g := range groups {
		if _, ok := n.groups[g.GID]; !ok {
			n.groups[g.GID] = g.Name
		}
	}
	return n, nil
}

// owner is "user:group", with numbers where there is no name, like the prototype's _uname/_gname.
func (n *idNames) owner(uid, gid int) string {
	u, ok := n.users[uid]
	if !ok {
		u = strconv.Itoa(uid)
	}
	g, ok := n.groups[gid]
	if !ok {
		g = strconv.Itoa(gid)
	}
	return u + ":" + g
}

// captureAccounts records each user's supplementary groups and login shell. Passwords are never
// read. Ported from capture_account.
func (c *capturer) captureAccounts() {
	for _, u := range c.res.Users {
		for _, g := range c.ids.list {
			if slices.Contains(g.Members, u.Name) {
				c.add(record.Item{Type: record.TypeGroup, ID: g.Name, User: u.Name})
			}
		}
		if u.Shell != "" {
			c.add(record.Item{Type: record.TypeShell, ID: u.Shell, User: u.Name})
		}
	}
}

// userRunner returns a runner for u's programs.
func (c *capturer) userRunner(u User) sysexec.Runner {
	su := sysexec.User{Name: u.Name, UID: u.UID, Home: u.Home}
	if c.opt.UserRunner != nil {
		return c.opt.UserRunner(su)
	}
	if r, ok := c.opt.Runner.(interface {
		AsUser(sysexec.User) sysexec.Runner
	}); ok {
		return r.AsUser(su)
	}
	return sysexec.AsUser(c.opt.Runner, su, c.opt.IsRoot, os.Getenv("PATH"))
}

// userOf returns the user whose home holds path, and path relative to that home.
func (c *capturer) userOf(path string) (User, string, bool) {
	for _, u := range c.res.Users {
		if rel, ok := strings.CutPrefix(path, strings.TrimRight(u.Home, "/")+"/"); ok && rel != "" {
			return u, rel, true
		}
	}
	return User{}, "", false
}
