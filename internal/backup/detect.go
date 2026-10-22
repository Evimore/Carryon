package backup

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/detect"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/state"
)

// DetectOptions configure a scan for new data folders, with what a capture learned.
type DetectOptions struct {
	Dirs   paths.Dirs
	Config config.Config
	System System
	// Items, Users, Excludes and Mounts are the capture's: the record, the users whose homes are
	// backed up, the effective exclude list and the mount table.
	Items    []record.Item
	Users    []capture.User
	Excludes []string
	Mounts   dest.MountTable
	// All looks at every folder; otherwise only at the folders of the packages in Only.
	All  bool
	Only []string
	// By says what is looking: state.ByBackup, ByHook.
	By string
	// Wait is how long to wait for the inbox lock.
	Wait time.Duration
	// Out receives what detect.new_paths = "auto" did. Nil discards it.
	Out io.Writer
	Now func() time.Time
}

// Detect looks for folders holding data no package owns, outside what the backup covers, and
// does what detect.new_paths says with them. See detect.Scan and detect.Suggest.
func Detect(ctx context.Context, opt DetectOptions) (detect.Outcome, error) {
	cfg := opt.Config
	if cfg.Detect.NewPaths == "off" {
		return detect.Outcome{}, nil
	}
	known, err := detect.ReadKnownFolders(opt.Dirs.KnownFolders())
	if err != nil {
		return detect.Outcome{}, err
	}
	matcher, err := catalog.NewMatcher(opt.Excludes)
	if err != nil {
		return detect.Outcome{}, err
	}
	var covered []string
	for _, t := range Targets(opt.System, cfg, opt.Items, opt.Users) {
		covered = append(covered, t.Path)
	}
	found, err := detect.Scan(ctx, detect.ScanOptions{
		Root:     opt.System.Root,
		Runner:   opt.System.Runner,
		Manager:  opt.System.Packages,
		Items:    opt.Items,
		Covered:  covered,
		Excluded: matcher.Excluded,
		Mounts:   WithoutSubvolumes(opt.Mounts),
		Ignore:   cfg.Detect.Ignore,
		Known:    known,
		LargeGB:  cfg.Detect.LargePathGB,
		All:      opt.All,
		Only:     opt.Only,
	})
	if err != nil {
		return detect.Outcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return detect.Outcome{}, err
	}
	return detect.Suggest(found, detect.SuggestOptions{
		Dirs: opt.Dirs, Mode: cfg.Detect.NewPaths, By: opt.By, Wait: opt.Wait, Out: opt.Out, Now: opt.Now,
	})
}

// detect is the nightly scan for new data folders. What it finds goes in the run's notes;
// problems are warnings, since the backup itself is fine.
func (b *run) detect(ctx context.Context, items []record.Item, users []capture.User, capd *Captured) {
	if b.opt.Config.Detect.NewPaths == "off" {
		return
	}
	b.log.Info("Looking for new data folders to back up")
	var said strings.Builder
	res, err := Detect(ctx, DetectOptions{
		Dirs: b.opt.Dirs, Config: b.opt.Config, System: b.opt.System,
		Items: items, Users: users, Excludes: capd.Excludes, Mounts: capd.Mounts,
		All: true, By: state.ByBackup, Wait: state.InboxWait, Out: &said, Now: b.opt.Now,
	})
	for _, line := range strings.Split(strings.TrimSpace(said.String()), "\n") {
		if line != "" {
			b.log.Info("%s", line)
		}
	}
	if res.SetAside != "" {
		b.warn("The inbox was damaged; it was set aside as %s", res.SetAside)
	}
	for _, e := range res.New {
		note := "new data to back up in " + e.Path
		if what := detect.Describe(e); what != "" {
			note += " (" + what + ")"
		}
		b.log.Info("Found %s: see carryon inbox", note)
		b.res.Run.Notes = append(b.res.Run.Notes, note+": see carryon inbox")
	}
	for _, e := range res.Added {
		note := detect.AddedNote(e)
		b.log.Info("Carryon is %s", note)
		b.res.Run.Notes = append(b.res.Run.Notes, note)
	}
	if err != nil {
		b.warn("Carryon couldn't look for new data folders to back up: %v", err)
	}
}
