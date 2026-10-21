package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/rules"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// secretContent is in the backup's copy of a file: it must never reach the inbox.
const secretContent = "token = hunter2-from-the-backup"

// asYouGoRecord writes the record of desktop, whose apps' settings are offered as they get
// installed: OBS (two system files, a home folder and a service), htop (a file this machine has
// already), kate (ignored), a laptop-only app, and vim in the plain package list.
func asYouGoRecord(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"meta.json": `{"format": 2, "host": "desktop", "machine_name": "desktop", "user": "alice", "home": "/home/alice",
			"users": [{"name": "alice", "home": "/home/alice"}], "chassis": "desktop", "complete": true, "kernel": "6.17", "machine": "x"}`,
		"bundles.json": `{"packages": {"title": "Packages", "when": "always"},
			"app-htop": {"title": "htop", "when": "always"},
			"app-kate": {"title": "Kate", "when": "always"},
			"app-obs-studio": {"title": "OBS Studio", "when": "always"},
			"app-tlp": {"title": "TLP", "when": "chassis:laptop"}}`,
		"items.jsonl": `{"bundle": "packages", "id": "vim", "repo": "extra", "type": "pkg"}
{"bundle": "app-htop", "id": "htop", "repo": "extra", "type": "pkg"}
{"bundle": "app-htop", "id": "/etc/htoprc", "kind": "unowned", "mode": "644", "type": "file"}
{"bundle": "app-kate", "id": "kate", "repo": "extra", "type": "pkg"}
{"bundle": "app-kate", "home": ".config/katerc", "id": "/home/alice/.config/katerc", "type": "path", "user": "alice"}
{"bundle": "app-obs-studio", "id": "obs-studio", "repo": "extra", "type": "pkg"}
{"bundle": "app-obs-studio", "id": "/etc/obs/a.conf", "kind": "unowned", "mode": "644", "type": "file"}
{"bundle": "app-obs-studio", "id": "/etc/obs/b.conf", "kind": "unowned", "mode": "644", "type": "file"}
{"bundle": "app-obs-studio", "home": ".config/obs-studio", "id": "/home/alice/.config/obs-studio", "type": "path", "user": "alice"}
{"bundle": "app-obs-studio", "id": "obs-relay.service", "type": "svc"}
{"bundle": "app-tlp", "id": "tlp", "repo": "extra", "type": "pkg"}
{"bundle": "app-tlp", "id": "tlp.service", "type": "svc"}
`,
		"files/etc/htoprc":     "color_scheme=1\n",
		"files/etc/obs/a.conf": secretContent + "\n",
		"files/etc/obs/b.conf": "b\n",
		".git/HEAD":            "ref: refs/heads/main\n",
	} {
		writeFile(t, filepath.Join(dir, name), body)
	}
	return dir
}

// asYouGoMachine is the machine restored onto as you go: alice, htop's settings already there,
// another a.conf than the backup's.
func asYouGoMachine(t *testing.T) (root string, run *sysexec.Fake) {
	t.Helper()
	root = t.TempDir()
	for name, body := range map[string]string{
		"etc/passwd":     "root:x:0:0::/root:/usr/bin/bash\nalice:x:1000:1000::/home/alice:/usr/bin/zsh\n",
		"etc/group":      "wheel:x:998:alice\n",
		"etc/htoprc":     "color_scheme=1\n",
		"etc/obs/a.conf": "mine\n",
	} {
		writeFile(t, filepath.Join(root, name), body)
	}
	run = &sysexec.Fake{Responses: map[string]sysexec.Response{
		"systemctl is-enabled -- obs-relay.service": {Stdout: "disabled\n", Code: 1},
		"systemctl is-enabled -- tlp.service":       {Stdout: "disabled\n", Code: 1},
	}}
	return root, run
}

var snapTime = time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC)

