package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/backup"
	"github.com/Evimore/carryon/internal/dest"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/setup"
	"github.com/Evimore/carryon/internal/state"
)

// readOnly is the engine for restic commands that only read (snapshots, ls, restore into a temp
// folder): it passes --no-lock, so they never write a lock into the repository. restic check keeps
// using the normal engine, since it needs its lock.
func readOnly(e *engine.Engine) *engine.Engine {
	ro := *e
	ro.NoLock = true
	return &ro
}

// checkRepository checks the repository: the destination is the one recorded at init (before any
// restic command), it opens, this machine's newest snapshot is recent and the recovery kit is
// saved. The deep check then looks inside the newest snapshot, re-reads part of the data and
// restores a few files.
func (v *verifier) checkRepository(ctx context.Context) {
	r := v.rep
	r.section("The backup repository")
	opt := v.opt
	url := opt.Config.Repository.URL
	if opt.Engine == nil || opt.Checker == nil {
		r.add(Line{Level: Problem, Text: "verify: a restic engine and a destination checker are needed"})
		return
	}
	id, err := state.ReadDestination(opt.Dirs)
	if err != nil {
		r.add(Line{Level: Problem, Text: err.Error()})
		return
	}
	if id.RepoID == "" {
		r.add(Line{Level: Problem, Text: "Carryon has no record of which repository belongs at " + url + ".",
			Fix: "Run `sudo carryon init` to connect to the repository again."})
		return
	}
	// Fail closed, like the backup: no restic command runs on a destination that isn't the one
	// recorded at init.
	res := opt.Checker.Check(ctx, url, id)
	if !res.OK() {
		switch res.Status {
		case dest.NotMounted, dest.Missing, dest.Unreachable:
			r.add(Line{Level: Warning, Text: "The repository couldn't be checked: " + res.Message, Fix: res.Fix})
			v.checkLastBackup()
		default:
			r.add(Line{Level: Problem, Text: res.Message + " Verify didn't run restic on it.", Fix: res.Fix})
		}
		return
	}

	out, err := readOnly(opt.Engine).Run(ctx, "snapshots", "--json")
	if err == nil {
		err = out.Err()
	}
	var snaps []engine.Snapshot
	if err == nil {
		snaps, err = engine.ParseSnapshots([]byte(out.Stdout))
	}
	if err != nil {
		cause, fix := problemOf(err)
		r.add(Line{Level: Problem, Text: "The repository " + url + " can't be opened: " + cause, Fix: fix})
		return
	}
	r.ok("The repository %s opens with the saved password.", url)

	var mine []engine.Snapshot
	others := map[string]bool{}
	for _, s := range snaps {
		if s.Hostname == opt.Machine {
			mine = append(mine, s)
		} else {
			others[s.Hostname] = true
		}
	}
	if len(mine) == 0 {
		r.add(Line{Level: Problem, Text: "The repository has no backups of " + opt.Machine + " yet.",
			Fix: "Back up now with: sudo carryon backup"})
	} else {
		last := mine[len(mine)-1]
		now := opt.Now()
		text := fmt.Sprintf("The newest backup of %s is from %s (%s); %s kept.", opt.Machine, stamp(last.Time),
			ago(now, last.Time), count(len(mine), "backup"))
		if now.Sub(last.Time) < v.maxAge() {
			r.ok("%s", text)
		} else {
			r.add(Line{Level: Problem, Text: text + " That's older than verify.max_age_hours.",
				Fix: "See why backups stopped with: carryon status\nBack up now with: sudo carryon backup"})
		}
	}
	if len(others) > 0 {
		names := make([]string, 0, len(others))
		for n := range others {
			names = append(names, n)
		}
		sort.Strings(names)
		r.info("Other machines in this repository: %s.", strings.Join(names, ", "))
	}
	v.checkKit()

	if !opt.Deep {
		r.info("This was the quick check. --deep also looks inside the newest backup, re-reads part of the data and tries a restore.")
		return
	}
	if len(mine) == 0 {
		return
	}
	last := mine[len(mine)-1]
	v.checkContents(ctx, last)
	v.checkIntegrity(ctx)
	v.checkRestore(ctx, last)
}

// checkLastBackup checks the newest backup's age from the run history, for when the repository
// can't be reached.
func (v *verifier) checkLastBackup() {
	runs, _ := state.ReadRuns(v.opt.Dirs)
	t, ok := lastBackup(runs)
	if !ok {
		v.rep.add(Line{Level: Warning, Text: "The run history has no successful backup of this machine."})
		return
	}
	now := v.opt.Now()
	text := "The last successful backup ended " + stamp(t) + " (" + ago(now, t) + ")."
	if now.Sub(t) < v.maxAge() {
		v.rep.ok("%s", text)
		return
	}
	v.rep.add(Line{Level: Problem, Text: text + " That's older than verify.max_age_hours.",
		Fix: "Make the backup destination available, then run: sudo carryon backup"})
}

// checkKit checks that the user confirmed saving the recovery kit. It warns until they do.
func (v *verifier) checkKit() {
	if setup.KitConfirmed(v.opt.Dirs) {
		when := ""
		if data, err := os.ReadFile(setup.KitConfirmedFile(v.opt.Dirs)); err == nil {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err == nil {
				when = " (" + t.Format("2006-01-02") + ")"
			}
		}
		v.rep.ok("You confirmed the recovery kit is saved outside this machine%s.", when)
		return
	}
	v.rep.add(Line{Level: Warning,
		Text: "The recovery kit isn't confirmed as saved: without the password and the repository address, a new machine can't get the backup back.",
		Fix:  "Show it with `sudo carryon recovery-kit`, save it in your password manager, and confirm there."})
}

