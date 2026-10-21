package cli

import (
	"context"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/facts"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/rules"
)

// hookOffer returns the pacman hook's step for restoring as you go, or nil when no source is kept.
// pacman names every package it changed without saying which it installed, so the step takes
// those it names that are installed now and that the record didn't hold before the hook
// refreshes it: the record's package list is read here, before the refresh. Without a record yet,
// every installed package pacman names counts.
func hookOffer(dirs paths.Dirs, sys backup.System, targets []string) func(context.Context) ([]string, error) {
	if len(targets) == 0 || !restore.HasKept(dirs.State) {
		return nil
	}
	had := map[string]bool{}
	if rec, err := record.Load(dirs.RecordDir()); err == nil {
		for _, it := range rec.Items {
			if it.Type == record.TypePkg || it.Type == record.TypeAUR {
				had[it.ID] = true
			}
		}
	}
	return func(ctx context.Context) ([]string, error) {
		now, err := sys.Packages.Installed(ctx)
		if err != nil {
			return nil, err
		}
		isInstalled := map[string]bool{}
		for _, p := range now {
			isInstalled[p] = true
		}
		var installed []string
		for _, t := range targets {
			if isInstalled[t] && !had[t] {
				installed = append(installed, t)
			}
		}
		return restore.Offer(ctx, restore.OfferOptions{
			Dirs:      dirs,
			Here:      &restore.Machine{Root: sys.Root, Run: sys.Runner, UserRunner: sys.UserRunner, IsRoot: sys.IsRoot},
			Installed: installed,
			Facts:     func() (rules.Facts, error) { return facts.Collect(ctx, sys.Root, sys.Runner) },
			Wait:      hookInboxWait,
			Now:       time.Now(),
		})
	}
}
