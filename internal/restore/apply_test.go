package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Evimore/carryon/internal/capture"
	"github.com/Evimore/carryon/internal/catalog"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/engine"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/pkgmgr/pacman"
	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/sysexec"
	"github.com/Evimore/carryon/internal/ui"
)

// events is the ordered log of everything an apply ran: programs and restic commands.
type events struct {
	mu    sync.Mutex
	lines []string
}

func (e *events) add(line string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, line)
}

// recRunner is a runner that succeeds unless told otherwise, and logs every command. Commands run
// as a user are logged as "as NAME: command".
type recRunner struct {
	log      *events
	user     string
	programs map[string]bool
	fail     map[string]bool   // command lines that fail
	out      map[string]string // what quiet commands print
}

func newRec(programs []string, fail []string, out map[string]string) *recRunner {
	r := &recRunner{log: &events{}, programs: map[string]bool{}, fail: map[string]bool{}, out: out}
	for _, p := range programs {
		r.programs[p] = true
	}
	for _, f := range fail {
		r.fail[f] = true
	}
	return r
}

func (r *recRunner) line(name string, args []string) string {
	l := sysexec.Join(append([]string{name}, args...))
	if r.user != "" {
		l = "as " + r.user + ": " + l
	}
	return l
}

func (r *recRunner) result(l string, name string, args []string) error {
	if r.fail[l] {
		return &sysexec.ExitError{Args: append([]string{name}, args...), Code: 1, Status: "exit status 1"}
	}
	return nil
}

func (r *recRunner) Run(_ context.Context, name string, args ...string) error {
	l := r.line(name, args)
	r.log.add(l)
	return r.result(l, name, args)
}

func (r *recRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	l := r.line(name, args)
	r.log.add(l)
	return []byte(r.out[l]), r.result(l, name, args)
}

func (r *recRunner) Exists(name string) bool { return r.programs[name] }

func (r *recRunner) AsUser(u sysexec.User) sysexec.Runner {
	c := *r
	c.user = u.Name
	return &c
}

// backupRepo is a backup with one snapshot, whose files are given as "path": "content" (or
// "path -> target" for symlinks). Restoring writes the files the include file matches.
type backupRepo struct {
	log   *events
	files map[string]string
	fail  bool
}

func (f *backupRepo) Snapshots(_ context.Context, host string) ([]engine.Snapshot, error) {
	return []engine.Snapshot{{ID: "5e1f0c0ffee5", ShortID: "5e1f0c0f", Hostname: host}}, nil
}

func (f *backupRepo) Run(_ context.Context, args ...string) (engine.Result, error) {
	f.log.add("restic " + strings.Join(args, " "))
	if len(args) < 6 || args[0] != "restore" || args[2] != "--target" || args[4] != "--include-file" {
		return engine.Result{Code: 1, Stderr: "unexpected restic command"}, nil
	}
	target, list := args[3], args[5]
	b, err := os.ReadFile(list)
	if err != nil {
		return engine.Result{}, err
	}
	pats := nonEmptyLines(b)
	for _, p := range pats {
		f.log.add("  include " + p)
	}
	if f.fail {
		return engine.Result{Code: 1, Stderr: "Fatal: unable to open repository"}, nil
	}
	names := make([]string, 0, len(f.files))
	for n := range f.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p, link, isLink := strings.Cut(n, " -> ")
		if !matchesAny(pats, p) {
			continue
		}
		dst := filepath.Join(target, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return engine.Result{}, err
		}
		os.Remove(dst)
		if isLink {
			err = os.Symlink(link, dst)
		} else {
			err = os.WriteFile(dst, []byte(f.files[n]), 0o644)
		}
		if err != nil {
			return engine.Result{}, err
		}
	}
	return engine.Result{}, nil
}

// matchesAny reports whether p, or a folder holding it, matches one of restic's include patterns.
func matchesAny(pats []string, p string) bool {
	for _, pat := range pats {
		for a := p; a != "/"; a = path.Dir(a) {
			if ok, _ := path.Match(pat, a); ok {
				return true
			}
		}
	}
	return false
}

// owners records Chown calls by system path.
type owners struct {
	root string
	mu   sync.Mutex
	set  map[string]string
}

func (o *owners) chown(p string, uid, gid int) error {
	if _, err := os.Lstat(p); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	rel, _ := filepath.Rel(o.root, p)
	o.set["/"+rel] = fmt.Sprintf("%d:%d", uid, gid)
	return nil
}

// entry is a path in a fake tree, for comparing before and after.
type entry struct {
	mode fs.FileMode
	link string
	data []byte
}

