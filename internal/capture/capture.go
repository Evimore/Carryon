// Package capture builds the system record: it looks at the machine and writes what makes it
// someone's (packages, changed system files, services, users, language tools and backed-up paths)
// into a staged record folder. See docs/design.md, "The system record" and "What gets captured".
//
// Every file is read under a root folder (Options.Root), so tests use a fake system tree, and every
// program runs through a sysexec.Runner, so tests use a fake. Packages come from a pkgmgr.Manager
// and machine facts from internal/facts.
//
// Before writing, capture groups the items into bundles with internal/bundle, applying the user's
// bundles.conf rules (Options.BundleRules), so every caller gets a grouped record. Swapping the
// staged folder into place, SUMMARY.md and the history belong to the record store.
//
// It ports the prototype's capture and the collectors it calls (legacy/sysbackup.py).
package capture

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/version"
)

// Files in a staged record besides record.MetaFile, record.ItemsFile and record.BundlesFile.
const (
	FilesDir      = "files"            // copies of edited and added system files
	UserUnitsDir  = "user-units"       // user-units/<user>/: each user's unit files and drop-ins
	RepoKeysFile  = "repo-keys.asc"    // signing keys of extra repositories
	VersionsFile  = "versions.txt"     // every installed package with its version
	SkippedFile   = "skipped.txt"      // what couldn't be read (capture without root)
	NotBackedUpMD = "not-backed-up.md" // what is left out on purpose (nightly capture only)
)

// MaxFile is the size above which a system file is flagged too_big instead of copied.
const MaxFile = 100 << 20

// Facts is what capture needs to know about the machine. *facts.System provides it.
type Facts interface {
	Hostname() string
	Fingerprint() string
	Chassis(ctx context.Context) string
	FingerprintReaders(ctx context.Context) []string
}

// Logger reports progress and problems that don't stop the capture. *ui.Logger provides it.
type Logger interface {
	Info(format string, a ...any)
	Warn(format string, a ...any)
}

