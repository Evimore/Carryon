// Package facts describes the machine Carryon runs on: its hardware fingerprint, chassis, devices,
// kernel modules, root filesystem and bootloader. Restore uses these facts to decide which bundles
// start ticked (see rules.Facts), and bundling uses the device helpers to write when-rules.
//
// Everything is read relative to a root folder, "/" on a real machine and a fake tree in tests.
// Files are preferred over programs; programs only fill gaps, and run through a Runner so tests can
// fake them.
package facts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Evimore/carryon/internal/rules"
)

// Runner runs external programs. Its methods match internal/sysexec's runner, so that one plugs in.
type Runner interface {
	// Output runs a program and returns what it printed on standard output. The output is used
	// even when err is set, like the prototype's sh().
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	// Exists reports whether a program is installed.
	Exists(name string) bool
}

// System reads facts about one machine.
type System struct {
	Root string // "/" on a real machine
	Run  Runner // may be nil: then only files are read
}

// New returns a System reading from root and running programs with run.
func New(root string, run Runner) *System {
	if root == "" {
		root = "/"
	}
	return &System{Root: root, Run: run}
}

// Collect reads all the facts about the machine at root. Missing pieces are left empty; it only
// fails when root doesn't look like a running Linux system.
func Collect(ctx context.Context, root string, run Runner) (rules.Facts, error) {
	return New(root, run).Facts(ctx)
}

// Facts reads all the facts about the machine. See Collect.
func (s *System) Facts(ctx context.Context) (rules.Facts, error) {
	if _, err := os.Stat(s.path("proc/self/mountinfo")); err != nil {
		return rules.Facts{}, fmt.Errorf("reading machine facts: %s doesn't look like a running Linux system: %w", s.Root, err)
	}
	ids, err := s.HardwareIDs(ctx)
	if err != nil {
		return rules.Facts{}, err
	}
	return rules.Facts{
		Host:       s.Hostname(),
		Machine:    s.Fingerprint(),
		Chassis:    s.Chassis(ctx),
		RootFS:     s.RootFS(),
		Bootloader: s.Bootloader(ctx),
		IDs:        ids,
	}, nil
}

// Hostname returns the kernel's hostname, or /etc/hostname when /proc doesn't have it.
func (s *System) Hostname() string {
	for _, p := range []string{"proc/sys/kernel/hostname", "etc/hostname"} {
		if v := s.readTrim(p); v != "" {
			return v
		}
	}
	return ""
}

// path turns an absolute system path ("/sys/...", or "sys/...") into one under the root.
func (s *System) path(p string) string {
	return filepath.Join(s.Root, strings.TrimPrefix(p, "/"))
}

// readTrim reads a file under the root with surrounding whitespace stripped, or "" if it can't.
func (s *System) readTrim(p string) string {
	b, err := os.ReadFile(s.path(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// output runs a program if a runner is set and the program exists, and returns its output, even
// when the program failed.
func (s *System) output(ctx context.Context, name string, args ...string) string {
	if s.Run == nil || !s.Run.Exists(name) {
		return ""
	}
	out, _ := s.Run.Output(ctx, name, args...)
	return string(out)
}