// keepFrom keeps the record in dir, as fetched from a snapshot of desktop.
func keepFrom(t *testing.T, dirs paths.Dirs, dir, id string) {
	t.Helper()
	src, err := OpenRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	src.Snapshot = &engine.Snapshot{ID: id, ShortID: id[:8], Hostname: "desktop", Time: snapTime, Paths: []string{"/"}}
	if _, err := Keep(dirs.State, src, nil, snapTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestKeep(t *testing.T) {
	dirs := paths.Dirs{State: t.TempDir()}
	if _, _, err := OpenKept(dirs.State); !errors.Is(err, ErrNoKept) {
		t.Fatalf("nothing kept yet: %v", err)
	}
	rec := asYouGoRecord(t)
	keepFrom(t, dirs, rec, "1111111111111111")
	src, k, err := OpenKept(dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	if k.Machine != "desktop" || src.Snapshot.ID != "1111111111111111" || len(src.Record.Items) != 12 {
		t.Errorf("kept %+v with %d items", k, len(src.Record.Items))
	}
	if _, err := os.Stat(filepath.Join(src.Dir, ".git")); err == nil {
		t.Error("the record's git history was kept")
	}
	if b, _ := os.ReadFile(filepath.Join(src.Dir, "files/etc/obs/a.conf")); string(b) != secretContent+"\n" {
		t.Errorf("a.conf = %q", b)
	}
	for _, p := range []string{KeptDir(dirs.State), filepath.Join(src.Dir, "files/etc/obs/a.conf"), filepath.Join(KeptDir(dirs.State), keptInfoFile)} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s isn't root-only: %v %v", p, fi.Mode(), err)
		}
	}

	// The same snapshot again keeps what was restored; another replaces it and its offers.
	if err := MarkRestored(dirs.State, []string{"app-htop"}); err != nil {
		t.Fatal(err)
	}
	if err := changeOffers(dirs.State, func(o map[string]offerPrint) { o["app-obs-studio"] = offerPrint{Fingerprint: "x"} }); err != nil {
		t.Fatal(err)
	}
	src.Snapshot = &engine.Snapshot{ID: "1111111111111111"}
	if replaced, err := Keep(dirs.State, src, []string{"app-kate"}, time.Now()); err != nil || replaced != nil {
		t.Fatalf("same snapshot: %v %v", replaced, err)
	}
	if k, _ := ReadKept(dirs.State); !slices.Equal(k.Restored, []string{"app-htop", "app-kate"}) || len(readOffers(dirs.State)) != 1 {
		t.Errorf("restored %q, offers %v", k.Restored, readOffers(dirs.State))
	}
	src2, _ := OpenRecord(rec)
	src2.Snapshot = &engine.Snapshot{ID: "2222222222222222", Hostname: "desktop"}
	replaced, err := Keep(dirs.State, src2, nil, time.Now())
	if err != nil || replaced == nil || replaced.Snapshot.ID != "1111111111111111" {
		t.Fatalf("another snapshot: %+v %v", replaced, err)
	}
	if k, _ := ReadKept(dirs.State); len(k.Restored) != 0 || len(readOffers(dirs.State)) != 0 {
		t.Errorf("restored %q and offers %v carried over to another snapshot", k.Restored, readOffers(dirs.State))
	}
	if err := DropKept(dirs.State); err != nil || HasKept(dirs.State) {
		t.Errorf("drop: %v", err)
	}
}

// offerEnv is a kept record of desktop and the machine it's offered onto.
func offerEnv(t *testing.T) (paths.Dirs, OfferOptions) {
	t.Helper()
	dirs := paths.Dirs{State: t.TempDir()}
	keepFrom(t, dirs, asYouGoRecord(t), "1111111111111111")
	root, run := asYouGoMachine(t)
	return dirs, OfferOptions{
		Dirs:      dirs,
		Here:      &Machine{Root: root, Run: run},
		Installed: []string{"obs-studio", "vim", "htop", "tlp"},
		Facts:     func() (rules.Facts, error) { return rules.Facts{Chassis: "desktop"}, nil },
		Wait:      time.Second,
		Now:       time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC),
		Location:  time.UTC,
	}
}

