package restore

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
	"github.com/Evimore/carryon/internal/ui"
)

// LocalUser is an account on this machine.
type LocalUser struct {
	Name   string
	UID    int
	GID    int
	Home   string
	Shell  string
	Groups []string // supplementary groups
}

// firstHumanUID is the lowest user ID given to people rather than system services.
const firstHumanUID = 1000

// human reports whether the account belongs to a person: user ID 1000 and up with a login shell.
func (u LocalUser) human() bool {
	base := filepath.Base(u.Shell)
	return u.UID >= firstHumanUID && u.UID != 65534 && u.Shell != "" &&
		base != "nologin" && base != "false"
}

// ReadUsers reads the accounts in <root>/etc/passwd and their groups from <root>/etc/group.
func ReadUsers(root string) ([]LocalUser, error) {
	data, err := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if err != nil {
		return nil, fmt.Errorf("reading the user list: %w", err)
	}
	var users []LocalUser
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
		users = append(users, LocalUser{Name: f[0], UID: uid, GID: gid, Home: f[5], Shell: f[6]})
	}
	data, err = os.ReadFile(filepath.Join(root, "etc/group"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading the group list: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] == "" || strings.HasPrefix(f[0], "#") {
			continue
		}
		for _, m := range strings.Split(f[3], ",") {
			m = strings.TrimSpace(m)
			for i := range users {
				if users[i].Name == m && m != "" {
					users[i].Groups = append(users[i].Groups, f[0])
				}
			}
		}
	}
	return users, nil
}

// UserMap says which user here receives each recorded user's files and settings.
type UserMap struct {
	From record.User
	To   LocalUser
}

// Changed reports whether the user's name or home differs here, so paths and owners need adapting.
func (m UserMap) Changed() bool { return m.From.Name != m.To.Name || m.From.Home != m.To.Home }

// MapOptions say how recorded users are matched to users here.
type MapOptions struct {
	// Given maps recorded user names to users here (restore --user OLD=NEW).
	Given map[string]string
	// Invoking is the user running the restore (SUDO_USER): with one recorded user who has no
	// account here, their files go to this user by default, as in the prototype.
	Invoking string
}

// MapUsers matches each recorded user to a user here: the one named in Given, else the user of
// the same name, else one the user picks.
func MapUsers(meta record.Meta, here []LocalUser, ask ui.Prompter, opt MapOptions) ([]UserMap, error) {
	find := func(name string) (LocalUser, bool) {
		i := slices.IndexFunc(here, func(u LocalUser) bool { return u.Name == name })
		if i < 0 {
			return LocalUser{}, false
		}
		return here[i], true
	}
	var humans []LocalUser
	for _, u := range here {
		if u.human() {
			humans = append(humans, u)
		}
	}
	recorded := meta.AllUsers()
	for old := range opt.Given {
		if !slices.ContainsFunc(recorded, func(u record.User) bool { return u.Name == old }) {
			return nil, fmt.Errorf("--user %s=...: the backup has no user %s", old, old)
		}
	}
	var out []UserMap
	for _, ru := range recorded {
		if name, ok := opt.Given[ru.Name]; ok {
			u, ok := find(name)
			if !ok {
				return nil, fmt.Errorf("--user %s=%s: there is no user %s here: create the account first (sudo useradd -m %s)", ru.Name, name, name, name)
			}
			out = append(out, UserMap{From: ru, To: u})
			continue
		}
		if u, ok := find(ru.Name); ok {
			out = append(out, UserMap{From: ru, To: u})
			continue
		}
		if len(humans) == 0 {
			return nil, fmt.Errorf("%s has no account here, and there is no other user to receive their files: create one first (sudo useradd -m %s)", ru.Name, ru.Name)
		}
		m := ui.Menu{
			Title: fmt.Sprintf("Who gets %s's files?", ru.Name),
			Text:  fmt.Sprintf("%s (%s) has no account here. Choose who receives their files and settings:", ru.Name, ru.Home),
			Flag:  fmt.Sprintf("--user %s=NAME", ru.Name),
		}
		for _, u := range humans {
			m.Options = append(m.Options, ui.Option{Value: u.Name, Note: u.Home})
		}
		switch {
		case len(recorded) == 1 && opt.Invoking != "" && slices.ContainsFunc(humans, func(u LocalUser) bool { return u.Name == opt.Invoking }):
			m.Default = opt.Invoking
		case len(humans) == 1:
			m.Default = humans[0].Name
		}
		name, err := ask.Menu(m)
		if err != nil {
			return nil, err
		}
		u, _ := find(name)
		out = append(out, UserMap{From: ru, To: u})
	}
	return out, nil
}

// ParseUserFlags reads --user OLD=NEW values.
func ParseUserFlags(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range values {
		old, nw, ok := strings.Cut(v, "=")
		if !ok || old == "" || nw == "" {
			return nil, fmt.Errorf("--user %q: write it as OLD=NEW, such as --user alice=bob", v)
		}
		out[old] = nw
	}
	return out, nil
}
