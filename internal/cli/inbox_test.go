package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/ui"
)

// inboxDirs is a machine with a config file and an inbox holding a suggestion for each entry.
func inboxDirs(t *testing.T, entries ...state.Entry) paths.Dirs {
	t.Helper()
	dirs := confDir(t, "[repository]\nurl = \"/mnt/backup\"\n")
	dirs.State = t.TempDir()
	if len(entries) > 0 {
		if _, err := state.UpdateInbox(dirs, state.InboxWait, func(in *state.Inbox) error {
			for _, e := range entries {
				in.Put(e)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

// jellyfin is a suggestion found by name: its bundle finds the folder without a put rule.
func jellyfin() state.Entry {
	e := state.NewEntry(state.KindPath, "/var/lib/jellyfin")
	e.App, e.Bundle, e.By, e.Size = "jellyfin", "app-jellyfin", state.ByHook, 3<<29
	e.Found = time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	return e
}

// media is a suggestion whose app is named differently from the folder, so it needs a put rule.
func media() state.Entry {
	e := state.NewEntry(state.KindPath, "/srv/media-cache")
	e.App, e.Bundle, e.By, e.Size, e.Large = "plex-media-server", "app-plex-media-server", state.ByBackup, 7<<30, true
	e.Found = time.Date(2026, 10, 2, 3, 0, 0, 0, time.UTC)
	return e
}

func obsOffer() state.Entry {
	e := state.NewEntry(state.KindRestore, "app-obs-studio")
	e.App, e.By = "obs-studio", state.ByHook
	e.Changes = []string{"restores ~/.config/obs-studio (alice)", "enables obs-relay.service"}
	e.Found = time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	return e
}

func loadTestConfig(t *testing.T, dirs paths.Dirs) config.Config {
	t.Helper()
	cfg, _, err := config.Load(dirs.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func inboxIDs(t *testing.T, dirs paths.Dirs) []string {
	t.Helper()
	in, err := state.ReadInbox(dirs)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, e := range in.Entries {
		ids = append(ids, e.ID)
	}
	return ids
}

func usePrompter(t *testing.T, p ui.Prompter) {
	t.Helper()
	old := newPrompter
	newPrompter = func(context.Context, bool) ui.Prompter { return p }
	t.Cleanup(func() { newPrompter = old })
}

func TestInboxListsWithoutATerminalOrRoot(t *testing.T) {
	useStatusClock(t)
	asUser(t, 1000)
	usePrompter(t, ui.Defaults{})
	dirs := inboxDirs(t, jellyfin(), media(), obsOffer())
	out, err := run(t, dirs, "inbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"3 suggestions waiting:",
		jellyfin().ID + "     Back up /var/lib/jellyfin?",
		"1.5 GiB, probably jellyfin, found by the pacman hook on 2026-10-01",
		"7.0 GiB (large), probably plex-media-server, found by the nightly backup on 2026-10-02",
		obsOffer().ID + "  Restore the saved settings of app-obs-studio?",
		"restore with: sudo carryon restore --bundle app-obs-studio",
		"  - restores ~/.config/obs-studio (alice)\n",
		"  - enables obs-relay.service\n",
		"Settle one with: sudo carryon add ID, or sudo carryon ignore ID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(inboxIDs(t, dirs)) != 3 {
		t.Error("listing changed the inbox")
	}

	empty := inboxDirs(t)
	if out, err := run(t, empty, "inbox"); err != nil || !strings.Contains(out, "Nothing is waiting") {
		t.Errorf("empty inbox: %q, %v", out, err)
	}
}

func TestInboxJSON(t *testing.T) {
	asUser(t, 1000)
	dirs := inboxDirs(t, jellyfin())
	out, err := run(t, dirs, "inbox", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got state.Inbox
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(got.Entries) != 1 || got.Entries[0].ID != jellyfin().ID || got.Entries[0].Path != "/var/lib/jellyfin" {
		t.Errorf("got %+v", got)
	}
	out, err = run(t, inboxDirs(t), "inbox", "--json")
	if err != nil || !strings.Contains(out, `"entries": []`) {
		t.Errorf("empty inbox: %q, %v", out, err)
	}
}

func TestAddSettlesASuggestionByID(t *testing.T) {
	asUser(t, 0)
	dirs := inboxDirs(t, jellyfin(), media(), obsOffer())

	out, err := run(t, dirs, "add", jellyfin().ID)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Added /var/lib/jellyfin to backup.extra_paths") || !strings.Contains(out, "Settled "+jellyfin().ID) {
		t.Errorf("out = %q", out)
	}
	if _, err := os.Stat(dirs.BundlesFile()); !os.IsNotExist(err) {
		t.Errorf("a folder its bundle finds by name got a put rule: %v", err)
	}

	out, err = run(t, dirs, "add", media().ID)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rules, err := os.ReadFile(dirs.BundlesFile())
	if err != nil || string(rules) != "put app-plex-media-server path /srv/media-cache\n" {
		t.Errorf("bundles.conf = %q, %v", rules, err)
	}
	if _, err := run(t, dirs, "add", media().ID); err == nil {
		t.Error("adding a settled ID again worked")
	}
	if _, err := run(t, dirs, "add", "/srv/media-cache"); err != nil {
		t.Fatal(err)
	}
	if rules, _ := os.ReadFile(dirs.BundlesFile()); strings.Count(string(rules), "put ") != 1 {
		t.Errorf("the put rule was written twice:\n%s", rules)
	}

	cfg := loadTestConfig(t, dirs)
	want := []string{"/root", "/srv", "/usr/local", "/var/lib/jellyfin"} // /srv already covers /srv/media-cache
	if strings.Join(cfg.Backup.ExtraPaths, " ") != strings.Join(want, " ") {
		t.Errorf("extra_paths = %q", cfg.Backup.ExtraPaths)
	}
	if ids := inboxIDs(t, dirs); len(ids) != 1 || ids[0] != obsOffer().ID {
		t.Errorf("inbox = %q", ids)
	}

	_, err = run(t, dirs, "add", obsOffer().ID)
	if err == nil || !strings.Contains(err.Error(), "carryon restore --bundle app-obs-studio") {
		t.Errorf("add of a restore offer: %v", err)
	}
	_, err = run(t, dirs, "add", "path-0123456789")
	if err == nil || !strings.Contains(err.Error(), "no suggestion path-0123456789") {
		t.Errorf("unknown ID: %v", err)
	}
}

func TestAddByPathSettlesSuggestionsInside(t *testing.T) {
	asUser(t, 0)
	dirs := inboxDirs(t, jellyfin(), media())
	out, err := run(t, dirs, "add", "/var/lib")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if ids := inboxIDs(t, dirs); len(ids) != 1 || ids[0] != media().ID {
		t.Errorf("inbox = %q", ids)
	}
	if _, err := os.Stat(dirs.BundlesFile()); !os.IsNotExist(err) {
		t.Errorf("adding a folder that wasn't suggested wrote a put rule: %v", err)
	}
}

func TestIgnoreSettlesSuggestions(t *testing.T) {
	asUser(t, 0)
	dirs := inboxDirs(t, jellyfin(), media(), obsOffer())

	out, err := run(t, dirs, "ignore", "/srv/media-cache/") // by path
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Added /srv/media-cache to detect.ignore") || !strings.Contains(out, "Settled "+media().ID) {
		t.Errorf("out = %q", out)
	}
	if _, err := run(t, dirs, "ignore", jellyfin().ID); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, dirs, "ignore", "/var/lib/jellyfin/cache"); err != nil || !strings.Contains(out, "already ignored") {
		t.Errorf("ignoring inside an ignored folder: %q, %v", out, err)
	}
	cfg := loadTestConfig(t, dirs)
	if strings.Join(cfg.Detect.Ignore, " ") != "/srv/media-cache /var/lib/jellyfin" {
		t.Errorf("detect.ignore = %q", cfg.Detect.Ignore)
	}
	if strings.Join(cfg.Backup.ExtraPaths, " ") != "/root /srv /usr/local" {
		t.Errorf("ignore changed extra_paths: %q", cfg.Backup.ExtraPaths)
	}

	out, err = run(t, dirs, "ignore", obsOffer().ID)
	if err != nil || !strings.Contains(out, "won't offer to restore app-obs-studio again") {
		t.Fatalf("%q, %v", out, err)
	}
	in, err := state.ReadInbox(dirs)
	if err != nil || len(in.Entries) != 0 || strings.Join(in.IgnoredBundles, " ") != "app-obs-studio" {
		t.Errorf("inbox = %+v, %v", in, err)
	}

	// Backing an ignored folder up after all takes it out of detect.ignore.
	out, err = run(t, dirs, "add", "/var/lib/jellyfin")
	if err != nil || !strings.Contains(out, "Took /var/lib/jellyfin out of detect.ignore") {
		t.Errorf("%q, %v", out, err)
	}
	if cfg := loadTestConfig(t, dirs); strings.Join(cfg.Detect.Ignore, " ") != "/srv/media-cache" {
		t.Errorf("detect.ignore = %q", cfg.Detect.Ignore)
	}
}

func TestInboxCommandsNeedRoot(t *testing.T) {
	asUser(t, 1000)
	dirs := inboxDirs(t, jellyfin())
	for _, cmd := range []string{"add", "ignore"} {
		if _, err := run(t, dirs, cmd, jellyfin().ID); err == nil || !strings.Contains(err.Error(), "needs root") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
}

func TestInboxGoesThroughSuggestionsInATerminal(t *testing.T) {
	asUser(t, 0)
	dirs := inboxDirs(t, jellyfin(), media(), obsOffer())
	script := &ui.Script{Answers: []any{"add", "ignore", "later"}}
	usePrompter(t, script)
	out, err := run(t, dirs, "inbox")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(script.Asked) != 3 || script.Asked[0].Title != "Back up /var/lib/jellyfin?" || script.Asked[2].Title != "Restore the saved settings of app-obs-studio?" {
		t.Errorf("asked %+v", script.Asked)
	}
	cfg := loadTestConfig(t, dirs)
	if !strings.Contains(strings.Join(cfg.Backup.ExtraPaths, " "), "/var/lib/jellyfin") {
		t.Errorf("extra_paths = %q", cfg.Backup.ExtraPaths)
	}
	if strings.Join(cfg.Detect.Ignore, " ") != "/srv/media-cache" {
		t.Errorf("detect.ignore = %q", cfg.Detect.Ignore)
	}
	if ids := inboxIDs(t, dirs); len(ids) != 1 || ids[0] != obsOffer().ID {
		t.Errorf("inbox = %q", ids)
	}

	// Leaving early keeps the rest.
	script = &ui.Script{Answers: []any{ui.ErrCancelled}}
	usePrompter(t, script)
	if out, err := run(t, dirs, "inbox"); err != nil || !strings.Contains(out, "the rest stays in the inbox") {
		t.Errorf("%q, %v", out, err)
	}
	if len(inboxIDs(t, dirs)) != 1 {
		t.Error("cancelling changed the inbox")
	}
}

func TestStatusCountsTheInbox(t *testing.T) {
	useStatusClock(t)
	asUser(t, 0)
	dirs := inboxDirs(t, jellyfin(), media())
	dirs.Run = t.TempDir()
	putStatus(t, "all-fine", dirs.StatusFile())
	out, err := run(t, dirs, "status")
	if err != nil || !strings.Contains(out, "2 suggestions waiting: carryon inbox") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := run(t, dirs, "ignore", jellyfin().ID); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, dirs, "status")
	if !strings.Contains(out, "1 suggestion waiting: carryon inbox") {
		t.Errorf("after ignore:\n%s", out)
	}
}

// carryon-add@.service and carryon-ignore@.service pass their instance name with --entry, which
// settles an entry in the inbox and refuses anything else: a path, an option, an ID the inbox
// doesn't hold, even with a folder of that name where the command runs.
func TestEntryFlagTakesOnlyAnIDInTheInbox(t *testing.T) {
	asUser(t, 0)
	t.Chdir(t.TempDir())
	if err := os.Mkdir("path-0123456789", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"add", "ignore"} {
		for _, args := range [][]string{
			{"--entry=-etc-shadow"},
			{"--entry=/etc/shadow"},
			{"--entry=/var/lib/jellyfin"},
			{"--entry=--yes"},
			{"--entry=path-3fa9c2d1e0/.."},
			{"--entry="},
			{"--entry=path-0123456789"}, // shaped like an ID, a folder here, not in the inbox
			{"--entry=" + jellyfin().ID, "/etc/shadow"},
			{"--entry=" + jellyfin().ID, "--entry=/etc"},
		} {
			dirs := inboxDirs(t, jellyfin())
			before, _ := os.ReadFile(dirs.ConfigFile())
			if out, err := run(t, dirs, append([]string{cmd}, args...)...); err == nil {
				t.Errorf("carryon %s %q worked:\n%s", cmd, args, out)
			}
			if after, _ := os.ReadFile(dirs.ConfigFile()); string(after) != string(before) {
				t.Errorf("carryon %s %q changed the config:\n%s", cmd, args, after)
			}
			if ids := inboxIDs(t, dirs); len(ids) != 1 {
				t.Errorf("carryon %s %q changed the inbox: %q", cmd, args, ids)
			}
		}
	}

	dirs := inboxDirs(t, jellyfin(), media(), obsOffer())
	out, err := run(t, dirs, "add", "--entry="+jellyfin().ID)
	if err != nil || !strings.Contains(out, "Added /var/lib/jellyfin to backup.extra_paths") {
		t.Errorf("add --entry: %q, %v", out, err)
	}
	if _, err := run(t, dirs, "add", "--entry="+jellyfin().ID); err == nil || !strings.Contains(err.Error(), "no suggestion "+jellyfin().ID) {
		t.Errorf("add --entry of a settled ID: %v", err)
	}
	if _, err := run(t, dirs, "add", "--entry="+obsOffer().ID); err == nil || !strings.Contains(err.Error(), "not a folder to back up") {
		t.Errorf("add --entry of a restore offer: %v", err)
	}
	out, err = run(t, dirs, "ignore", "--entry="+media().ID)
	if err != nil || !strings.Contains(out, "Added /srv/media-cache to detect.ignore") {
		t.Errorf("ignore --entry: %q, %v", out, err)
	}
	out, err = run(t, dirs, "ignore", "--entry="+obsOffer().ID)
	if err != nil || !strings.Contains(out, "won't offer to restore app-obs-studio again") {
		t.Errorf("ignore --entry of a restore offer: %q, %v", out, err)
	}
	if ids := inboxIDs(t, dirs); len(ids) != 0 {
		t.Errorf("inbox = %q", ids)
	}
	if _, err := run(t, dirs, "add"); err == nil {
		t.Error("add with neither PATH|ID nor --entry worked")
	}
}
