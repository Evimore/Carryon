package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Evimore/carryon/internal/sysexec"
)

// commLen is how much of a program's name the kernel keeps in /proc/PID/comm.
const commLen = 15

// Apps finds out whether the user runs an app, by its package name. It looks only at the user's
// own processes, which the agent may read since it runs as the user.
type Apps struct {
	// Runner runs pacman -Qlq, to learn which programs the package installs. Nil looks for the
	// package's name only.
	Runner sysexec.Runner
	Proc   string // "/proc", or a fake one in tests
	UID    int
}

// Running reports whether one of the user's processes runs a program of app: one the package
// installs in a bin folder, or one named like the package.
func (a Apps) Running(ctx context.Context, app string) bool {
	if app == "" {
		return false
	}
	programs := map[string]bool{}
	names := map[string]bool{app: true}
	if a.Runner != nil {
		if out, err := a.Runner.Output(ctx, "pacman", "-Qlq", "--", app); err == nil {
			for _, f := range strings.Split(string(out), "\n") {
				f = strings.TrimSpace(f)
				if strings.HasSuffix(f, "/") || !strings.Contains(f, "/bin/") {
					continue
				}
				programs[f] = true
				names[filepath.Base(f)] = true
			}
		}
	}
	short := map[string]bool{}
	for n := range names {
		if len(n) > commLen {
			n = n[:commLen]
		}
		short[n] = true
	}

	proc := a.Proc
	if proc == "" {
		proc = "/proc"
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := filepath.Join(proc, e.Name())
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != a.UID {
			continue
		}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			exe = strings.TrimSuffix(exe, " (deleted)") // the package was just upgraded
			if programs[exe] || names[filepath.Base(exe)] {
				return true
			}
		}
		if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil && short[strings.TrimSpace(string(comm))] {
			return true
		}
		// A script runs as its interpreter: look at the first two words of its command line.
		if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			args := strings.Split(string(cmdline), "\x00")
			for _, arg := range args[:min(2, len(args))] {
				if arg != "" && (programs[arg] || names[filepath.Base(arg)]) {
					return true
				}
			}
		}
	}
	return false
}
