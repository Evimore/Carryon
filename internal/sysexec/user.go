package sysexec

import (
	"context"
	"fmt"
	"strconv"
)

// User is the person whose programs a runner from AsUser runs.
type User struct {
	Name string // login name, such as "alice"
	UID  int
	Home string // home folder, such as "/home/alice"
}

// AsUser wraps r so commands run as u, like the prototype's user_cmd. When root is true (Carryon
// runs as root) and u isn't root, every command becomes
//
//	runuser -u NAME -- env HOME=... USER=... XDG_RUNTIME_DIR=/run/user/UID
//	    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/UID/bus PATH=HOME/.cargo/bin:HOME/.local/bin:PATH cmd...
//
// so the program sees the user's home, settings and session bus. path is the PATH to extend,
// normally root's own. Otherwise commands run unchanged through r.
//
// Exists asks r, so it answers for root's PATH, as the prototype's have did.
func AsUser(r Runner, u User, root bool, path string) Runner {
	if !root || u.UID == 0 {
		return r
	}
	return &userRunner{inner: r, user: u, path: path}
}

type userRunner struct {
	inner Runner
	user  User
	path  string
}

func (r *userRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	argv, err := r.argv(name, args)
	if err != nil {
		return nil, err
	}
	return r.inner.Output(ctx, argv[0], argv[1:]...)
}

func (r *userRunner) Run(ctx context.Context, name string, args ...string) error {
	argv, err := r.argv(name, args)
	if err != nil {
		return err
	}
	return r.inner.Run(ctx, argv[0], argv[1:]...)
}

func (r *userRunner) Exists(name string) bool { return r.inner.Exists(name) }

// argv builds the runuser command line for name and args.
func (r *userRunner) argv(name string, args []string) ([]string, error) {
	u := r.user
	if u.Name == "" || u.Home == "" {
		return nil, fmt.Errorf("can't run %s as user %d: their name or home folder is unknown", name, u.UID)
	}
	path := r.path
	if path == "" {
		path = "/usr/bin"
	}
	rt := "/run/user/" + strconv.Itoa(u.UID)
	argv := []string{
		"runuser", "-u", u.Name, "--", "env",
		"HOME=" + u.Home,
		"USER=" + u.Name,
		"XDG_RUNTIME_DIR=" + rt,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + rt + "/bus",
		"PATH=" + u.Home + "/.cargo/bin:" + u.Home + "/.local/bin:" + path,
		name,
	}
	return append(argv, args...), nil
}