func snapshotTree(t *testing.T, root string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := entry{mode: fi.Mode()}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			e.link, _ = os.Readlink(p)
		case fi.Mode().IsRegular():
			e.data, _ = os.ReadFile(p)
		}
		out["/"+rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// describeEntry shows a path's type, mode and owner, and a short file's content.
func describeEntry(e entry, owner string) string {
	var b strings.Builder
	switch {
	case e.mode&os.ModeSymlink != 0:
		b.WriteString("-> " + e.link)
	case e.mode.IsDir():
		fmt.Fprintf(&b, "folder %04o", e.mode.Perm())
	default:
		fmt.Fprintf(&b, "file %04o", e.mode.Perm())
	}
	if owner != "" {
		b.WriteString("  owner " + owner)
	}
	if e.mode.IsRegular() {
		if len(e.data) <= 400 && utf8.Valid(e.data) && !bytes.ContainsRune(e.data, 0) {
			for _, l := range strings.SplitAfter(string(e.data), "\n") {
				if l != "" {
					b.WriteString("\n    | " + strings.TrimSuffix(l, "\n"))
				}
			}
		} else {
			fmt.Fprintf(&b, "  (%d bytes, sha256 %x)", len(e.data), sha256.Sum256(e.data))
		}
	}
	return b.String()
}

// treeChanges lists what differs between two snapshots of a tree, and every path chowned.
func treeChanges(before, after map[string]entry, own map[string]string) string {
	var paths []string
	for p := range after {
		paths = append(paths, p)
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		old, had := before[p]
		cur, has := after[p]
		switch {
		case !has:
			fmt.Fprintf(&b, "- %s\n", p)
		case !had:
			fmt.Fprintf(&b, "+ %s  %s\n", p, describeEntry(cur, own[p]))
		case old.mode != cur.mode || old.link != cur.link || !bytes.Equal(old.data, cur.data) || own[p] != "":
			fmt.Fprintf(&b, "~ %s  %s\n", p, describeEntry(cur, own[p]))
		}
	}
	return b.String()
}

// applyCase is a plan from plan_test.go's scenarios applied to its fake tree.
type applyCase struct {
	scenario string
	extra    map[string]string // files added to the tree before applying; "a -> b" makes a symlink
	programs []string          // programs that are installed
	fail     []string          // command lines that fail
	out      map[string]string // what quiet commands print
	answers  []any             // answers to questions while applying; nil: the defaults
	yes      bool
}

// The desktop's backup: what the data phase restores.
var desktopBackup = map[string]string{
	"/etc/ssh/ssh_host_ed25519_key":        "test host key\n",
	"/etc/ssh/ssh_host_ed25519_key.pub":    "ssh-ed25519 AAAAtest root@desktop\n",
	"/root/.bashrc":                        "export EDITOR=vim\n",
	"/home/alice/.config/kitty/kitty.conf": "include /home/alice/.config/kitty/theme.conf\nfont_size 11\n",
	"/home/alice/.config/kitty/theme.conf": "background #000000\n",
	"/home/alice/.config/kitty/current -> /home/alice/.config/kitty/theme.conf": "",
	"/home/alice/.local/share/fonts/Example.ttf":                                "\x00\x01font /home/alice\x00",
	"/home/alice/.config/other/untouched.conf":                                  "not picked\n",
}

var pacmanConf = "[options]\nArchitecture = auto\n\n[core]\nInclude = /etc/pacman.d/mirrorlist\n\n[extra]\nInclude = /etc/pacman.d/mirrorlist\n"

var applyCases = map[string]applyCase{
	// Everything goes: paru is there, alice is signed in (her session bus is up), and the kernel
	// command line is in /etc/kernel/cmdline, so the initramfs is made again.
	"same-machine": {
		scenario: "same-machine",
		extra: map[string]string{
			"etc/pacman.conf": pacmanConf, "run/user/1000/bus": "", "usr/bin/zsh": "",
		},
		programs: []string{"paru", "mkinitcpio", "loginctl", "flatpak", "npm"},
		out: map[string]string{
			"loginctl list-sessions --no-legend":                "      3 1000 alice seat0 tty2\n",
			"loginctl show-session 3 -p Type -p Class -p State": "Type=tty\nClass=user\nState=active\n",
		},
		yes: true,
	},
	// A new laptop: no AUR helper (one is installed from the repositories), limine, alice not
	// signed in, and failures along the way that the rest of the restore goes on past. The
	// initramfs config is for review only: the laptop needs the encrypt hook.
	"different-machine": {
		scenario: "different-machine",
		extra:    map[string]string{"etc/pacman.conf": pacmanConf},
		programs: []string{"limine-update", "mkinitcpio", "rustup", "flatpak"},
		fail: []string{
			"pacman -Syu --needed mypkg",
			"pacman -S --needed --noconfirm mypkg",
			"systemctl enable -- sshd.service",
			"as alice: cargo install --locked ripgrep",
		},
		answers: []any{true},
	},
	// bob's machine: alice's files go to bob, whose home is /srv/home/bob, so paths and User=
	// lines are adapted.
	"other-user": {
		scenario: "other-user",
		extra: map[string]string{
			"etc/pacman.conf": pacmanConf, "run/user/1001/bus": "",
		},
		programs: []string{"paru", "mkinitcpio"},
		yes:      true,
	},
}

func addExtra(t *testing.T, root string, extra map[string]string) {
	t.Helper()
	for name, body := range extra {
		if from, to, ok := strings.Cut(name, " -> "); ok {
			p := filepath.Join(root, from)
			mkdirAll(t, filepath.Dir(p))
			if err := os.Symlink(to, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeFile(t, filepath.Join(root, name), body)
	}
}

// applied is everything an apply did, for golden files and checks.
type applied struct {
	res    *Result
	events []string
	before map[string]entry
	after  map[string]entry
	owners map[string]string
	out    string
	root   string
}

// render is what an apply golden file pins.
func (ap applied) render(tmp string) string {
	var b strings.Builder
	b.WriteString("== commands\n")
	for _, l := range ap.events {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n== changed files\n")
	b.WriteString(treeChanges(ap.before, ap.after, ap.owners))
	b.WriteString("\n== problems\n")
	for _, p := range ap.res.Problems {
		b.WriteString("- " + p + "\n")
	}
	b.WriteString("\n== notes\n")
	for _, n := range ap.res.Notes {
		fmt.Fprintf(&b, "%s: %s\n", n.App, n.Text)
	}
	if ap.res.NotBackedUp != "" {
		b.WriteString("re-download list: " + ap.res.NotBackedUp + "\n")
	}
	b.WriteString("\n== output\n" + ap.out)
	return normalize(b.String(), ap.root, tmp)
}

var tempName = regexp.MustCompile(`carryon-(restore|aur)-\d+`)

func normalize(s, root, tmp string) string {
	s = strings.ReplaceAll(s, root, "$ROOT")
	s = strings.ReplaceAll(s, tmp, "$TMP")
	return tempName.ReplaceAllString(s, "carryon-$1-N")
}

// testCatalog has KDE Wallet's note, and one for kitty.
func testCatalog() *catalog.Catalog {
	return &catalog.Catalog{Apps: []catalog.App{
		{ID: "kitty", Name: "kitty", Recognize: catalog.Recognize{Paths: []string{".config/kitty"}},
			Notes: []catalog.Note{{Text: "kitty reads its settings when it starts: open a new window."}}},
		{ID: "kwallet", Name: "KDE Wallet", Recognize: catalog.Recognize{Paths: []string{".local/share/kwalletd"}},
			Notes: []catalog.Note{{Text: "KDE Wallet opens with its old password."}}},
	}}
}

// runApply applies a plan to the tree it was made for.
func runApply(t *testing.T, plan *Plan, pd planned, ac applyCase, files map[string]string) (applied, string) {
	t.Helper()
	addExtra(t, pd.root, ac.extra)
	rec := newRec(ac.programs, ac.fail, ac.out)
	tmp := t.TempDir()
	own := &owners{root: pd.root, set: map[string]string{}}
	var out bytes.Buffer
	var ask ui.Prompter = ui.Defaults{}
	script := &ui.Script{Answers: ac.answers}
	if ac.answers != nil {
		ask = script
	}
	a := &Applier{
		Source: pd.src,
		Here: &Machine{Root: pd.root, Hostname: pd.host, Run: rec, Users: pd.local,
			Packages: fakePackages{}},
		Packages: pacman.New(rec, pacman.Options{Root: pd.root}),
		Repo:     &backupRepo{log: rec.log, files: files},
		Ask:      ask,
		Log:      ui.NewLogger(&out, false, false),
		Out:      &out,
		Yes:      ac.yes,
		Catalog:  testCatalog(),
		Chown:    own.chown,
		TempDir:  tmp,
	}
	before := snapshotTree(t, pd.root)
	res := a.Apply(context.Background(), plan)
	if len(script.Answers) > 0 {
		t.Errorf("unused answers: %v", script.Answers)
	}
	return applied{
		res: res, events: rec.log.lines, before: before, after: snapshotTree(t, pd.root),
		owners: own.set, out: out.String(), root: pd.root,
	}, tmp
}

func TestApplyGolden(t *testing.T) {
	for name, ac := range applyCases {
		t.Run(name, func(t *testing.T) {
			plan, _, pd := planOn(t, "testdata/desktop", scenarios[ac.scenario])
			ap, tmp := runApply(t, plan, pd, ac, desktopBackup)
			checkGolden(t, "apply-"+name, ap.render(tmp))
			checkNeverWritten(t, pd.src, plan, ap)
		})
	}
}

// checkNeverWritten fails if a review-only file was copied from the backup, or changed in any
// other way than kernel parameters being added to the bootloader config, keeping the old one as
// *.pre-restore.
func checkNeverWritten(t *testing.T, src *Source, plan *Plan, ap applied) {
	t.Helper()
	cmdline := map[string]bool{}
	for _, s := range capture.CmdlineSources {
		cmdline[s.Path] = true
	}
	for p, e := range ap.after {
		base := strings.TrimSuffix(p, PreRestore)
		if !ReviewOnly(base) {
			continue
		}
		saved, _ := os.ReadFile(filepath.Join(src.Dir, FilesDir, base))
		if len(saved) > 0 && bytes.Equal(e.data, saved) {
			t.Errorf("%s holds the backup's copy", p)
		}
		old, had := ap.before[p]
		if had && old.link == e.link && bytes.Equal(old.data, e.data) {
			continue
		}
		orig := ap.before[base]
		switch {
		case !cmdline[base]:
			t.Errorf("%s was written", p)
		case p != base && !bytes.Equal(e.data, orig.data):
			t.Errorf("%s isn't what %s was", p, base)
		case p == base:
			got := string(e.data)
			for _, s := range plan.Steps[PhaseBoot] {
				got = strings.Replace(got, " "+s.Item.ID, "", 1)
			}
			if got != string(orig.data) {
				t.Errorf("%s changed beyond added kernel parameters:\n%s", p, e.data)
			}
		}
	}
	for p := range ap.before {
		if _, ok := ap.after[p]; !ok && ReviewOnly(p) {
			t.Errorf("%s was removed", p)
		}
	}
}

// TestApplyKeepsGoing pins that failing steps don't stop the ones after them.
func TestApplyKeepsGoing(t *testing.T) {
	ac := applyCases["different-machine"]
	plan, _, pd := planOn(t, "testdata/desktop", scenarios[ac.scenario])
	ap, _ := runApply(t, plan, pd, ac, desktopBackup)
	if len(ap.res.Problems) < len(ac.fail) {
		t.Errorf("problems = %q", ap.res.Problems)
	}
	// The last phase still ran, and so did the files after the failed package install.
	for _, want := range []string{"as alice: pipx install black", "limine-update"} {
		if !slices.Contains(ap.events, want) {
			t.Errorf("%q didn't run after the failures:\n%s", want, strings.Join(ap.events, "\n"))
		}
	}
	if _, ok := ap.after["/etc/locale.conf"]; !ok {
		t.Error("/etc/locale.conf wasn't written after the failures")
	}
}

// tamperedSource is a record with files that must never be written, copied into a temp folder so
// the test can add its own items.
func tamperedSource(t *testing.T, extra map[string]string) *Source {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/desktop")); err != nil {
		t.Fatal(err)
	}
	for name, body := range extra {
		writeFile(t, filepath.Join(dir, FilesDir, name), body)
	}
	src, err := OpenRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestApplyNeverWritesReviewOnlyFiles(t *testing.T) {
	ctx := context.Background()
	evil := "UUID=00000000-0000-0000-0000-000000000000 / ext4 defaults 0 1\n"
	src := tamperedSource(t, map[string]string{
		"etc/crypttab": evil, "etc/kernel/cmdline": evil, "etc/default/grub": evil,
		"usr/local/link/fstab": evil, "usr/local/link/default/limine": evil,
	})
	root := buildTree(t, "here-laptop.txt")
	addExtra(t, root, map[string]string{"usr/local/link -> /etc": "", "usr/local/up -> ../../etc/kernel": ""})
	writeFile(t, filepath.Join(src.Dir, FilesDir, "usr/local/up/cmdline"), evil)
	local, err := ReadUsers(root)
	if err != nil {
		t.Fatal(err)
	}
	alice := local[len(local)-1]
	users := []UserMap{{From: record.User{Name: "alice", Home: "/home/alice"}, To: alice}}
	file := func(id string) Step {
		return Step{Item: record.Item{Type: record.TypeFile, ID: id, Mode: "644", Owner: "root:root"}, Target: id}
	}
	// A plan changed by hand: review-only files, and the initramfs config the laptop must not
	// get (it drops the encrypt hook), moved into the files phase; paths that reach them through
	// links; data and user units that would cover them.
	plan := &Plan{From: "desktop", Users: users, mainUser: "alice", Steps: map[Phase][]Step{
		PhaseFiles: {
			file("/etc/fstab"), file("/etc/crypttab"), file("/etc/kernel/cmdline"), file("/etc/default/grub"),
			file("/etc/mkinitcpio.conf"), file("/usr/local/link/fstab"), file("/usr/local/link/default/limine"),
			file("/usr/local/up/cmdline"), file("/etc/../etc/fstab"),
		},
		PhaseData: {
			{Item: record.Item{Type: record.TypePath, ID: "/etc"}},
			{Item: record.Item{Type: record.TypePath, ID: "/etc/f*"}},
			{Item: record.Item{Type: record.TypePath, ID: "/"}},
			{Item: record.Item{Type: record.TypePath, ID: "/etc/mkinitcpio.conf.d"}},
		},
		PhaseUserServices: {
			{Item: record.Item{Type: record.TypeUunit, ID: "../../../../etc/fstab"}, User: &alice},
		},
	}}
	rec := newRec(nil, nil, nil)
	repo := &backupRepo{log: rec.log, files: map[string]string{"/etc/fstab": evil, "/etc/mkinitcpio.conf": evil}}
	a := &Applier{Source: src, Here: &Machine{Root: root, Run: rec, Users: local},
		Packages: pacman.New(rec, pacman.Options{Root: root}), Repo: repo,
		Chown: (&owners{root: root, set: map[string]string{}}).chown, TempDir: t.TempDir()}
	before := snapshotTree(t, root)
	res := a.Apply(ctx, plan)
	after := snapshotTree(t, root)
	for _, p := range []string{"/etc/fstab", "/etc/crypttab", "/etc/kernel/cmdline", "/etc/default/grub",
		"/etc/default/limine", "/etc/mkinitcpio.conf", "/etc/mkinitcpio.conf.d/10-encrypt.conf"} {
		b, a := before[p], after[p]
		if !bytes.Equal(b.data, a.data) || b.link != a.link {
			t.Errorf("%s was written", p)
		}
		if _, ok := after[p+PreRestore]; ok {
			t.Errorf("%s was moved away", p)
		}
	}
	for p, e := range after {
		if bytes.Contains(e.data, []byte("UUID=0000")) {
			t.Errorf("%s holds the backup's disk IDs", p)
		}
	}
	if slices.ContainsFunc(rec.log.lines, func(l string) bool { return strings.HasPrefix(l, "restic") }) {
		t.Errorf("restic ran: %q", rec.log.lines)
	}
	if got, want := len(res.Problems), 14; got != want {
		t.Errorf("%d problems, want %d:\n%s", got, want, strings.Join(res.Problems, "\n"))
	}
	for _, want := range []string{
		"/etc/fstab was not copied: disks have other IDs",
		"/etc/mkinitcpio.conf was not copied: this machine needs initramfs hooks the backup lacks (encrypt",
		"/usr/local/link/fstab was not copied: a link here points it at /etc/fstab, which is never copied",
		"/usr/local/up/cmdline was not copied: a link here points it at /etc/kernel/cmdline",
		"/etc/../etc/fstab was not copied: it isn't a system file",
		"/etc wasn't restored: it would write /etc/crypttab",
		"/etc/f* wasn't restored: it would write /etc/fstab",
		"user unit ../../../../etc/fstab wasn't written",
	} {
		if !slices.ContainsFunc(res.Problems, func(p string) bool { return strings.Contains(p, want) }) {
			t.Errorf("no problem says %q:\n%s", want, strings.Join(res.Problems, "\n"))
		}
	}
}

func TestApplyKeepsTheFirstPreRestoreCopy(t *testing.T) {
	ac := applyCases["same-machine"]
	plan, _, pd := planOn(t, "testdata/desktop", scenarios[ac.scenario])
	runApply(t, plan, pd, ac, desktopBackup)
	first, err := os.ReadFile(filepath.Join(pd.root, "etc/mkinitcpio.conf"+PreRestore))
	if err != nil {
		t.Fatal(err)
	}
	// The same plan again: what the first restore wrote is replaced, the original copy stays.
	runApply(t, plan, pd, applyCase{programs: ac.programs, yes: true}, desktopBackup)
	again, err := os.ReadFile(filepath.Join(pd.root, "etc/mkinitcpio.conf"+PreRestore))
	if err != nil || !bytes.Equal(first, again) {
		t.Errorf("the first copy was lost: %q, then %q (%v)", first, again, err)
	}
}

func TestApplyWaitsForTheDesktopToClose(t *testing.T) {
	session := map[string]string{
		"loginctl list-sessions --no-legend":                "      2 1000 alice seat0 tty1\n      5 1001 bob - pts/0\n",
		"loginctl show-session 2 -p Type -p Class -p State": "Type=wayland\nClass=user\nState=active\n",
	}
	for _, tc := range []struct {
		name    string
		answers []any
		home    bool
	}{
		{"without a terminal", nil, false},
		{"the user says no", []any{false}, false},
		{"the user says yes", []any{true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := scenarios["same-machine"]
			sc.pick = func([]Tick) Picked { return Picked{Bundles: []string{"home-apps"}} }
			plan, _, pd := planOn(t, "testdata/desktop", sc)
			ap, _ := runApply(t, plan, pd, applyCase{programs: []string{"loginctl"}, out: session, answers: tc.answers}, desktopBackup)
			_, restored := ap.after["/home/alice/.config/kitty/kitty.conf"]
			if restored != tc.home {
				t.Errorf("kitty.conf restored = %v, want %v", restored, tc.home)
			}
			said := slices.ContainsFunc(ap.res.Problems, func(p string) bool {
				return strings.Contains(p, "alice's home files weren't restored, because alice was signed in to a desktop")
			})
			if said == tc.home {
				t.Errorf("problems = %q", ap.res.Problems)
			}
			if !strings.Contains(ap.out, "alice is signed in to a desktop") {
				t.Errorf("output:\n%s", ap.out)
			}
		})
	}
}

func TestApplyDataWithoutABackup(t *testing.T) {
	sc := scenarios["same-machine"]
	sc.pick = func([]Tick) Picked { return Picked{Bundles: []string{"home-apps", "root-home"}} }
	plan, _, pd := planOn(t, "testdata/desktop", sc)
	a := &Applier{Source: pd.src, Here: &Machine{Root: pd.root, Run: newRec(nil, nil, nil)}}
	res := a.Apply(context.Background(), plan)
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "the backup isn't open") {
		t.Errorf("problems = %q", res.Problems)
	}
}

func TestMergeRclone(t *testing.T) {
	for _, tc := range []struct {
		name, keep, old, want string
		added                 int
	}{
		{"adds the other remotes",
			"[cloud]\ntype = webdav\n",
			"[cloud]\ntype = webdav\nurl = old\n\n[photos]\ntype = s3\n\n[music]\ntype = sftp\n",
			"[cloud]\ntype = webdav\n\n[photos]\ntype = s3\n\n[music]\ntype = sftp\n", 2},
		{"nothing new", "[cloud]\ntype = webdav\n", "[cloud]\ntype = drive\n", "[cloud]\ntype = webdav\n", 0},
		{"encrypted configs are left alone", "[cloud]\n", "# Encrypted rclone configuration File\n\nRCLONE_ENCRYPT_V0:\nabc\n", "[cloud]\n", 0},
		{"text before the first remote is dropped", "[a]\n", "# comment\n[b]\nx = 1\n", "[a]\n\n[b]\nx = 1\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := mergeRclone([]byte(tc.keep), []byte(tc.old))
			if string(got) != tc.want || n != tc.added {
				t.Errorf("got %d:\n%s\nwant %d:\n%s", n, got, tc.added, tc.want)
			}
		})
	}
}

func TestResolveStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	addExtra(t, root, map[string]string{
		"etc/hostname": "laptop\n",
		"a -> /etc":    "", "b -> a/../usr": "", "c -> ../../../../etc": "", "loop -> loop": "",
		"usr/x -> ../etc/default": "",
	})
	m := &Machine{Root: root}
	for _, tc := range []struct{ in, want string }{
		{"/a/fstab", "/etc/fstab"},
		{"/a/new/file", "/etc/new/file"},
		{"/b/x/grub", "/etc/default/grub"},
		{"/c/fstab", "/etc/fstab"},
		{"/a", "/a"}, // the name itself isn't followed
		{"/usr/bin/zsh", "/usr/bin/zsh"},
	} {
		got, err := m.resolve(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("resolve(%s) = %q, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := m.resolve("/loop/x"); err == nil {
		t.Error("a symlink loop resolved")
	}
}

func TestCoversProtected(t *testing.T) {
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{"/", true}, {"/etc", true}, {"/etc/*", true}, {"/etc/fstab", true}, {"/etc/f*", true},
		{"/etc/default", true}, {"/etc/mkinitcpio.d/linux.preset", true},
		{"/etc/ssh/ssh_host_*", false}, {"/root", false}, {"/srv", false}, {"/var/lib/fprint", false},
		{"/etc/fstab.d", false}, {"/e*", true},
	} {
		if _, got := coversProtected(tc.p); got != tc.want {
			t.Errorf("coversProtected(%s) = %v", tc.p, got)
		}
	}
}

func TestCopyTreeReplacesLinksInTheWay(t *testing.T) {
	src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "conf/settings"), "new\n")
	writeFile(t, filepath.Join(outside, "settings"), "secret\n")
	// A link where a folder goes, pointing outside: it's replaced, never followed.
	if err := os.Symlink(outside, filepath.Join(dst, "conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := copyTree(filepath.Join(src, "conf"), filepath.Join(dst, "conf")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "settings")); string(b) != "secret\n" {
		t.Errorf("wrote through the link: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "conf/settings")); string(b) != "new\n" {
		t.Errorf("settings = %q", b)
	}
}

// homeStep is a home path for the user who receives alice's files.
func homeStep(u *LocalUser, rel string) Step {
	return Step{Item: record.Item{Type: record.TypePath, ID: "/home/alice/" + rel, Home: rel}, User: u,
		Target: path.Join(u.Home, rel)}
}

func TestApplyKeepsTheRcloneLogin(t *testing.T) {
	root := t.TempDir()
	live := "[cloud]\ntype = webdav\nurl = https://dav.example.com/bob\n"
	addExtra(t, root, map[string]string{
		"etc/passwd":                          "bob:x:1001:1001::/home/bob:/usr/bin/bash\n",
		"home/bob/.config/rclone/rclone.conf": live,
	})
	bob := LocalUser{Name: "bob", UID: 1001, GID: 1001, Home: "/home/bob"}
	plan := &Plan{From: "desktop", mainUser: "alice",
		Users: []UserMap{{From: record.User{Name: "alice", Home: "/home/alice"}, To: bob}},
		Steps: map[Phase][]Step{PhaseData: {homeStep(&bob, ".config/rclone"), homeStep(&bob, ".config/kitty")}}}
	rec := newRec(nil, nil, nil)
	repo := &backupRepo{log: rec.log, files: map[string]string{
		"/home/alice/.config/rclone/rclone.conf": "[cloud]\ntype = webdav\nurl = https://dav.example.com/alice\n\n[photos]\ntype = s3\n",
		"/home/alice/.config/rclone/filters.txt": "- /home/alice/tmp/**\n",
		"/home/alice/.config/kitty/kitty.conf":   "font_size 11\n",
	}}
	own := &owners{root: root, set: map[string]string{}}
	var log bytes.Buffer
	a := &Applier{Source: &Source{Dir: t.TempDir()}, Here: &Machine{Root: root, Run: rec}, Repo: repo,
		Chown: own.chown, TempDir: t.TempDir(), Log: ui.NewLogger(&log, false, false)}
	res := a.Apply(context.Background(), plan)
	if len(res.Problems) > 0 {
		t.Fatalf("problems: %q", res.Problems)
	}
	got, _ := os.ReadFile(filepath.Join(root, "home/bob/.config/rclone/rclone.conf"))
	if want := live + "\n[photos]\ntype = s3\n"; string(got) != want {
		t.Errorf("rclone.conf =\n%s\nwant\n%s", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "home/bob/.config/rclone/filters.txt")); string(b) != "- /home/bob/tmp/**\n" {
		t.Errorf("filters.txt = %q", b)
	}
	if own.set["/home/bob/.config/rclone/rclone.conf"] != "1001:1001" || own.set["/home/bob/.config/kitty/kitty.conf"] != "1001:1001" {
		t.Errorf("owners: %v", own.set)
	}
	if !strings.Contains(log.String(), "kept bob's working rclone config and added 1 other remotes from the backup") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestApplyRefusesLinksOutOfTheHome(t *testing.T) {
	root := t.TempDir()
	addExtra(t, root, map[string]string{
		"etc/shadow":                   "root:secret\n",
		"home/bob/.config/app -> /etc": "",
	})
	bob := LocalUser{Name: "bob", UID: 1001, GID: 1001, Home: "/home/bob"}
	plan := &Plan{From: "desktop", mainUser: "alice",
		Users: []UserMap{{From: record.User{Name: "alice", Home: "/home/alice"}, To: bob}},
		Steps: map[Phase][]Step{
			PhaseData: {homeStep(&bob, ".config/app/shadow")},
			PhaseDirs: {{Item: record.Item{Type: record.TypeMkdir, ID: "/home/alice/.config/app/x", Rel: ".config/app/x"},
				User: &bob, Target: "/home/bob/.config/app/x"}},
		}}
	rec := newRec(nil, nil, nil)
	repo := &backupRepo{log: rec.log, files: map[string]string{"/home/alice/.config/app/shadow": "root:owned\n"}}
	own := &owners{root: root, set: map[string]string{}}
	a := &Applier{Source: &Source{Dir: t.TempDir()}, Here: &Machine{Root: root, Run: rec}, Repo: repo,
		Chown: own.chown, TempDir: t.TempDir()}
	res := a.Apply(context.Background(), plan)
	if b, _ := os.ReadFile(filepath.Join(root, "etc/shadow")); string(b) != "root:secret\n" {
		t.Errorf("/etc/shadow = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/x")); err == nil {
		t.Error("made /etc/x through the link")
	}
	if len(res.Problems) != 2 || len(own.set) != 0 {
		t.Errorf("problems %q, owners %v", res.Problems, own.set)
	}
}

// TestApplyDataFromARealBackup restores from a restic repository made in a temp folder into a
// fake system tree.
func TestApplyDataFromARealBackup(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	// The machine that was backed up lives in tmp/desktop, so that's where its paths are.
	desk := filepath.Join(tmp, "desktop")
	aliceHome := filepath.Join(desk, "home/alice")
	addExtra(t, desk, map[string]string{
		"srv/www/index.html":                                    "hello\n",
		"srv/www/cache/page":                                    "cached\n",
		"home/alice/.config/app/settings.conf":                  "data_dir=" + aliceHome + "/data\n",
		"home/alice/.config/app/data -> " + aliceHome + "/data": "",
		"home/alice/.config/other.conf":                         "not picked\n",
	})
	pw := filepath.Join(tmp, "password")
	writeFile(t, pw, "correct horse battery staple\n")
	e := engine.New(config.Repository{URL: filepath.Join(tmp, "repo"), PasswordFile: pw},
		paths.Dirs{Conf: filepath.Join(tmp, "conf"), Cache: filepath.Join(tmp, "cache")}, "desktop")
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home")}
	var resticOut bytes.Buffer
	e.Stderr = &resticOut
	for _, args := range [][]string{{"init"}, {"backup", "--host", "desktop", desk}} {
		r, err := e.Run(ctx, args...)
		if err == nil {
			err = r.Err()
		}
		if err != nil {
			t.Fatalf("restic %s: %v\n%s", args[0], err, r.Stderr)
		}
	}

	root := t.TempDir()
	addExtra(t, root, map[string]string{
		"etc/passwd": "root:x:0:0::/root:/usr/bin/bash\nhttp:x:33:33::/srv/http:/usr/bin/nologin\nbob:x:1001:1001::/home/bob:/usr/bin/bash\n",
		"etc/group":  "root:x:0:\nhttp:x:33:\nbob:x:1001:\n",
	})
	bob := LocalUser{Name: "bob", UID: 1001, GID: 1001, Home: "/home/bob"}
	// The web folder belonged to http, which had other numbers there.
	uid, gid := os.Getuid(), os.Getgid()
	www := filepath.Join(desk, "srv/www")
	plan := &Plan{From: "desktop", mainUser: "alice",
		Users: []UserMap{{From: record.User{Name: "alice", Home: aliceHome}, To: bob}},
		Steps: map[Phase][]Step{PhaseData: {
			{Item: record.Item{Type: record.TypePath, ID: www, Owner: "http:http", UID: &uid, GID: &gid}, Target: www},
			{Item: record.Item{Type: record.TypePath, ID: aliceHome + "/.config/app", Home: ".config/app"}, User: &bob,
				Target: "/home/bob/.config/app"},
		}}}
	own := &owners{root: root, set: map[string]string{}}
	var log bytes.Buffer
	a := &Applier{Source: &Source{Dir: t.TempDir()}, Here: &Machine{Root: root, Run: newRec(nil, nil, nil)},
		Repo: e, Chown: own.chown, TempDir: t.TempDir(), Log: ui.NewLogger(&log, false, false)}
	res := a.Apply(ctx, plan)
	if len(res.Problems) > 0 {
		t.Fatalf("problems: %q\n%s\n%s", res.Problems, log.String(), resticOut.String())
	}
	for p, want := range map[string]string{
		filepath.Join(www, "index.html"):      "hello\n",
		filepath.Join(www, "cache/page"):      "cached\n",
		"/home/bob/.config/app/settings.conf": "data_dir=/home/bob/data\n",
	} {
		if b, err := os.ReadFile(filepath.Join(root, p)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", p, b, err, want)
		}
	}
	if l, _ := os.Readlink(filepath.Join(root, "home/bob/.config/app/data")); l != "/home/bob/data" {
		t.Errorf("link = %q", l)
	}
	if _, err := os.Lstat(filepath.Join(root, "home/bob/.config/other.conf")); err == nil {
		t.Error("restored a path that wasn't picked")
	}
	for p, want := range map[string]string{
		www + "/index.html":                   "33:33",
		www:                                   "33:33",
		"/home/bob/.config/app/settings.conf": "1001:1001",
		"/home/bob/.config":                   "1001:1001",
	} {
		if own.set[p] != want {
			t.Errorf("owner of %s = %q, want %s", p, own.set[p], want)
		}
	}
	if !strings.Contains(log.String(), "handed back to http:http") {
		t.Errorf("log:\n%s", log.String())
	}
}
