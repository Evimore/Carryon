package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/state"
)

// The nightly backup scans for new data folders: "ask" puts them in the inbox, "auto" backs them
// up from the next backup on, and either way the run history says so.
func TestBackupLooksForNewDataFolders(t *testing.T) {
	for _, mode := range []string{"ask", "auto", "off"} {
		t.Run(mode, func(t *testing.T) {
			env := newEnv(t)
			write(t, filepath.Join(env.root, "var/lib/jellyfin/library.db"), "x\n")
			write(t, filepath.Join(env.root, "var/lib/pacman/local/ALPM_DB_VERSION"), "9\n") // known
			opt := env.options()
			opt.Config.Detect.NewPaths = mode
			if err := os.MkdirAll(env.dirs.Conf, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := config.Write(env.dirs.ConfigFile(), opt.Config); err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), opt)
			if err != nil {
				t.Fatalf("backup: %v\n%s", err, env.log)
			}
			if w := strings.Join(res.Run.Warnings, "\n"); strings.Contains(w, "new data") || strings.Contains(w, "inbox") {
				t.Errorf("warnings %q", res.Run.Warnings)
			}
			notes := strings.Join(res.Run.Notes, "\n")
			in, _ := state.ReadInbox(env.dirs)
			cfg, _, _ := config.Load(env.dirs.ConfigFile())
			extra := strings.Join(cfg.Backup.ExtraPaths, " ")
			switch mode {
			case "ask":
				e, ok := in.Find("/var/lib/jellyfin")
				if !ok || e.By != state.ByBackup || !e.Found.Equal(env.now) || len(in.Entries) != 1 {
					t.Errorf("inbox %+v", in.Entries)
				}
				if !strings.Contains(notes, "new data to back up in /var/lib/jellyfin") {
					t.Errorf("notes %q", res.Run.Notes)
				}
			case "auto":
				if !strings.Contains(extra, "/var/lib/jellyfin") || len(in.Entries) != 0 {
					t.Errorf("extra_paths %q, inbox %+v", extra, in.Entries)
				}
				if !strings.Contains(notes, "now backing up /var/lib/jellyfin") {
					t.Errorf("notes %q", res.Run.Notes)
				}
			case "off":
				if len(in.Entries) != 0 || strings.Contains(extra, "jellyfin") || strings.Contains(notes, "jellyfin") {
					t.Errorf("off did something: inbox %+v, extra_paths %q, notes %q", in.Entries, extra, notes)
				}
			}
		})
	}
}

// Without the list of known system folders the scan would suggest the system's own state, so it
// doesn't scan, and says why.
func TestBackupWithoutTheKnownFolders(t *testing.T) {
	env := newEnv(t)
	if err := os.Remove(env.dirs.KnownFolders()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(env.root, "var/lib/jellyfin/library.db"), "x\n")
	res, err := Run(context.Background(), env.options())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Run.Warnings, "\n"), "known system folders") {
		t.Errorf("result %q, warnings %q", res.Run.Result, res.Run.Warnings)
	}
	if in, _ := state.ReadInbox(env.dirs); len(in.Entries) != 0 {
		t.Errorf("inbox %+v", in.Entries)
	}
}
