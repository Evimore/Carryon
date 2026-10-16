package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/sysexec"
)

// Logger reports progress and problems. *ui.Logger provides it.
type Logger interface {
	Info(format string, a ...any)
	Warn(format string, a ...any)
}

// System is the machine Carryon captures and backs up.
type System struct {
	// Root is where files are read from: "/" (or empty) for the real system, a fake tree in tests.
	Root string
	// Runner runs programs (as root, or as whoever runs Carryon).
	Runner sysexec.Runner
	// UserRunner runs programs as a user. Nil means the capture's default.
	UserRunner func(sysexec.User) sysexec.Runner
	// Packages is the package manager.
	Packages pkgmgr.Manager
	// Facts reads machine facts. Nil means the real ones under Root.
	Facts capture.Facts
	// IsRoot says Carryon runs as root.
	IsRoot bool
	// Users are the users to capture. Nil means the ones backup.users selects.
	Users []capture.User
	// Mounts is the mount table. Nil means reading it under Root.
	Mounts dest.MountTable
	// Catalog is the app catalog. Nil means loading it from the share folder.
	Catalog *catalog.Catalog
}

func (s System) root() string {
	if s.Root == "" {
		return "/"
	}
	return s.Root
}

// real is where a system path is found under Root.
func (s System) real(p string) string { return filepath.Join(s.root(), p) }

// CaptureOptions configure a capture of the record.
type CaptureOptions struct {
	Dirs   paths.Dirs
	Config config.Config
	System System
	// Nightly adds the slow checks the pacman hook leaves out (edited package files, the
	// not-backed-up list).
	Nightly bool
	// Out is a folder to write the record into, for a look, instead of publishing it as the
	// machine's record. It must be empty or not exist yet.
	Out string
	// ExcludeFile is where the effective exclude list is written for restic. Empty means a
	// temporary file, removed before Capture returns.
	ExcludeFile string
	// Git runs git for the record's history. Nil means System.Runner.
	Git sysexec.Runner
	Log Logger
	Now func() time.Time
}

// Captured is what a capture produced.
type Captured struct {
	Result    *capture.Result
	Published bool                 // the record went into the record folder
	Publish   record.PublishResult // what changed, when published
	Excludes  []string             // the effective exclude list
	Mounts    dest.MountTable      // the mount table capture used
	Warnings  []string             // problems that didn't stop the capture
}

// stagePrefix names the folders captures are staged in, next to the record folder.
const stagePrefix = ".stage-"

// staleStage is how old a leftover stage must be before a capture removes it.
const staleStage = 6 * time.Hour