// Options configure a capture.
type Options struct {
	// Dir is the staged record folder to write. It must be empty or not exist yet.
	Dir string
	// Root is where the system's files are read from. Empty means "/". Programs always run on
	// the real system, through Runner.
	Root string
	// Runner runs programs as root (or as whoever runs Carryon).
	Runner sysexec.Runner
	// UserRunner returns a runner whose programs run as the given user. Nil means Runner's own
	// AsUser method when it has one, else sysexec.AsUser.
	UserRunner func(sysexec.User) sysexec.Runner
	// Packages is the package manager backend.
	Packages pkgmgr.Manager
	// Facts reads machine facts. Nil means facts.New(Root, Runner).
	Facts Facts
	// Config supplies backup.users, backup.extra_paths and machine_name.
	Config config.Config
	// Users are the users to capture. Nil means SelectUsers(Root, Config.Backup.Users).
	Users []User
	// Excludes is the effective exclude list (see BuildExcludes): home entries it leaves out
	// aren't recorded.
	Excludes []string
	// Mounts is the mount table. Nil means reading it under Root; mount points in home folders
	// aren't recorded.
	Mounts dest.MountTable
	// Nightly turns on the slow parts the pacman hook's quick capture leaves out: package files
	// edited outside their backup= list (pacman -Qkk) and the not-backed-up list.
	Nightly bool
	// BundleRules are the user's bundles.conf rules. The zero value means none.
	BundleRules rules.BundleRules
	// Hints are the catalog's re-download hints, for the not-backed-up list.
	Hints []catalog.Hint
	// IsRoot says Carryon runs as root. Without root, files root alone can read are skipped and
	// the record is marked incomplete.
	IsRoot bool
	// Log receives progress and warnings. Nil means they are dropped (they're also in Result).
	Log Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Result is what a capture found: the record's contents and what was learned on the way (the
// prototype's ctx in assign_bundles).
type Result struct {
	Dir     string         // the staged record folder
	Meta    record.Meta    // as written to meta.json
	Items   []record.Item  // as written to items.jsonl: grouped, in restore order (bundle.SortItems)
	Bundles record.Bundles // as written to bundles.json, in restore order
	Users   []User         // the users captured, the main one first

	// RepoOf maps each package the configured repositories offer to its repository.
	RepoOf map[string]string
	// Owned holds every path a package owns.
	Owned map[string]bool
	// RepoFiles are the files holding extra repositories' sections (left out of the files).
	RepoFiles []string
	// FingerprintReaders are the USB IDs of the fingerprint readers present ("usb:VVVV:PPPP").
	FingerprintReaders []string
	// BluetoothMACs are the adapters with pairings in /var/lib/bluetooth, lower case.
	BluetoothMACs []string

	// Skipped lists what couldn't be read. When it isn't empty, the record is incomplete.
	Skipped []string
	// Warnings are problems that didn't stop the capture.
	Warnings []string
}

// capturer holds one capture's state.
type capturer struct {
	opt    Options
	root   string
	stage  string
	log    Logger
	ids    *idNames
	res    *Result
	items  []record.Item
	mounts dest.MountTable

	// test hooks
	readFile func(path string) ([]byte, error)
	owner    func(path string, fi fs.FileInfo) (uid, gid int)
}

// Run captures the machine into opt.Dir.
func Run(ctx context.Context, opt Options) (*Result, error) {
	c, err := newCapturer(opt)
	if err != nil {
		return nil, err
	}
	return c.run(ctx)
}

func newCapturer(opt Options) (*capturer, error) {
	if opt.Dir == "" {
		return nil, errors.New("capture: no record folder given")
	}
	if opt.Runner == nil || opt.Packages == nil {
		return nil, errors.New("capture: a runner and a package manager are needed")
	}
	if opt.Root == "" {
		opt.Root = "/"
	}
	if opt.Facts == nil {
		opt.Facts = facts.New(opt.Root, opt.Runner)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	log := opt.Log
	if log == nil {
		log = nopLogger{}
	}
	return &capturer{
		opt:      opt,
		root:     opt.Root,
		stage:    opt.Dir,
		log:      log,
		res:      &Result{Dir: opt.Dir},
		readFile: os.ReadFile,
		owner:    statOwner,
	}, nil
}

func (c *capturer) run(ctx context.Context) (*Result, error) {
	if err := prepareDir(c.stage); err != nil {
		return nil, err
	}
	if !c.opt.Packages.Present() {
		return nil, fmt.Errorf("%s isn't installed here, and Carryon needs it to list your packages", c.opt.Packages.Name())
	}
	if !c.opt.IsRoot {
		c.warn("not running as root: files only root can read are skipped and the record is marked incomplete")
	}
	var err error
	if c.ids, err = readIDNames(c.root); err != nil {
		return nil, err
	}
	users := c.opt.Users
	if users == nil {
		var warnings []string
		users, warnings, err = SelectUsers(c.root, c.opt.Config.Backup.Users)
		if err != nil {
			return nil, err
		}
		for _, w := range warnings {
			c.warn("%s", w)
		}
	}
	c.res.Users = users
	c.mounts = c.opt.Mounts
	if c.mounts == nil {
		if c.mounts, err = dest.ReadMounts(c.root); err != nil {
			c.warn("can't read the mount table, so mounts in home folders may be recorded: %v", err)
		}
	}

	c.log.Info("packages and repositories")
	if err := c.capturePackages(ctx); err != nil {
		return nil, err
	}
	c.log.Info("files changed or added outside of packages")
	if err := c.captureFiles(ctx); err != nil {
		return nil, err
	}
	c.log.Info("services")
	if err := c.captureServices(ctx); err != nil {
		return nil, err
	}
	if err := c.unitNeeds(ctx); err != nil {
		return nil, err
	}
	c.log.Info("kernel, accounts and language tools")
	c.captureKernel()
	c.captureAccounts()
	c.captureTools(ctx)
	c.log.Info("home folders and secrets")
	if err := c.capturePaths(); err != nil {
		return nil, err
	}
	c.res.FingerprintReaders = c.opt.Facts.FingerprintReaders(ctx)

	c.log.Info("grouping into bundles")
	c.group(ctx)
	c.res.Items = c.items
	c.res.Meta = c.meta(ctx)
	rec := record.Record{Meta: c.res.Meta, Items: c.items, Bundles: c.res.Bundles}
	if err := rec.Save(c.stage); err != nil {
		return nil, fmt.Errorf("writing the record to %s: %w", c.stage, err)
	}
	versions, err := c.opt.Packages.Versions(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.write(VersionsFile, versions); err != nil {
		return nil, err
	}
	if c.opt.Nightly {
		c.log.Info("listing what's installed but left out of the backup on purpose")
		if err := c.write(NotBackedUpMD, []byte(c.notBackedUp())); err != nil {
			return nil, err
		}
	}
	if len(c.res.Skipped) > 0 {
		if err := c.write(SkippedFile, []byte(strings.Join(c.res.Skipped, "\n")+"\n")); err != nil {
			return nil, err
		}
	}
	return c.res, nil
}

// prepareDir makes sure dir exists, is empty and is readable by its owner only.
func prepareDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("making the record folder: %w", err)
		}
	case err != nil:
		return fmt.Errorf("reading the record folder: %w", err)
	case len(entries) > 0:
		return fmt.Errorf("%s isn't empty: capture writes a whole new record, so give it an empty folder", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("making the record folder private: %w", err)
	}
	return os.Mkdir(filepath.Join(dir, FilesDir), 0o700)
}

// meta describes the machine for meta.json.
func (c *capturer) meta(ctx context.Context) record.Meta {
	f := c.opt.Facts
	m := record.Meta{
		Carryon:     version.Version,
		Chassis:     f.Chassis(ctx),
		Complete:    c.opt.IsRoot && len(c.res.Skipped) == 0,
		Distro:      osReleaseID(c.root),
		Format:      record.FormatCurrent,
		Host:        f.Hostname(),
		Kernel:      c.readTrim("/proc/sys/kernel/osrelease"),
		Machine:     f.Fingerprint(),
		MachineName: c.opt.Config.MachineName,
	}
	for _, u := range c.res.Users {
		m.Users = append(m.Users, record.User{Name: u.Name, Home: u.Home})
	}
	if len(c.res.Users) > 0 {
		m.User, m.Home = c.res.Users[0].Name, c.res.Users[0].Home
	}
	return m
}

// notBackedUp writes the not-backed-up list, one part per user.
func (c *capturer) notBackedUp() string {
	var mounts []string
	for _, m := range c.mounts {
		mounts = append(mounts, c.path(m.MountPoint))
	}
	host := c.opt.Facts.Hostname()
	var parts []string
	for _, u := range c.res.Users {
		name := host
		if len(c.res.Users) > 1 {
			name = fmt.Sprintf("%s, %s's home", host, u.Name)
		}
		parts = append(parts, catalog.NotBackedUp(catalog.ListInput{
			Host:     name,
			Date:     c.opt.Now(),
			Home:     c.path(u.Home),
			Excludes: c.opt.Excludes,
			Hints:    c.opt.Hints,
			Mounts:   mounts,
		}))
	}
	return strings.Join(parts, "\n")
}

// sortItems sorts by type, then id, then user: the order grouping sees them in.
func sortItems(items []record.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.User < b.User
	})
}

