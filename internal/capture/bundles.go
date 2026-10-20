package capture

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/sysexec"
)

// group puts the items into bundles and sorts them in restore order.
func (c *capturer) group(ctx context.Context) {
	sortItems(c.items) // grouping doesn't depend on the order items were found in
	m := &bundleMachine{root: c.root, readFile: c.readFile, pkgs: c.opt.Packages, warn: c.warn, sys: facts.New(c.root, c.opt.Runner)}
	items, bundles := bundle.Assign(ctx, bundle.Input{
		Items:              c.items,
		Rules:              c.opt.BundleRules,
		Host:               c.opt.Facts.Hostname(),
		RepoOf:             c.res.RepoOf,
		FingerprintReaders: c.res.FingerprintReaders,
		BluetoothMACs:      c.res.BluetoothMACs,
		RecordDir:          c.stage,
		Machine:            m,
	})
	bundle.SortItems(items)
	c.items, c.res.Bundles = items, bundles
}

// NewBundleMachine answers grouping's questions from the machine at root, the way capture does.
// warn receives what it couldn't find out; nil drops it.
func NewBundleMachine(root string, run sysexec.Runner, pkgs pkgmgr.Manager, warn func(format string, a ...any)) bundle.Machine {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	return &bundleMachine{root: root, readFile: os.ReadFile, pkgs: pkgs, warn: warn, sys: facts.New(root, run)}
}

// programDirs is where autostart programs are looked for, like root's PATH under systemd.
var programDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/bin", "/usr/sbin", "/bin", "/sbin"}

// bundleMachine answers grouping's questions from the machine being captured.
type bundleMachine struct {
	root     string
	readFile func(path string) ([]byte, error)
	pkgs     pkgmgr.Manager
	warn     func(format string, a ...any)
	sys      *facts.System
}

func (m *bundleMachine) path(p string) string { return filepath.Join(m.root, p) }

func (m *bundleMachine) ReadFile(p string) ([]byte, error) { return m.readFile(m.path(p)) }

func (m *bundleMachine) LookPath(prog string) string {
	for _, dir := range programDirs {
		p := path.Join(dir, prog)
		if fi, err := os.Stat(m.path(p)); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (m *bundleMachine) Owners(ctx context.Context, paths []string) map[string]string {
	owners, err := m.pkgs.Owners(ctx, paths)
	if err != nil {
		m.warn("can't tell which packages own %v, so their autostart entries are grouped with other settings: %v", paths, err)
	}
	return owners
}

func (m *bundleMachine) FilesOf(ctx context.Context, pkgs []string) []string {
	files, err := m.pkgs.FilesOf(ctx, pkgs...)
	if err != nil {
		m.warn("can't list the files of %v, so their bundle starts ticked on every machine: %v", pkgs, err)
	}
	var out []string
	for _, p := range pkgs {
		out = append(out, files[p]...)
	}
	return out
}
func (m *bundleMachine) ModuleExists(ctx context.Context, mod string) bool {
	return m.sys.ModuleExists(ctx, mod)
}
func (m *bundleMachine) LoadedModules() []string { return m.sys.LoadedModules() }
func (m *bundleMachine) ModuleFirmware(ctx context.Context, mod string) []string {
	return m.sys.ModuleFirmware(ctx, mod)
}
func (m *bundleMachine) ModuleHolders(mod string) []string    { return m.sys.ModuleHolders(mod) }
func (m *bundleMachine) DevicesForModule(mod string) []string { return m.sys.DevicesForModule(mod) }
func (m *bundleMachine) DeviceName(ctx context.Context, id string) string {
	return m.sys.DeviceName(ctx, id)
}
func (m *bundleMachine) USBDevices() []string { return m.sys.USBDevices() }
