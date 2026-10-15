package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/sysexec"
)

func TestSummaryMatchesPrototype(t *testing.T) {
	// testdata/summary-format1.md is what the prototype's summary_md wrote for the fixture.
	r, err := Load("testdata/format1")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/summary-format1.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Summary(); got != string(want) {
		t.Fatalf("SUMMARY.md differs from the prototype's:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestSummaryFormat2AndLongBundles(t *testing.T) {
	r := &Record{
		Meta: Meta{Format: FormatCurrent, Host: "desktop-7", MachineName: "desktop", Kernel: "6.17.1-arch1-1",
			Users: []User{{Name: "alice", Home: "/home/alice"}, {Name: "bob", Home: "/home/bob"}}},
		Bundles: Bundles{{ID: "packages", Title: "Packages", When: "always"}},
	}
	for i := 0; i < 32; i++ {
		r.Items = append(r.Items, Item{Type: TypePkg, ID: fmt.Sprintf("pkg%02d", i), Bundle: "packages"})
	}
	s := r.Summary()
	for _, want := range []string{
		"# desktop: system record\n",
		"Users `alice`, `bob`, kernel 6.17.1-arch1-1, unknown chassis. **Incomplete** (captured without root).",
		"`packages` · 32 items · when: `always`",
		"- package    pkg29\n- ... 2 more\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}

func TestCommitMessage(t *testing.T) {
	if s, b := commitMessage(nil, true); s != "first record" || b != "" {
		t.Errorf("first = %q, %q", s, b)
	}
	lines := []string{"+ package    vim  [extra]", "- AUR        paru-bin"}
	s, b := commitMessage(lines, false)
	if s != "2 change(s): + package vim [extra], - AUR paru-bin" {
		t.Errorf("subject = %q", s)
	}
	if b != strings.Join(lines, "\n") {
		t.Errorf("body = %q", b)
	}

	var many []string
	for i := 0; i < 400; i++ {
		many = append(many, fmt.Sprintf("+ package    a-package-with-a-really-quite-long-name-%03d", i))
	}
	s, b = commitMessage(many, false)
	if !strings.HasPrefix(s, "400 change(s): + package a-package-with-a-really-quite-long-name-000, ") || len([]rune(s)) != 200 {
		t.Errorf("subject = %q (%d runes)", s, len([]rune(s)))
	}
	if n := len(strings.Split(b, "\n")); n != 300 {
		t.Errorf("body has %d lines, want 300", n)
	}
}

// fixture is a test's record folder, its parent and a clock.
type fixture struct {
	t      *testing.T
	parent string
	dir    string
	now    time.Time
	git    sysexec.Runner
}

func newFixture(t *testing.T, git sysexec.Runner) *fixture {
	parent := t.TempDir()
	return &fixture{t: t, parent: parent, dir: filepath.Join(parent, "record"),
		now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("", 2*3600)), git: git}
}

// realGit returns a runner for the real git, or skips the test.
func realGit(t *testing.T) sysexec.Runner {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed: skipping the history test")
	}
	return sysexec.New()
}

// stage writes the fixture record, changed by edit, to a new stage folder and returns its path.
func (f *fixture) stage(edit func(*Record)) string {
	f.t.Helper()
	r, err := Load("testdata/format1")
	if err != nil {
		f.t.Fatal(err)
	}
	if edit != nil {
		edit(r)
	}
	stage, err := os.MkdirTemp(f.parent, "stage-")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := r.Save(stage); err != nil {
		f.t.Fatal(err)
	}
	write(f.t, filepath.Join(stage, "files", "etc", "locale.conf"), "LANG=en_US.UTF-8\n")
	write(f.t, filepath.Join(stage, "versions.txt"), "base 3-2\n")
	return stage
}

func (f *fixture) publish(stage string) PublishResult {
	f.t.Helper()
	f.now = f.now.Add(time.Hour)
	res, err := Publish(context.Background(), stage, f.dir, PublishOptions{Commit: true, Git: f.git, Now: func() time.Time { return f.now }})
	if err != nil {
		f.t.Fatal(err)
	}
	if exists(stage) {
		f.t.Errorf("the stage %s is still there after publishing", stage)
	}
	return res
}

func (f *fixture) history() []Commit {
	f.t.Helper()
	commits, err := History(context.Background(), f.dir, f.git, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return commits
}

func addVim(r *Record) {
	r.Items = append(r.Items, Item{Type: TypePkg, ID: "vim", Repo: "extra", Bundle: "packages"})
}

func TestPublishCommitsOnlyWhenItemsChange(t *testing.T) {
	f := newFixture(t, realGit(t))

	res := f.publish(f.stage(nil))
	if !res.First || !res.Committed || len(res.Changes()) != 0 {
		t.Fatalf("first publish = %+v", res)
	}
	if got := f.history(); len(got) != 1 || got[0].Subject != "first record" {
		t.Fatalf("history = %+v", got)
	}

	// Same items, other files changed: no commit.
	res = f.publish(f.stage(nil))
	if res.First || res.Committed {
		t.Fatalf("unchanged publish = %+v", res)
	}
	if got := f.history(); len(got) != 1 {
		t.Fatalf("an unchanged publish made a commit: %+v", got)
	}

	// One item more: exactly one commit, with the change in its subject and body.
	res = f.publish(f.stage(addVim))
	if !res.Committed || !reflect.DeepEqual(res.Changes(), []string{"+ package    vim  [extra]"}) {
		t.Fatalf("changed publish = %+v", res)
	}
	got := f.history()
	if len(got) != 2 {
		t.Fatalf("want 2 commits, got %+v", got)
	}
	want := Commit{Hash: got[0].Hash, Date: f.now, Subject: "1 change(s): + package vim [extra]",
		Changes: []string{"+ package    vim  [extra]"}}
	if !reflect.DeepEqual(got[0], want) || !got[0].Date.Equal(f.now) {
		t.Errorf("newest commit = %+v\nwant %+v", got[0], want)
	}

	// A bundle move isn't a change.
	res = f.publish(f.stage(func(r *Record) {
		addVim(r)
		r.Items[len(r.Items)-1].Bundle = "app-vim"
	}))
	if res.Committed {
		t.Errorf("a bundle move made a commit")
	}

	if got := f.history(); len(got) != 2 {
		t.Errorf("history = %+v", got)
	}
}

func TestPublishWritesTheRecordStoreFiles(t *testing.T) {
	f := newFixture(t, nil)
	f.publish(f.stage(nil))
	r, err := Load(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := os.ReadFile(filepath.Join(f.dir, SummaryFile))
	if string(summary) != r.Summary() {
		t.Errorf("SUMMARY.md doesn't match the record")
	}
	if got := read(t, filepath.Join(f.dir, GitignoreFile)); got != "last-capture\nnot-backed-up.md\n" {
		t.Errorf(".gitignore = %q", got)
	}
	if got := read(t, filepath.Join(f.dir, LastCaptureFile)); got != "2026-10-01T10:00:00+02:00\n" {
		t.Errorf("last-capture = %q", got)
	}
	if got := read(t, filepath.Join(f.dir, "files", "etc", "locale.conf")); got != "LANG=en_US.UTF-8\n" {
		t.Errorf("files/ wasn't kept: %q", got)
	}
	info, err := os.Stat(f.dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("record folder mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	if exists(filepath.Join(f.dir, ".git")) {
		t.Errorf("a history was made without git")
	}
	f.assertNoLeftovers()
}

func TestPublishKeepsHistoryAndTheNotBackedUpList(t *testing.T) {
	f := newFixture(t, realGit(t))
	stage := f.stage(nil)
	write(t, filepath.Join(stage, NotBackedUpFile), "# Not in the backup\n")
	f.publish(stage)

	f.publish(f.stage(addVim)) // a quick capture: no list
	if got := read(t, filepath.Join(f.dir, NotBackedUpFile)); got != "# Not in the backup\n" {
		t.Errorf("not-backed-up.md = %q", got)
	}
	stage = f.stage(addVim)
	write(t, filepath.Join(stage, NotBackedUpFile), "# Not in the backup, again\n")
	f.publish(stage)
	if got := read(t, filepath.Join(f.dir, NotBackedUpFile)); got != "# Not in the backup, again\n" {
		t.Errorf("not-backed-up.md = %q", got)
	}
	if got := f.history(); len(got) != 2 {
		t.Errorf("history = %+v", got)
	}
	f.assertNoLeftovers()
}

func TestPublishWithoutGit(t *testing.T) {
	fake := &sysexec.Fake{} // knows no programs: git is missing
	f := newFixture(t, fake)
	f.publish(f.stage(nil))
	f.publish(f.stage(addVim))
	if calls := fake.Commands(); len(calls) != 0 {
		t.Errorf("ran %q without git", calls)
	}
	if _, err := History(context.Background(), f.dir, fake, 0); !errors.Is(err, ErrNoGit) {
		t.Errorf("History err = %v, want ErrNoGit", err)
	}
}

func TestPublishWithoutCommit(t *testing.T) {
	f := newFixture(t, realGit(t))
	res, err := Publish(context.Background(), f.stage(nil), f.dir, PublishOptions{Git: f.git})
	if err != nil || res.Committed || exists(filepath.Join(f.dir, ".git")) {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestPublishReportsAGitFailureAfterPublishing(t *testing.T) {
	fake := &sysexec.Fake{Programs: []string{"git"}} // every git command fails as unexpected
	f := newFixture(t, fake)
	_, err := Publish(context.Background(), f.stage(nil), f.dir, PublishOptions{Commit: true, Git: fake})
	if !errors.Is(err, ErrHistory) {
		t.Fatalf("err = %v, want ErrHistory", err)
	}
	if _, err := Load(f.dir); err != nil {
		t.Errorf("the record wasn't published: %v", err)
	}
	cmd := fake.Commands()[0]
	for _, want := range []string{"env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_DIR=", "GIT_AUTHOR_NAME=carryon",
		"GIT_COMMITTER_EMAIL=carryon@localhost", "git --no-pager -C " + f.dir, "-c commit.gpgSign=false -c core.hooksPath=/dev/null"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("git command %q lacks %q", cmd, want)
		}
	}
}

func TestPublishRefusesAnIncompleteStage(t *testing.T) {
	f := newFixture(t, nil)
	stage := f.stage(nil)
	if err := os.Remove(filepath.Join(stage, ItemsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), stage, f.dir, PublishOptions{}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v", err)
	}
	if exists(f.dir) {
		t.Errorf("an incomplete record was published")
	}
}

func TestPublishAcrossFilesystems(t *testing.T) {
	old := renameStage
	renameStage = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameStage = old })

	f := newFixture(t, nil)
	stage := f.stage(nil)
	if err := os.Symlink("/opt/hello/bin/hello", filepath.Join(stage, "files", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stage, "versions.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.publish(stage)
	if link, err := os.Readlink(filepath.Join(f.dir, "files", "hello")); err != nil || link != "/opt/hello/bin/hello" {
		t.Errorf("symlink = %q, %v", link, err)
	}
	if info, err := os.Stat(filepath.Join(f.dir, "versions.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("versions.txt lost its mode: %v %v", info.Mode(), err)
	}
	if info, err := os.Stat(f.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("record folder mode = %v, %v", info.Mode(), err)
	}
	f.assertNoLeftovers()
}

// TestInterruptedPublishRecovers sets up what a publish leaves behind when it dies at each step,
// and checks that the next publish (or Recover) gets back to one complete record with its
// history.
func TestInterruptedPublishRecovers(t *testing.T) {
	git := realGit(t)
	cases := []struct {
		name  string
		crash func(f *fixture, next string) // next is a complete staged record with vim
		want  int                           // items in the record after Recover
	}{
		{"while copying across filesystems", func(f *fixture, next string) {
			tmp := f.dir + tmpSuffix + "123"
			mustRename(t, next, tmp)
			must(t, os.Remove(filepath.Join(tmp, ItemsFile)))
		}, 18},
		{"after staging, before the swap", func(f *fixture, next string) {
			mustRename(t, next, f.dir+newSuffix)
		}, 18},
		{"half way through the swap", func(f *fixture, next string) {
			mustRename(t, next, f.dir+newSuffix)
			mustRename(t, f.dir, f.dir+oldSuffix)
		}, 19},
		{"after the swap, before keeping the history", func(f *fixture, next string) {
			mustRename(t, next, f.dir+newSuffix)
			mustRename(t, f.dir, f.dir+oldSuffix)
			mustRename(t, f.dir+newSuffix, f.dir)
		}, 19},
		{"while removing the old record", func(f *fixture, next string) {
			mustRename(t, next, f.dir+newSuffix)
			mustRename(t, f.dir, f.dir+oldSuffix)
			mustRename(t, f.dir+newSuffix, f.dir)
			mustRename(t, filepath.Join(f.dir+oldSuffix, ".git"), filepath.Join(f.dir, ".git"))
			must(t, os.Remove(filepath.Join(f.dir+oldSuffix, MetaFile)))
		}, 19},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, git)
			f.publish(f.stage(nil))
			// a complete next record, as Publish would have prepared it
			next := f.stage(addVim)
			r, _ := Load(next)
			write(t, filepath.Join(next, SummaryFile), r.Summary())
			c.crash(f, next)

			if err := Recover(f.dir); err != nil {
				t.Fatal(err)
			}
			r, err := Load(f.dir)
			if err != nil {
				t.Fatalf("no complete record after recovering: %v", err)
			}
			if len(r.Items) != c.want {
				t.Errorf("got %d items, want %d", len(r.Items), c.want)
			}
			f.assertNoLeftovers()
			if got := f.history(); len(got) != 1 {
				t.Fatalf("history lost: %+v", got)
			}

			// The next publish works and records the change against the recovered record.
			res := f.publish(f.stage(addVim))
			if wantCommit := c.want == 18; res.Committed != wantCommit {
				t.Errorf("committed = %v, want %v", res.Committed, wantCommit)
			}
		})
	}
}

func TestHistoryErrors(t *testing.T) {
	ctx := context.Background()
	git := realGit(t)
	dir := t.TempDir()
	if _, err := History(ctx, filepath.Join(dir, "missing"), git, 0); !errors.Is(err, ErrNoHistory) {
		t.Errorf("missing record: err = %v", err)
	}
	if _, err := History(ctx, dir, git, 0); !errors.Is(err, ErrNoHistory) {
		t.Errorf("no .git: err = %v", err)
	}
	if _, err := History(ctx, dir, nil, 0); !errors.Is(err, ErrNoGit) {
		t.Errorf("no git: err = %v", err)
	}
	if _, err := (gitRepo{run: git, dir: dir}).output(ctx, time.Time{}, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if _, err := History(ctx, dir, git, 0); !errors.Is(err, ErrNoHistory) {
		t.Errorf("no commits: err = %v", err)
	}

	if os.Geteuid() == 0 {
		t.Log("running as root: can't test a record we may not read")
		return
	}
	locked := filepath.Join(t.TempDir(), "record")
	must(t, os.Mkdir(locked, 0o700))
	must(t, os.Mkdir(filepath.Join(locked, ".git"), 0o700))
	must(t, os.Chmod(locked, 0))
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if _, err := History(ctx, locked, git, 0); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("unreadable record: err = %v", err)
	}
}

func TestHistoryLimit(t *testing.T) {
	f := newFixture(t, realGit(t))
	f.publish(f.stage(nil))
	f.publish(f.stage(addVim))
	commits, err := History(context.Background(), f.dir, f.git, 1)
	if err != nil || len(commits) != 1 || !strings.HasPrefix(commits[0].Subject, "1 change(s)") {
		t.Fatalf("commits = %+v, err = %v", commits, err)
	}
}

func TestParseLog(t *testing.T) {
	out := "abc\x1f2026-10-01T10:00:00+02:00\x1f2 change(s): x\x1f+ package    vim\n~ file       /etc/hosts\n\nnot a change\n\x1e\n" +
		"def\x1f2026-09-30T08:00:00Z\x1ffirst record\x1f\x1e\n"
	got, err := parseLog(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Hash != "abc" || !reflect.DeepEqual(got[0].Changes, []string{"+ package    vim", "~ file       /etc/hosts"}) ||
		got[1].Subject != "first record" || got[1].Changes != nil {
		t.Errorf("parseLog = %+v", got)
	}
	if _, err := parseLog("garbage\x1e"); err == nil {
		t.Errorf("want an error for garbage")
	}
}

func (f *fixture) assertNoLeftovers() {
	f.t.Helper()
	entries, err := os.ReadDir(f.parent)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); n != "record" && n != "record"+lockSuffix && !strings.HasPrefix(n, "stage-") {
			f.t.Errorf("leftover %s next to the record", n)
		}
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(text), 0o644))
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	must(t, os.Rename(from, to))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