// Capture captures the machine and publishes the record into the record folder, with its history,
// or writes it to opt.Out.
func Capture(ctx context.Context, opt CaptureOptions) (*Captured, error) {
	opt = opt.withDefaults()
	out := &Captured{}
	excludeFile := opt.ExcludeFile
	if excludeFile == "" {
		tmp, err := os.MkdirTemp("", "carryon-excludes-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		excludeFile = filepath.Join(tmp, "excludes")
	}
	in, err := prepare(opt, excludeFile, out)
	if err != nil {
		return nil, err
	}
	if err := captureWith(ctx, opt, in, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (opt CaptureOptions) withDefaults() CaptureOptions {
	if opt.Log == nil {
		opt.Log = nopLogger{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Git == nil {
		opt.Git = opt.System.Runner
	}
	return opt
}

// warn adds a warning to out and logs it.
func (out *Captured) warn(log Logger, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	out.Warnings = append(out.Warnings, msg)
	log.Warn("%s", msg)
}

// inputs is what a capture and a backup read before looking at the machine in detail.
type inputs struct {
	users    []capture.User
	catalog  *catalog.Catalog
	excludes []string
	rules    rules.BundleRules
}

// prepare selects the users, reads the mount table and the catalog, and writes the effective
// exclude list to excludeFile.
func prepare(opt CaptureOptions, excludeFile string, out *Captured) (*inputs, error) {
	sys := opt.System
	if sys.Runner == nil || sys.Packages == nil {
		return nil, errors.New("capture: a runner and a package manager are needed")
	}
	in := &inputs{users: sys.Users}
	if in.users == nil {
		var ws []string
		var err error
		in.users, ws, err = capture.SelectUsers(sys.root(), opt.Config.Backup.Users)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			out.warn(opt.Log, "%s", w)
		}
	}
	out.Mounts = sys.Mounts
	if out.Mounts == nil {
		var err error
		if out.Mounts, err = dest.ReadMounts(sys.root()); err != nil {
			out.warn(opt.Log, "%v", err)
		}
	}
	in.catalog = sys.Catalog
	if in.catalog == nil {
		var err error
		if in.catalog, err = catalog.Load(filepath.Join(opt.Dirs.Share, "catalog")); err != nil {
			out.warn(opt.Log, "the app catalog wasn't read, so its excludes and hints are missing: %v", err)
		}
	}
	// Nested btrfs subvolumes are part of the backup, so they aren't excluded as mounts.
	excludes, ws, err := capture.BuildExcludes(excludeFile, capture.ExcludeSources{
		Dirs: opt.Dirs, Config: opt.Config, Catalog: in.catalog, Users: in.users, Mounts: withoutSubvolumes(out.Mounts),
	})
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		out.warn(opt.Log, "%s", w)
	}
	in.excludes = excludes
	if in.rules, err = readBundleRules(opt.Dirs.BundlesFile(), out, opt.Log); err != nil {
		return nil, err
	}
	out.Excludes = excludes
	return in, nil
}

// captureWith captures the machine into a stage and publishes it, or into opt.Out.
func captureWith(ctx context.Context, opt CaptureOptions, in *inputs, out *Captured) error {
	sys := opt.System
	stage := opt.Out
	if stage == "" {
		recordDir := opt.Dirs.RecordDir()
		parent := filepath.Dir(recordDir)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		removeStaleStages(recordDir, opt.Now())
		var err error
		if stage, err = os.MkdirTemp(parent, filepath.Base(recordDir)+stagePrefix); err != nil {
			return fmt.Errorf("making a folder for the new record: %w", err)
		}
		// Publish moves the stage away when it succeeds; anything left is a failed capture.
		defer os.RemoveAll(stage)
	}
	var hints []catalog.Hint
	if in.catalog != nil {
		hints = in.catalog.Hints()
	}
	res, err := capture.Run(ctx, capture.Options{
		Dir:         stage,
		Root:        sys.Root,
		Runner:      sys.Runner,
		UserRunner:  sys.UserRunner,
		Packages:    sys.Packages,
		Facts:       sys.Facts,
		Config:      opt.Config,
		Users:       in.users,
		Excludes:    in.excludes,
		BundleRules: in.rules,
		Mounts:      withoutSubvolumes(out.Mounts),
		Nightly:     opt.Nightly,
		Hints:       hints,
		IsRoot:      sys.IsRoot,
		Log:         opt.Log,
		Now:         opt.Now,
	})
	if err != nil {
		return err
	}
	out.Result = res
	out.Warnings = append(out.Warnings, res.Warnings...)
	if opt.Out != "" {
		return nil
	}
	pub, err := record.Publish(ctx, stage, opt.Dirs.RecordDir(), record.PublishOptions{Commit: true, Git: opt.Git, Now: opt.Now})
	if errors.Is(err, record.ErrHistory) {
		out.warn(opt.Log, "%v", err)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("saving the system record: %w", err)
	}
	out.Published = true
	out.Publish = pub
	return nil
}

// readBundleRules reads the user's bundles.conf. No file means no rules; lines it can't read are
// warnings.
func readBundleRules(file string, out *Captured, log Logger) (rules.BundleRules, error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return rules.BundleRules{}, nil
	}
	if err != nil {
		return rules.BundleRules{}, fmt.Errorf("reading %s: %w", file, err)
	}
	defer f.Close()
	r, problems, err := rules.ParseBundleRules(f, file)
	if err != nil {
		return rules.BundleRules{}, fmt.Errorf("reading %s: %w", file, err)
	}
	for _, p := range problems {
		out.warn(log, "%s", p)
	}
	return r, nil
}

// removeStaleStages removes stages that failed captures left behind. Recent ones may belong to a
// capture still running (the pacman hook's, say) and stay.
func removeStaleStages(recordDir string, now time.Time) {
	parent, base := filepath.Split(recordDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base+stagePrefix) {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > staleStage {
			os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any) {}
func (nopLogger) Warn(string, ...any) {}
