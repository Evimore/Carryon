package engine

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
)

// The rclone shim. restic runs as root, so it can read every file it backs up, but rclone must use
// the login in rclone_user's own rclone config. For rclone: repositories the engine sets restic's
// rclone.program to "carryon rclone-shim"; restic then starts
//
//	carryon rclone-shim serve restic --stdio --b2-hard-delete REMOTE:PATH
//
// and talks to it over stdin and stdout. The shim replaces its own process with rclone, run as the
// user, so restic's pipes reach rclone untouched. It never reads Carryon's config: the engine passes
// the user in CARRYON_RCLONE_USER, so a missing or broken config can't stop rclone from starting.
// The shim must never write to stdout, which belongs to restic's protocol.

// ShimExec is what the rclone shim runs: argv[0] is a name, Path the program.
type ShimExec struct {
	Path string
	Argv []string
	Env  []string
}

// ShimLookups are the system lookups the shim needs, replaceable in tests.
type ShimLookups struct {
	Euid     int
	LookPath func(name string) (string, error)
	HomeOf   func(username string) (string, error)
}

// SystemLookups are the real lookups.
func SystemLookups() ShimLookups {
	return ShimLookups{
		Euid:     os.Geteuid(),
		LookPath: exec.LookPath,
		HomeOf: func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		},
	}
}

// Variables that point at root's own folders or secrets, dropped before rclone runs as the user.
var rootOnlyEnv = []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "RCLONE_CONFIG", RcloneUserEnv}

// ShimCommand decides how the shim runs rclone with args: as rclone_user through runuser when the
// shim runs as root and a user is set in env, plain rclone otherwise.
func ShimCommand(env, args []string, sys ShimLookups) (ShimExec, error) {
	rclone, err := sys.LookPath("rclone")
	if err != nil {
		return ShimExec{}, fmt.Errorf("rclone isn't installed (on Arch: sudo pacman -S rclone): %w", err)
	}
	name := lookupVar(env, RcloneUserEnv)
	if sys.Euid != 0 || name == "" || name == "root" {
		return ShimExec{Path: rclone, Argv: append([]string{"rclone"}, args...), Env: withoutVars(env, RcloneUserEnv)}, nil
	}
	home, err := sys.HomeOf(name)
	if err != nil || home == "" {
		return ShimExec{}, fmt.Errorf("rclone_user %q isn't a user on this machine: set repository.rclone_user to the user whose rclone login holds the remote", name)
	}
	runuser, err := sys.LookPath("runuser")
	if err != nil {
		return ShimExec{}, fmt.Errorf("runuser (from util-linux) is needed to run rclone as %s: %w", name, err)
	}
	envProg, err := sys.LookPath("env")
	if err != nil {
		return ShimExec{}, fmt.Errorf("env (from coreutils) is needed to run rclone as %s: %w", name, err)
	}
	// rclone runs as the user, who can read its environment: keep restic's settings out of it.
	userEnv := withoutVars(env, rootOnlyEnv...)
	userEnv = withoutPrefix(userEnv, "RESTIC_")
	argv := append([]string{"runuser", "-u", name, "--", envProg, "HOME=" + home, rclone}, args...)
	return ShimExec{Path: runuser, Argv: argv, Env: userEnv}, nil
}

// execFn replaces the process; tests swap it out.
var execFn = syscall.Exec

// RunShim runs rclone in place of the current process, as ShimCommand decides. It only returns on
// failure.
func RunShim(args []string) error {
	c, err := ShimCommand(os.Environ(), args, SystemLookups())
	if err != nil {
		return err
	}
	if err := execFn(c.Path, c.Argv, c.Env); err != nil {
		return fmt.Errorf("starting %s: %w", c.Path, err)
	}
	return nil
}

func withoutPrefix(env []string, prefix string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