func (c *capturer) add(items ...record.Item) { c.items = append(c.items, items...) }

func (c *capturer) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	c.res.Warnings = append(c.res.Warnings, msg)
	c.log.Warn("%s", msg)
}

// skip records a path that couldn't be read.
func (c *capturer) skip(path string) { c.res.Skipped = append(c.res.Skipped, path) }

// path turns an absolute system path into one under the root.
func (c *capturer) path(abs string) string { return filepath.Join(c.root, abs) }

// readTrim reads a system file with surrounding space trimmed, or "" if it can't.
func (c *capturer) readTrim(abs string) string {
	b, err := c.readFile(c.path(abs))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// write writes a file into the staged record.
func (c *capturer) write(name string, data []byte) error {
	if err := os.WriteFile(filepath.Join(c.stage, name), data, 0o600); err != nil {
		return fmt.Errorf("writing the record: %w", err)
	}
	return nil
}

// statOwner returns a file's numeric owner and group.
func statOwner(_ string, fi fs.FileInfo) (int, int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return 0, 0
}

// osReleaseID is the distribution's ID from os-release, like "arch" or "cachyos".
func osReleaseID(root string) string {
	for _, p := range []string{"etc/os-release", "usr/lib/os-release"} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ID="); ok {
				return strings.Trim(v, `"'`)
			}
		}
		return ""
	}
	return ""
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any) {}
func (nopLogger) Warn(string, ...any) {}
