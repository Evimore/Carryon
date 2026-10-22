package restore

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/sysexec"
)

// Packages is what the plan needs from the package manager. pkgmgr.Manager provides it.
type Packages interface {
	// Installed lists every installed package's name.
	Installed(ctx context.Context) ([]string, error)
	// SyncPackages lists the packages the configured repositories offer.
	SyncPackages(ctx context.Context) ([]string, error)
	// ConfiguredRepos lists the configured repositories.
	ConfiguredRepos(ctx context.Context) ([]string, error)
	// Packages lists the packages installed on purpose, then the ones installed as dependencies
	// that are only optionally needed.
	Packages(ctx context.Context) ([]pkgmgr.Package, error)
	// RemoveCommand is the command that removes packages by hand, program first, such as
	// pacman -Rns. The plan only shows it.
	RemoveCommand(pkgs []string) []string
}

// Machine is this machine, the one restored onto. The plan only reads it.
type Machine struct {
	// Root is where its files are read from: "/" on a real machine, a fake tree in tests.
	Root string
	// Hostname is shown in the plan's header.
	Hostname string
	// Facts are its machine facts (facts.Collect under Root): whether it's the machine the backup
	// is of, and its root filesystem. Zero means unknown, which is never the same machine.
	Facts rules.Facts
	// Run runs programs (systemctl, flatpak, npm) as root, or as whoever runs Carryon.
	Run sysexec.Runner
	// UserRunner returns a runner whose programs run as the given user (cargo, pipx, Flatpak
	// apps of their own). Nil means Run's own AsUser method when it has one, else sysexec.AsUser.
	UserRunner func(sysexec.User) sysexec.Runner
	// IsRoot says Carryon runs as root, for sysexec.AsUser.
	IsRoot bool
	// Packages is the package manager.
	Packages Packages
	// Users are the accounts here (ReadUsers). Nil means reading them under Root.
	Users []LocalUser
}

// path turns an absolute system path into one under the root.
func (m *Machine) path(abs string) string {
	return filepath.Join(m.root(), strings.TrimPrefix(abs, "/"))
}

func (m *Machine) root() string {
	if m.Root == "" {
		return "/"
	}
	return m.Root
}

// lexists reports whether something (even a broken symlink) is at the system path.
func (m *Machine) lexists(abs string) bool {
	_, err := os.Lstat(m.path(abs))
	return err == nil
}

func (m *Machine) isDir(abs string) bool {
	fi, err := os.Stat(m.path(abs))
	return err == nil && fi.IsDir()
}

// readlink returns the target of the symlink at the system path, and false if it isn't one.
func (m *Machine) readlink(abs string) (string, bool) {
	fi, err := os.Lstat(m.path(abs))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	t, err := os.Readlink(m.path(abs))
	return t, err == nil
}

// isSymlink reports whether the system path is a symlink.
func (m *Machine) isSymlink(abs string) bool {
	_, ok := m.readlink(abs)
	return ok
}

// kernelParams returns the parameters of the persistent kernel command line, read like capture
// reads them (the first bootloader config that exists, else the running kernel's).
func (m *Machine) kernelParams() map[string]bool {
	out := map[string]bool{}
	for _, s := range capture.CmdlineSources {
		b, err := os.ReadFile(m.path(s.Path))
		if err != nil {
			continue
		}
		for _, mm := range s.Pattern.FindAllStringSubmatch(string(b), -1) {
			for _, p := range strings.Fields(mm[2]) {
				out[p] = true
			}
		}
		return out
	}
	b, _ := os.ReadFile(m.path("/proc/cmdline"))
	for _, p := range strings.Fields(string(b)) {
		out[p] = true
	}
	return out
}

// unitState returns what `systemctl is-enabled` says about a system unit: enabled, masked...
func (m *Machine) unitState(ctx context.Context, unit string) string {
	out, _ := m.Run.Output(ctx, "systemctl", "is-enabled", "--", unit)
	return strings.TrimSpace(string(out))
}

func (m *Machine) userRunner(u LocalUser) sysexec.Runner {
	su := sysexec.User{Name: u.Name, UID: u.UID, Home: u.Home}
	if m.UserRunner != nil {
		return m.UserRunner(su)
	}
	if r, ok := m.Run.(interface {
		AsUser(sysexec.User) sysexec.Runner
	}); ok {
		return r.AsUser(su)
	}
	return sysexec.AsUser(m.Run, su, m.IsRoot, os.Getenv("PATH"))
}

var cargoLine = regexp.MustCompile(`^(\S+) v\S+`)

// toolKey identifies an installed language tool: its type and id, and the user for per-user ones.
func toolKey(typ, id, user string) string { return typ + ":" + id + "@" + user }

// tools lists the language tools installed here, as capture finds them: global npm packages and
// system Flatpak apps, then each given user's cargo and pipx installs and Flatpak apps. A tool
// that isn't installed or fails lists nothing.
func (m *Machine) tools(ctx context.Context, users []LocalUser) map[string]bool {
	have := map[string]bool{}
	r := m.Run
	if r.Exists("npm") {
		out, _ := r.Output(ctx, "npm", "ls", "-g", "--depth=0", "--parseable")
		lines := nonEmptyLines(out)
		if len(lines) > 0 {
			lines = lines[1:] // the global folder itself
		}
		for _, l := range lines {
			if i := strings.LastIndex(l, "/node_modules/"); i >= 0 {
				l = l[i+len("/node_modules/"):]
			}
			have[toolKey("npm", l, "")] = true
		}
	}
	flatpaks := func(r sysexec.Runner, which, user string) {
		out, _ := r.Output(ctx, "flatpak", "list", "--app", which, "--columns=application,origin")
		for _, l := range nonEmptyLines(out) {
			have[toolKey("flatpak", strings.Split(l, "\t")[0], user)] = true
		}
	}
	if r.Exists("flatpak") {
		flatpaks(r, "--system", "")
	}
	for _, u := range users {
		ur := m.userRunner(u)
		if m.userHas(u, ".cargo/bin/cargo", "cargo") {
			out, _ := ur.Output(ctx, "cargo", "install", "--list")
			for _, l := range nonEmptyLines(out) {
				if mm := cargoLine.FindStringSubmatch(l); mm != nil {
					have[toolKey("cargo", mm[1], u.Name)] = true
				}
			}
		}
		if m.userHas(u, ".local/bin/pipx", "pipx") {
			out, _ := ur.Output(ctx, "pipx", "list", "--short")
			for _, l := range nonEmptyLines(out) {
				have[toolKey("pipx", strings.Fields(l)[0], u.Name)] = true
			}
		}
		if r.Exists("flatpak") {
			flatpaks(ur, "--user", u.Name)
		}
	}
	return have
}

// userHas reports whether a program is on root's PATH or in the user's own bin folder.
func (m *Machine) userHas(u LocalUser, inHome, name string) bool {
	if m.Run.Exists(name) {
		return true
	}
	fi, err := os.Stat(m.path(filepath.Join(u.Home, inHome)))
	return err == nil && fi.Mode().IsRegular()
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out
}
