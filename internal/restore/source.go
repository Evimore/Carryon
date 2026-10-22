// Package restore rebuilds a machine from a backup. It chooses what to restore from (the machine,
// the snapshot and its record), works out which bundles start ticked here, makes the plan (only
// what differs from this machine, in the order applying it follows), and applies it (Applier).
// See docs/design.md, "Restoring".
//
// Only the Applier changes the system. Planning reads: fetching a record writes into a temp
// folder, remembered choices go to Carryon's state folder (SaveChoices), and the plan file is
// written by the caller.
//
// It ports the prototype's get_source, pick, adapt, file_state, boot_risk, make_plan, show_plan,
// apply_plan, install_one_by_one, remap_owner, merge_rclone and restore_data (legacy/sysbackup.py).
package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/ui"
)

// Where the record lives inside a snapshot.
const (
	// RecordPath is where Carryon keeps the record, and so where it is inside its snapshots.
	RecordPath = "/var/lib/carryon/record"
)

// Files in a record besides meta.json, items.jsonl and bundles.json.
const (
	FilesDir      = "files"            // copies of edited and added system files
	UserUnitsDir  = "user-units"       // user-units/<user>/<unit>
	NotBackedUpMD = "not-backed-up.md" // what was left out on purpose and how it comes back
)

// Source is the record a restore works from.
type Source struct {
	Dir      string           // the record folder
	Record   *record.Record   // the loaded record
	Snapshot *engine.Snapshot // the snapshot it came from; nil for a record on disk
	tmp      string           // temp folder to remove on Close
}

// Machine is the name of the machine the record describes.
func (s *Source) Machine() string { return s.Record.Meta.Name() }

// Close removes the temp folder a fetched record was put in.
func (s *Source) Close() error {
	if s == nil || s.tmp == "" {
		return nil
	}
	err := os.RemoveAll(s.tmp)
	s.tmp = ""
	return err
}

// OpenRecord uses a record on disk (restore --record DIR).
func OpenRecord(dir string) (*Source, error) {
	r, err := record.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("can't read the system record in %s: %w", dir, err)
	}
	return &Source{Dir: dir, Record: r}, nil
}

// Repo is the repository a record is fetched from. *engine.Engine provides it.
type Repo interface {
	// Snapshots lists the snapshots, oldest first; host "" lists every machine's.
	Snapshots(ctx context.Context, host string) ([]engine.Snapshot, error)
	// Run runs restic quietly.
	Run(ctx context.Context, args ...string) (engine.Result, error)
}

// FetchOptions choose the machine and snapshot to restore from.
type FetchOptions struct {
	// From is the machine to restore from (--from). Empty: the only one, or ask.
	From string
	// Snapshot is "latest" (the default), "pick" to choose from a list, or a snapshot ID or the
	// start of one.
	Snapshot string
	// Here is this machine's name. When the backup holds several machines, it's offered first.
	Here string
	// TempDir is where the record is put. Empty means /var/tmp, or the system's temp folder.
	TempDir string
	// Location is the time zone snapshot times are shown in. Nil means the local one.
	Location *time.Location
	// RecordPaths are where the record may be in the snapshot, in order. Empty means RecordPath.
	RecordPaths []string
}

// timeFormat is how snapshot times are shown.
const timeFormat = "2006-01-02 15:04"

// Fetch chooses the machine and snapshot, asking when the options don't say, and fetches only
// the record out of the snapshot into a temp folder. Close the source to remove it.
func Fetch(ctx context.Context, repo Repo, ask ui.Prompter, opt FetchOptions) (*Source, error) {
	snaps, err := repo.Snapshots(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, errors.New("the backup has no snapshots yet: nothing to restore from")
	}
	loc := opt.Location
	if loc == nil {
		loc = time.Local
	}
	machine, err := chooseMachine(snaps, ask, opt, loc)
	if err != nil {
		return nil, err
	}
	snap, err := chooseSnapshot(snaps, machine, ask, opt.Snapshot, loc)
	if err != nil {
		return nil, err
	}
	tmpBase := opt.TempDir
	if tmpBase == "" {
		tmpBase = defaultTempDir()
	}
	tmp, err := os.MkdirTemp(tmpBase, "carryon-source-")
	if err != nil {
		return nil, fmt.Errorf("making a folder for the record: %w", err)
	}
	src := &Source{Snapshot: &snap, tmp: tmp}
	recordPaths := opt.RecordPaths
	if len(recordPaths) == 0 {
		recordPaths = []string{RecordPath}
	}
	if err := src.fetch(ctx, repo, recordPaths); err != nil {
		src.Close()
		return nil, err
	}
	return src, nil
}