func TestOfferOncePerBundle(t *testing.T) {
	dirs, opt := offerEnv(t)
	ctx := context.Background()
	hints, err := Offer(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	// htop's settings are here already, tlp is for laptops and vim has no bundle of its own.
	want := []string{"carryon: saved settings for obs-studio (9 Oct) — restore with: carryon restore --bundle app-obs-studio"}
	if !slices.Equal(hints, want) {
		t.Errorf("hints = %q", hints)
	}
	in, _ := state.ReadInbox(dirs)
	if len(in.Entries) != 1 {
		t.Fatalf("inbox = %+v", in)
	}
	e := in.Entries[0]
	wantChanges := []string{
		"2 system files in /etc/obs",
		"restores ~/.config/obs-studio (alice)",
		"enables obs-relay.service",
	}
	if e.ID != state.EntryID(state.KindRestore, "app-obs-studio") || e.App != "obs-studio" || e.By != state.ByHook || !slices.Equal(e.Changes, wantChanges) {
		t.Errorf("entry = %+v", e)
	}
	raw, _ := os.ReadFile(dirs.InboxFile())
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("the inbox holds a file's contents:\n%s", raw)
	}
	if fi, err := os.Stat(offersPath(dirs.State)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("fingerprints: %v %v", fi, err)
	}

	// Installing it again offers nothing new; neither does an ignored or restored bundle.
	if hints, err := Offer(ctx, opt); err != nil || len(hints) != 0 {
		t.Errorf("offered again: %q %v", hints, err)
	}
	if _, err := state.UpdateInbox(dirs, time.Second, func(in *state.Inbox) error {
		in.IgnoredBundles = append(in.IgnoredBundles, "app-kate")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SettleOffer(dirs, "app-obs-studio"); err != nil {
		t.Fatal(err)
	}
	opt.Installed = []string{"obs-studio", "kate"}
	if hints, err := Offer(ctx, opt); err != nil || len(hints) != 0 {
		t.Errorf("offered a restored or ignored bundle: %q %v", hints, err)
	}
	if in, _ := state.ReadInbox(dirs); len(in.Entries) != 0 {
		t.Errorf("inbox = %+v", in.Entries)
	}
}

func TestOfferWithoutAKeptRecord(t *testing.T) {
	_, opt := offerEnv(t)
	opt.Dirs = paths.Dirs{State: t.TempDir()}
	if hints, err := Offer(context.Background(), opt); err != nil || hints != nil {
		t.Errorf("%q %v", hints, err)
	}
	if _, err := os.Stat(opt.Dirs.InboxFile()); err == nil {
		t.Error("an inbox was written")
	}
}

func TestOfferGivesUpWhenTheInboxIsBusy(t *testing.T) {
	dirs, opt := offerEnv(t)
	opt.Wait = 50 * time.Millisecond
	release, held, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = state.UpdateInbox(dirs, time.Second, func(*state.Inbox) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	start := time.Now()
	_, err := Offer(context.Background(), opt)
	close(release)
	<-done
	if !errors.Is(err, state.ErrInboxBusy) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
}

func TestCheckOffer(t *testing.T) {
	dirs, opt := offerEnv(t)
	ctx := context.Background()
	src, _, err := OpenKept(dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	plan := func() *Plan {
		p, _, err := PlanOffer(ctx, src, opt.Here, "app-obs-studio")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := CheckOffer(dirs, "app-obs-studio", plan(), opt.Now); !errors.Is(err, ErrNoOffer) {
		t.Errorf("before any offer: %v", err)
	}
	if _, err := Offer(ctx, opt); err != nil {
		t.Fatal(err)
	}
	p := plan()
	if p.Todo() != 4 || len(p.Steps[PhasePackages]) != 0 {
		t.Errorf("the offer's plan installs software or misses settings: %+v", p.Steps)
	}
	if err := CheckOffer(dirs, "app-obs-studio", p, opt.Now); err != nil {
		t.Errorf("same plan: %v", err)
	}

	// b.conf appears here: the plan differs from the offer, which is renewed.
	writeFile(t, filepath.Join(opt.Here.Root, "etc/obs/b.conf"), "b\n")
	p = plan()
	if err := CheckOffer(dirs, "app-obs-studio", p, opt.Now); !errors.Is(err, ErrOfferChanged) {
		t.Errorf("changed plan: %v", err)
	}
	in, _ := state.ReadInbox(dirs)
	if len(in.Entries) != 1 || in.Entries[0].Changes[0] != "replaces /etc/obs/a.conf" || !in.Entries[0].Found.Equal(opt.Now) {
		t.Errorf("renewed offer = %+v", in.Entries)
	}
	if err := CheckOffer(dirs, "app-obs-studio", p, opt.Now); err != nil {
		t.Errorf("the renewed offer: %v", err)
	}
}

func TestChangesSayPlacesAndCounts(t *testing.T) {
	alice := &LocalUser{Name: "alice", Home: "/home/alice"}
	p := &Plan{Steps: map[Phase][]Step{
		PhaseFiles: {
			{Item: recItem("file", "/etc/a/1.conf"), State: StateNew},
			{Item: recItem("file", "/etc/a/2.conf"), State: StateReplace},
			{Item: recItem("file", "/etc/b.conf"), State: StateNew},
		},
		PhaseReview:   {{Item: recItem("file", "/etc/fstab")}},
		PhaseAccount:  {{Item: recItem("group", "video"), User: alice}, {Item: recItem("shell", "/usr/bin/zsh"), User: alice}},
		PhaseServices: {{Item: recItem("mask", "foo.service")}},
		PhaseDirs:     {{Item: recItem("mkdir", "/home/alice/Sync"), User: alice, Target: "/home/alice/Sync"}},
		PhaseUserServices: {
			{Item: recItem("usvc", "sync.service"), User: alice},
			{Item: recItem("uunit", "sync.service"), User: alice, State: StateNew},
		},
		PhaseBoot: {{Item: recItem("kparam", "quiet")}},
	}}
	want := []string{
		"2 system files in /etc/a",
		"adds /etc/b.conf",
		"adds alice to the group video",
		"sets alice's login shell to /usr/bin/zsh",
		"masks foo.service",
		"creates the folder ~/Sync (alice)",
		"enables the user unit sync.service (alice)",
		"adds the user unit file sync.service (alice)",
		"adds the kernel parameter quiet",
	}
	if got := p.Changes(0); !slices.Equal(got, want) {
		t.Errorf("Changes =\n%s", strings.Join(got, "\n"))
	}
	if got := p.Changes(4); len(got) != 4 || got[3] != "and 6 more changes" {
		t.Errorf("Changes(4) = %q", got)
	}
}

func TestRunningOf(t *testing.T) {
	root := t.TempDir()
	proc := func(pid, uid, exe, argv0 string) {
		dir := filepath.Join(root, "proc", pid)
		writeFile(t, filepath.Join(dir, "status"), "Name:\tx\nUid:\t"+uid+"\t"+uid+"\t"+uid+"\t"+uid+"\n")
		writeFile(t, filepath.Join(dir, "cmdline"), argv0+"\x00--flag\x00")
		if exe != "" {
			if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
	}
	proc("10", "1000", "/usr/bin/bash", "-bash")
	proc("11", "1001", "/usr/bin/obs", "obs")                 // bob's
	proc("12", "1000", "/usr/lib/electron/electron", "obs")   // started through a wrapper
	writeFile(t, filepath.Join(root, "proc/self/status"), "") // not a process number
	progs := map[string]bool{"/usr/bin/obs": true, "/usr/lib/obs/libobs.so": true}
	names := map[string]bool{"obs": true}
	if prog, ok := runningOf(root, 1000, progs, names); !ok || prog != "obs" {
		t.Errorf("alice runs %q, %v", prog, ok)
	}
	if prog, ok := runningOf(root, 1002, progs, names); !ok || prog != "" {
		t.Errorf("nobody runs %q, %v", prog, ok)
	}
	if _, ok := runningOf(filepath.Join(root, "missing"), 1000, progs, names); ok {
		t.Error("an unreadable /proc counts as known")
	}
}

func recItem(typ, id string) record.Item { return record.Item{Type: typ, ID: id} }

// Restoring one app's bundle mid-session is the point of restoring as you go: its home files wait
// for the app to close, not for the user to sign out.
func TestApplyAppWaitsForTheAppNotTheDesktop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{{"the app is closed", false}, {"the app is running", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dirs, opt := offerEnv(t)
			ctx := context.Background()
			src, _, err := OpenKept(dirs.State)
			if err != nil {
				t.Fatal(err)
			}
			plan, pl, err := PlanOffer(ctx, src, opt.Here, "app-obs-studio")
			if err != nil {
				t.Fatal(err)
			}
			root := opt.Here.Root
			if tc.running {
				writeFile(t, filepath.Join(root, "proc/4242/status"), "Uid:\t1000\t1000\t1000\t1000\n")
				writeFile(t, filepath.Join(root, "proc/4242/cmdline"), "/usr/bin/obs\x00")
			} else {
				mkdirAll(t, filepath.Join(root, "proc"))
			}
			rec := newRec([]string{"loginctl"}, nil, map[string]string{
				"loginctl list-sessions --no-legend":                "      2 1000 alice seat0 tty1\n",
				"loginctl show-session 2 -p Type -p Class -p State": "Type=wayland\nClass=user\nState=active\n",
				"pacman -Ql obs-studio":                             "obs-studio /usr/\nobs-studio /usr/bin/\nobs-studio /usr/bin/obs\n",
			})
			pl.Here.Run = rec
			a := &Applier{
				Source: src, Here: pl.Here, Packages: pacman.New(rec, pacman.Options{Root: root}),
				Repo: &backupRepo{log: rec.log, files: map[string]string{"/home/alice/.config/obs-studio/basic.ini": "[General]\n"}},
				Yes:  true, Chown: func(string, int, int) error { return nil }, TempDir: t.TempDir(),
				AppPackages: []string{"obs-studio"},
			}
			res := a.Apply(ctx, plan)
			_, err = os.Stat(filepath.Join(root, "home/alice/.config/obs-studio/basic.ini"))
			if restored := err == nil; restored == tc.running {
				t.Errorf("restored = %v with obs running = %v; problems %q", restored, tc.running, res.Problems)
			}
			held := slices.ContainsFunc(res.Problems, func(p string) bool {
				return strings.Contains(p, "alice's settings for obs weren't restored, because obs was running: close it")
			})
			if held != tc.running {
				t.Errorf("problems = %q", res.Problems)
			}
			if slices.Contains(rec.log.lines, "loginctl list-sessions --no-legend") {
				t.Error("the desktop session was checked instead of the app")
			}
		})
	}
}

// alice has no account here and two people could receive her files: without asking, nobody does,
// so her folder isn't offered.
func TestOfferLeavesOutHomesNobodyReceives(t *testing.T) {
	dirs, opt := offerEnv(t)
	writeFile(t, filepath.Join(opt.Here.Root, "etc/passwd"),
		"root:x:0:0::/root:/usr/bin/bash\nbob:x:1000:1000::/home/bob:/usr/bin/bash\ncarol:x:1001:1001::/home/carol:/usr/bin/bash\n")
	if _, err := Offer(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	in, _ := state.ReadInbox(dirs)
	if len(in.Entries) != 1 || !slices.Equal(in.Entries[0].Changes, []string{"2 system files in /etc/obs", "enables obs-relay.service"}) {
		t.Errorf("inbox = %+v", in.Entries)
	}
}
