package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/bundle"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/restore"
	"github.com/Evimore/carryon/internal/ui"
)

// Restoring as you go, the command side: carryon restore keeps its source, applies offers for
// carryon-restore@.service (--offered) and stops them (--stop-offers). See docs/design.md,
// "As you go".

func orLocal(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

// appPackages are the packages of an app's bundle restored on its own, so the restore holds its
// home files back while the app runs instead of while its user is signed in. nil for other
// restores.
func appPackages(src *restore.Source, bundleID string) []string {
	if bundleID == "" || !bundle.IsApp(bundleID) {
		return nil
	}
	var pkgs []string
	for _, it := range src.Record.Items {
		if it.Bundle == bundleID && (it.Type == record.TypePkg || it.Type == record.TypeAUR) {
			pkgs = append(pkgs, it.ID)
		}
	}
	return pkgs
}

// keepSource keeps the source of a restore from the backup for restoring as you go, and notes
// what was restored from it (only when it applied cleanly), so it isn't offered again. When this
// machine backs up under the source machine's name, it tags the snapshot so retention keeps it.
// Problems are warnings: the restore itself is done.
func keepSource(ctx context.Context, log *ui.Logger, dirs paths.Dirs, env restoreSystem, src *restore.Source, fromKept bool, bundles []string, clean bool) {
	if src.Snapshot == nil || geteuid() != 0 {
		return // a record on disk: no snapshot to restore the rest from
	}
	var restored []string
	if clean {
		restored = bundles
	}
	if fromKept {
		for _, b := range restored {
			if err := restore.SettleOffer(dirs, b); err != nil {
				log.Warn("Carryon may offer to restore %s again: %v", b, err)
			}
		}
		return
	}
	replaced, err := restore.Keep(dirs.State, src, restored, env.Now())
	if err != nil {
		log.Warn("installing apps won't offer their saved settings: %v", err)
		return
	}
	if err := restore.CloseOffers(dirs, restored, replaced != nil); err != nil {
		log.Warn("%v", err)
	}
	cfg, err := loadConfig(dirs)
	if err != nil {
		return
	}
	if replaced != nil && replaced.Tagged {
		if err := guarded(ctx, env, cfg, func(repo restore.Repo) error { return restore.Untag(ctx, repo, *replaced) }); err != nil {
			log.Warn("the backup kept before stays out of retention: %v", err)
		}
	}
	if machineName(cfg) == src.Machine() {
		if err := guarded(ctx, env, cfg, func(repo restore.Repo) error { return restore.TagSource(ctx, repo, dirs.State) }); err != nil {
			log.Warn("retention may forget the backup apps' saved settings come from: %v", err)
		}
	}
}

// guarded checks the destination, then runs do on the repository: tagging writes into it.
func guarded(ctx context.Context, env restoreSystem, cfg config.Config, do func(restore.Repo) error) error {
	if env.Guard == nil {
		return errors.New("the destination can't be checked")
	}
	repo, err := env.Guard(ctx, cfg)
	if err != nil {
		return err
	}
	return do(repo)
}

// bundleIDRE is what a bundle ID passed to carryon-restore@.service may look like: a word that
// can't be a path or an option.
var bundleIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+-]{0,199}$`)

// unescapeInstance turns systemd's \xNN escapes in a unit's instance name back into characters,
// since a unit name can't hold some characters bundle IDs may have, such as "+".
func unescapeInstance(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// restoreOffered applies an offer for carryon-restore@.service: only the bundle's items other
// than software, only while the plan is the one offered, without a terminal. A plan that changed
// renews the offer and applies nothing.
func restoreOffered(ctx context.Context, out io.Writer, log *ui.Logger, dirs paths.Dirs, env restoreSystem, arg string) error {
	if geteuid() != 0 {
		return errors.New("restoring an offered bundle changes the system, so it needs root: start carryon-restore@BUNDLE.service, or run sudo carryon restore --bundle BUNDLE")
	}
	id := unescapeInstance(arg)
	if !bundleIDRE.MatchString(id) {
		return fmt.Errorf("%q isn't a bundle ID: carryon-restore@ takes the bundle of an offer in carryon inbox, such as app-obs-studio", arg)
	}
	src, kept, err := restore.OpenKept(dirs.State)
	if errors.Is(err, restore.ErrNoKept) {
		return fmt.Errorf("nothing is offered for restoring as you go, so %s wasn't restored: restore it with sudo carryon restore --bundle %s --from MACHINE", id, id)
	}
	if err != nil {
		return err
	}
	if _, ok := src.Record.Bundles.Get(id); !ok {
		return fmt.Errorf("the kept record of %s has no bundle %s, so nothing was restored", kept.Machine, id)
	}
	hostname, _ := os.Hostname()
	here := &restore.Machine{Root: env.Root, Hostname: hostname, Run: env.Run, IsRoot: true}
	plan, planner, err := restore.PlanOffer(ctx, src, here, id)
	if err != nil {
		return err
	}
	if len(plan.Changes(0)) == 0 {
		fmt.Fprintf(out, "Nothing to do: this machine already has the saved settings of %s.\n", id)
		if err := restore.SettleOffer(dirs, id); err != nil {
			log.Warn("%v", err)
		}
		return nil
	}
	if err := restore.CheckOffer(dirs, id, plan, env.Now()); err != nil {
		if errors.Is(err, restore.ErrOfferChanged) {
			return fmt.Errorf("restoring %s would now change something other than what was offered, so nothing was restored. The offer was renewed: see what it would change with carryon inbox, then restore again", id)
		}
		return fmt.Errorf("%s wasn't restored: %w", id, err)
	}
	planFile, err := writePlanFile(dirs, plan.Full(env.Location))
	if err != nil {
		log.Warn("%v", err)
	}
	plan.Summary(out, planFile, env.Location)
	cat, err := catalog.Load(filepath.Join(dirs.Share, "catalog"))
	if err != nil {
		log.Warn("the app catalog wasn't read, so its notes after restoring are missing: %v", err)
	}
	repo := &lazyRepo{open: func() (restore.Repo, error) {
		cfg, err := loadConfig(dirs)
		if err != nil {
			return nil, err
		}
		return env.Repo(cfg), nil
	}}
	applier := &restore.Applier{
		Source: src, Here: planner.Here, Packages: env.Installer, Repo: repo, Ask: ui.Defaults{}, Log: log,
		Out: out, Yes: true, Catalog: cat, Chown: env.Chown, TempDir: env.TempDir,
		AppPackages: appPackages(src, id),
	}
	res := applier.Apply(ctx, plan)
	if len(res.Problems) == 0 {
		if err := restore.SettleOffer(dirs, id); err != nil {
			log.Warn("Carryon may offer to restore %s again: %v", id, err)
		}
	}
	return reportRestore(out, log, dirs, res, false)
}

// stopOffering drops the kept source: nothing more is offered, the offers leave the inbox, and
// the snapshot's tag comes off so retention may forget it.
func stopOffering(ctx context.Context, out io.Writer, log *ui.Logger, dirs paths.Dirs, env restoreSystem) error {
	if err := needRoot("carryon restore --stop-offers"); err != nil {
		return err
	}
	kept, err := restore.ReadKept(dirs.State)
	if errors.Is(err, restore.ErrNoKept) {
		fmt.Fprintln(out, "Carryon isn't offering any saved settings.")
		return nil
	}
	if err != nil {
		log.Warn("%v", err)
	}
	if kept.Tagged {
		cfg, cerr := loadConfig(dirs)
		if cerr != nil {
			return cerr
		}
		if err := guarded(ctx, env, cfg, func(repo restore.Repo) error { return restore.Untag(ctx, repo, kept) }); err != nil {
			return fmt.Errorf("nothing was changed, since the backup the offers come from would stay out of retention (%v): try again once the backup can be reached", err)
		}
	}
	if err := restore.CloseOffers(dirs, nil, true); err != nil {
		return err
	}
	if err := restore.DropKept(dirs.State); err != nil {
		return err
	}
	fmt.Fprintf(out, "Carryon won't offer saved settings from %s any more. Restore them by hand with: sudo carryon restore --from %s\n", orDefault(kept.Machine, "the backup"), orDefault(kept.Machine, "MACHINE"))
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