// realPath is where a system path is on disk: under the system root.
func (v *verifier) realPath(p string) string { return filepath.Join(v.root(), p) }

// checkContents checks that the newest snapshot holds the record, Carryon's settings and the
// backed-up paths.
func (v *verifier) checkContents(ctx context.Context, snap engine.Snapshot) {
	want := []string{filepath.Join(v.opt.Dirs.RecordDir(), record.ItemsFile)}
	if _, err := os.Stat(v.opt.Dirs.ConfigFile()); err == nil {
		want = append(want, v.opt.Dirs.ConfigFile())
	}
	for _, p := range v.backedUp() {
		if p == "/" || v.isExcluded(p) {
			continue
		}
		if _, err := os.Lstat(v.realPath(p)); err == nil {
			want = append(want, v.realPath(p))
		}
	}
	want = dedupe(want)
	parents := map[string]bool{}
	for _, w := range want {
		parents[filepath.Dir(w)] = true
	}
	args := []string{"ls", "--json", snap.ID}
	for _, p := range sortedKeys(parents) {
		args = append(args, p)
	}
	out, err := readOnly(v.opt.Engine).Run(ctx, args...)
	if err == nil {
		err = out.Err()
	}
	have := map[string]bool{}
	if err == nil {
		err = engine.ParseLs(strings.NewReader(out.Stdout), func(n engine.Node) error {
			have[n.Path] = true
			return nil
		})
	}
	if err != nil {
		cause, fix := problemOf(err)
		v.rep.add(Line{Level: Problem, Text: "The newest backup's files couldn't be listed: " + cause, Fix: fix})
		return
	}
	// A path outside every folder the backup was given was added to the backup, or appeared,
	// after it was made: the next backup takes it in, so it isn't missing.
	var missing, later []string
	for _, w := range want {
		switch {
		case have[w]:
		case len(snap.Paths) == 0 || underAny(w, snap.Paths...):
			missing = append(missing, w)
		default:
			later = append(later, w)
		}
	}
	level := Problem
	if t, ok := lastCapture(v.opt.Dirs.RecordDir()); ok && t.After(snap.Time) {
		level = Warning // the record changed after this backup: the next one picks up new paths
	}
	okText := "The newest backup holds the record, Carryon's settings and every backed-up folder."
	if len(later) > 0 {
		okText = "The newest backup holds the record, Carryon's settings and every folder backed up when it was made."
	}
	v.rep.check(level, missing, okText,
		"The newest backup is missing some of what it should hold:",
		"Back up again with: sudo carryon backup. If they stay missing, check the exclude list (carryon why PATH).")
	if len(later) > 0 {
		v.rep.add(Line{Level: Info, Text: "Not in the newest backup yet, since they were added after it; the next backup takes them in:",
			Details: later})
	}
}

// checkIntegrity runs restic check, re-reading verify.read_subset of the data (all of it for
// --full). check takes the lock it needs; it changes nothing else.
func (v *verifier) checkIntegrity(ctx context.Context) {
	subset := v.opt.Config.Verify.ReadSubset
	args := []string{"check", "--read-data-subset=" + subset}
	what := subset + " of the data"
	if v.opt.Full {
		args = []string{"check", "--read-data"}
		what = "all of the data"
	}
	v.log.Info("Checking the repository's integrity (re-reading %s)", what)
	out, err := v.opt.Engine.Live(ctx, nil, args...)
	if err == nil && out.Code == 0 {
		v.rep.ok("The integrity check passed: restic re-read %s and found no errors.", what)
		return
	}
	if err == nil {
		err = out.Err()
	}
	cause, _ := problemOf(err)
	details := lastLines(out.Stdout+"\n"+out.Stderr, 8)
	v.rep.add(Line{Level: Problem, Text: "The integrity check failed: " + cause, Details: details,
		Fix: "See the whole report with: sudo carryon restic " + strings.Join(args, " ")})
}

// checkRestore restores the record and Carryon's settings from the newest snapshot into a
// temporary folder and compares the record with the one on disk.
func (v *verifier) checkRestore(ctx context.Context, snap engine.Snapshot) {
	items := filepath.Join(v.opt.Dirs.RecordDir(), record.ItemsFile)
	target := filepath.Join(v.tmp, "restore")
	args := []string{"restore", snap.ID, "--target", target, "--include", items}
	if _, err := os.Stat(v.opt.Dirs.ConfigFile()); err == nil {
		args = append(args, "--include", v.opt.Dirs.ConfigFile())
	}
	out, err := readOnly(v.opt.Engine).Run(ctx, args...)
	if err == nil {
		err = out.Err()
	}
	got, rerr := os.ReadFile(filepath.Join(target, items))
	if err != nil || rerr != nil {
		cause := ""
		if err != nil {
			cause, _ = problemOf(err)
		} else {
			cause = "the record wasn't among the restored files"
		}
		v.rep.add(Line{Level: Problem, Text: "The test restore failed: " + cause, Details: lastLines(out.Stderr, 4),
			Fix: "Try it by hand with: sudo carryon restic restore latest --target /tmp/carryon-test --include " + items})
		return
	}
	now, err := os.ReadFile(items)
	if err == nil && bytes.Equal(got, now) {
		v.rep.ok("A test restore worked: the restored record matches the one on disk.")
		return
	}
	v.rep.info("A test restore worked; the record changed after this backup (normal if something was installed since).")
}

// problemOf turns an error into what went wrong and what to do.
func problemOf(err error) (cause, fix string) {
	var re *engine.Error
	if errors.As(err, &re) {
		return re.Problem.Cause, re.Problem.Fix
	}
	var bp *backup.Problem
	if errors.As(err, &bp) {
		return bp.Cause, bp.Fix
	}
	if err == nil {
		return "", ""
	}
	return err.Error(), ""
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
