package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/ui"
)

// Machine is one machine with backups in a repository: a restic host.
type Machine struct {
	Name     string    // the machine name (the prototype's: the hostname)
	Newest   time.Time // when its newest backup was made
	Snapshot string    // the ID of its newest backup
	Count    int       // how many backups it has
}

// machines groups snapshots by machine, the most recently backed up first.
func machines(snaps []engine.Snapshot) []Machine {
	byName := map[string]*Machine{}
	for _, s := range snaps {
		m := byName[s.Hostname]
		if m == nil {
			m = &Machine{Name: s.Hostname}
			byName[s.Hostname] = m
		}
		m.Count++
		if s.Time.After(m.Newest) || m.Snapshot == "" {
			m.Newest, m.Snapshot = s.Time, s.ID
		}
	}
	out := make([]Machine, 0, len(byName))
	for _, m := range byName {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Newest.Equal(out[j].Newest) {
			return out[i].Newest.After(out[j].Newest)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func findMachine(list []Machine, name string) (Machine, bool) {
	for _, m := range list {
		if m.Name == name {
			return m, true
		}
	}
	return Machine{}, false
}

// listMachines lists the machines with backups in the repository.
func listMachines(ctx context.Context, eng *engine.Engine) ([]Machine, error) {
	snaps, err := eng.Snapshots(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("listing the backups in the repository: %w", err)
	}
	return machines(snaps), nil
}

// dump reads one file from a snapshot. ok is false when the snapshot doesn't have it.
func dump(ctx context.Context, eng *engine.Engine, snapshot, path string) (data []byte, ok bool, err error) {
	r, err := eng.Run(ctx, "dump", snapshot, path)
	if err != nil {
		return nil, false, err
	}
	if r.Code != 0 {
		if strings.Contains(r.Stderr, "not found") || strings.Contains(r.Stderr, "is a directory") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s from the backup %s: %w", path, snapshot, r.Err())
	}
	return []byte(r.Stdout), true, nil
}

// recordMetaPaths are where a backup keeps its record's meta.json: Carryon's record, then the
// prototype's.
func (e *Env) recordMetaPaths() []string {
	return []string{
		filepath.Join(e.Dirs.RecordDir(), "meta.json"),
		filepath.Join(e.sysbackupRoot(), "var/lib/sysbackup/manifest/meta.json"),
	}
}

// machineFingerprint reads the hardware fingerprint from the record in a machine's newest backup.
// It is "" when the backup has no record that says.
func (e *Env) machineFingerprint(ctx context.Context, eng *engine.Engine, m Machine) (string, error) {
	for _, p := range e.recordMetaPaths() {
		data, ok, err := dump(ctx, eng, m.Snapshot, p)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		var meta record.Meta
		if err := json.Unmarshal(data, &meta); err != nil {
			return "", fmt.Errorf("reading the record in the backup %s: %w", m.Snapshot, err)
		}
		return meta.Machine, nil
	}
	return "", nil
}

// sameMachine tells whether m's backups come from this machine: yes, no, or unknown when either
// fingerprint is missing.
type sameness int

const (
	unknownMachine sameness = iota
	thisMachine
	otherMachine
)

func (e *Env) sameMachine(ctx context.Context, eng *engine.Engine, m Machine) (sameness, error) {
	ours := e.fingerprint()
	if ours == "" {
		return unknownMachine, nil
	}
	theirs, err := e.machineFingerprint(ctx, eng, m)
	if err != nil || theirs == "" {
		return unknownMachine, err
	}
	if theirs == ours {
		return thisMachine, nil
	}
	return otherMachine, nil
}

// checkMachineName refuses names Carryon can't store and names another machine already uses in
// the repository.
func (e *Env) checkMachineName(ctx context.Context, eng *engine.Engine, list []Machine, name string) error {
	cfg := config.Default()
	cfg.MachineName = name
	if name == "" {
		return fmt.Errorf("the machine name is empty")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	m, ok := findMachine(list, name)
	if !ok {
		return nil
	}
	same, err := e.sameMachine(ctx, eng, m)
	if err != nil {
		return err
	}
	if same == otherMachine {
		return fmt.Errorf("another machine already keeps its backups as %q in this repository: choose a different name, or the two would share and prune each other's backups", name)
	}
	return nil
}

// chooseMachineName asks for the machine name, or checks the one given with --machine-name.
func (e *Env) chooseMachineName(ctx context.Context, eng *engine.Engine, list []Machine, given, def string) (string, error) {
	check := func(s string) error { return e.checkMachineName(ctx, eng, list, s) }
	name := given
	if given != "" {
		if err := check(given); err != nil {
			return "", fmt.Errorf("--machine-name %s: %w", given, err)
		}
	} else {
		var err error
		name, err = e.Prompt.Input(ui.Input{
			Title:    "Machine name",
			Text:     "The name this machine's backups are kept under in the repository. Each machine needs its own.",
			Default:  def,
			Validate: check,
			Flag:     "--machine-name",
		})
		if err != nil {
			return "", err
		}
	}
	if m, ok := findMachine(list, name); ok {
		same, err := e.sameMachine(ctx, eng, m)
		if err != nil {
			return "", err
		}
		if same == unknownMachine {
			e.Log.Warn("The repository already has %d backups named %s, and Carryon can't tell whether they come from this machine. If they don't, run init again with another --machine-name.", m.Count, name)
		} else {
			e.Log.Info("The repository already has %d backups of %s from this machine; new backups continue them.", m.Count, name)
		}
	}
	return name, nil
}

// defaultMachineName is the hostname, when it makes a valid machine name.
func (e *Env) defaultMachineName() string {
	h, err := e.hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(strings.TrimSpace(h), ".")
	return h
}