// fetch restores the record out of the snapshot and loads it.
func (s *Source) fetch(ctx context.Context, repo Repo, recordPaths []string) error {
	args := []string{"restore", s.Snapshot.ID, "--target", s.tmp}
	for _, p := range recordPaths {
		args = append(args, "--include", p)
	}
	res, err := repo.Run(ctx, args...)
	if err != nil {
		return err
	}
	for _, p := range recordPaths {
		dir := filepath.Join(s.tmp, p)
		if _, err := os.Stat(filepath.Join(dir, record.ItemsFile)); err != nil {
			continue
		}
		r, err := record.Load(dir)
		if err != nil {
			return fmt.Errorf("the system record in snapshot %s can't be read: %w", s.Snapshot.ShortID, err)
		}
		s.Dir, s.Record = dir, r
		return nil
	}
	msg := fmt.Sprintf("snapshot %s of %s has no system record", s.Snapshot.ShortID, s.Snapshot.Hostname)
	if e := res.Err(); e != nil {
		return fmt.Errorf("%s: %w", msg, e)
	}
	return errors.New(msg + ": it wasn't made by Carryon")
}

func defaultTempDir() string {
	if fi, err := os.Stat("/var/tmp"); err == nil && fi.IsDir() {
		return "/var/tmp" // on disk: a record can hold big files
	}
	return os.TempDir()
}

// chooseMachine picks the machine to restore from: --from, the only one, or the user's choice.
func chooseMachine(snaps []engine.Snapshot, ask ui.Prompter, opt FetchOptions, loc *time.Location) (string, error) {
	newest := map[string]time.Time{}
	for _, s := range snaps {
		if s.Time.After(newest[s.Hostname]) || newest[s.Hostname].IsZero() {
			newest[s.Hostname] = s.Time
		}
	}
	hosts := make([]string, 0, len(newest))
	for h := range newest {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	list := strings.Join(hosts, ", ")
	if opt.From != "" {
		if _, ok := newest[opt.From]; !ok {
			return "", fmt.Errorf("the backup has no snapshots of %s; machines in it: %s", opt.From, list)
		}
		return opt.From, nil
	}
	if len(hosts) == 1 {
		return hosts[0], nil
	}
	m := ui.Menu{
		Title: "Restore from which machine?",
		Text:  "Machines in the backup:",
		Flag:  "--from",
	}
	for _, h := range hosts {
		m.Options = append(m.Options, ui.Option{Value: h, Note: "newest backup " + newest[h].In(loc).Format(timeFormat)})
		if h == opt.Here {
			m.Default = h
		}
	}
	choice, err := ask.Menu(m)
	var need *ui.NeedsAnswerError
	if errors.As(err, &need) {
		return "", fmt.Errorf("the backup holds several machines (%s): choose one with --from", list)
	}
	return choice, err
}

// chooseSnapshot picks one of the machine's snapshots: the newest, one from a list, or by ID.
func chooseSnapshot(snaps []engine.Snapshot, machine string, ask ui.Prompter, which string, loc *time.Location) (engine.Snapshot, error) {
	var mine []engine.Snapshot
	for _, s := range snaps {
		if s.Hostname == machine {
			mine = append(mine, s)
		}
	}
	switch which {
	case "", "latest":
		return mine[len(mine)-1], nil
	case "pick":
		m := ui.Menu{
			Title:   "Which backup of " + machine + "?",
			Text:    "Newest first:",
			Default: mine[len(mine)-1].ID,
			Flag:    "--snapshot",
		}
		for i := len(mine) - 1; i >= 0; i-- {
			s := mine[i]
			m.Options = append(m.Options, ui.Option{Value: s.ID, Label: s.ShortID,
				Note: strings.TrimSpace(s.Time.In(loc).Format(timeFormat) + "  " + strings.Join(s.Tags, ","))})
		}
		id, err := ask.Menu(m)
		if err != nil {
			return engine.Snapshot{}, err
		}
		for _, s := range mine {
			if s.ID == id {
				return s, nil
			}
		}
		return engine.Snapshot{}, fmt.Errorf("no snapshot %s of %s", id, machine)
	}
	for i := len(mine) - 1; i >= 0; i-- {
		if strings.HasPrefix(mine[i].ID, which) {
			return mine[i], nil
		}
	}
	return engine.Snapshot{}, fmt.Errorf("no snapshot %s of %s: list them with: sudo carryon restic snapshots --host %s", which, machine, machine)
}
