package config

import (
	"errors"
	"strings"
	"testing"
)

// fakeSteps pretend the format went from version 1 to 3: version 2 renamed backup.delay to
// backup.after_change_delay, and version 3 moved machine_name's old name "hostname".
var fakeSteps = map[int]migration{
	1: {note: "backup.delay is now backup.after_change_delay", apply: func(raw map[string]any) error {
		moveSetting(raw, "backup.delay", "backup.after_change_delay")
		return nil
	}},
	2: {note: "hostname is now machine_name", apply: func(raw map[string]any) error {
		moveSetting(raw, "hostname", "machine_name")
		return nil
	}},
}

func TestMigrateUpgradesOlderFiles(t *testing.T) {
	path := write(t, "config_version = 1\nhostname = \"desktop\"\n[backup]\ndelay = \"30min\"\n")
	cfg, notes, unknown, err := load(path, fakeSteps, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %q (old names should be gone)", unknown)
	}
	if cfg.ConfigVersion != 3 || cfg.MachineName != "desktop" || cfg.Backup.AfterChangeDelay != "30min" {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "from config_version 1 to 2: backup.delay is now") || !strings.Contains(notes[1], "2 to 3") {
		t.Errorf("notes = %q", notes)
	}
	if cfg.Retention.KeepDaily != 7 {
		t.Errorf("defaults lost: %+v", cfg.Retention)
	}
}

func TestMigrateKeepsCurrentFilesAsTheyAre(t *testing.T) {
	path := write(t, "config_version = 3\n[backup]\ndelay = \"30min\"\n")
	_, notes, unknown, err := load(path, fakeSteps, 3)
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes = %q, err = %v", notes, err)
	}
	if len(unknown) != 1 || !strings.Contains(unknown[0], "backup.delay") {
		t.Errorf("unknown = %q", unknown)
	}
}

func TestMigrateErrors(t *testing.T) {
	failing := map[int]migration{1: {note: "x", apply: func(map[string]any) error { return errors.New("broken") }}}
	tests := []struct {
		text  string
		steps map[int]migration
		want  string
	}{
		{"config_version = 0\n", fakeSteps, "config_version 0 isn't valid"},
		{"config_version = 1\n", map[int]migration{}, "config_version 1 can't be upgraded to 3"},
		{"config_version = 1\n", failing, "upgrading config_version 1 to 2: broken"},
	}
	for _, tt := range tests {
		_, _, _, err := load(write(t, tt.text), tt.steps, 3)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.text, err, tt.want)
		}
	}
}

func TestMoveSettingKeepsANewerValue(t *testing.T) {
	raw := map[string]any{"backup": map[string]any{"delay": "1h", "after_change_delay": "2h"}}
	moveSetting(raw, "backup.delay", "backup.after_change_delay")
	b := raw["backup"].(map[string]any)
	if _, ok := b["delay"]; ok || b["after_change_delay"] != "2h" {
		t.Errorf("backup = %v", b)
	}
	moveSetting(raw, "nothing.here", "machine_name")
	if _, ok := raw["machine_name"]; ok {
		t.Errorf("a missing setting was created: %v", raw)
	}
}

func TestCurrentVersionHasAllItsMigrations(t *testing.T) {
	for v := 1; v < CurrentVersion; v++ {
		if _, ok := migrations[v]; !ok {
			t.Errorf("no migration from config_version %d", v)
		}
	}
}
