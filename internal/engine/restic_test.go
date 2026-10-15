package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/paths"
)

// shimTestEnv makes the test binary act as `carryon rclone-shim` when restic starts it.
const shimTestEnv = "CARRYON_ENGINE_TEST_SHIM"

var realExec = execFn

func TestMain(m *testing.M) {
	if os.Getenv(shimTestEnv) == "1" && len(os.Args) > 1 && os.Args[1] == "rclone-shim" {
		err := RunShim(os.Args[2:])
		fmt.Fprintln(os.Stderr, "rclone-shim:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func needRestic(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic isn't installed; the engine's restic tests need it")
	}
}

// testRepo is a fresh local repository in a temp folder.
type testRepo struct {
	e   *Engine
	dir string // a folder to back up
}

func newTestRepo(t *testing.T) testRepo {
	t.Helper()
	needRestic(t)
	tmp := t.TempDir()
	pw := filepath.Join(tmp, "password")
	if err := os.WriteFile(pw, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{URL: filepath.Join(tmp, "repo"), PasswordFile: pw}
	e := New(repo, paths.Dirs{Conf: filepath.Join(tmp, "conf"), Cache: filepath.Join(tmp, "cache")}, "desktop")
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home")}
	e.Stderr = &lockedBuffer{}

	src := filepath.Join(tmp, "src")
	for name, body := range map[string]string{"etc/hostname": "desktop\n", "etc/carryon/config.toml": "machine_name = \"desktop\"\n"} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := e.Run(context.Background(), "init")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("restic init: %v\n%s", err, r.Stderr)
	}
	return testRepo{e: e, dir: src}
}

func TestResticVersion(t *testing.T) {
	needRestic(t)
	e := New(config.Repository{URL: t.TempDir()}, paths.Dirs{Cache: t.TempDir()}, "desktop")
	v, err := e.CheckVersion(context.Background())
	if err != nil {
		t.Fatalf("CheckVersion: %v (restic %s)", err, v)
	}
	t.Logf("restic %s", v)
}

func TestResticBackupSnapshotsLs(t *testing.T) {
	tr := newTestRepo(t)
	e, ctx := tr.e, context.Background()

	r, err := e.Live(ctx, nil, "backup", "--json", "--host", e.Machine(), "--tag", "carryon", tr.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if shown := e.Stderr.(*lockedBuffer).String(); !strings.HasPrefix(shown, "$ restic backup --json") {
		t.Errorf("live output starts %q", shown)
	}
	out, err := ParseBackup(strings.NewReader(r.Stdout))
	if err != nil || out.Summary == nil {
		t.Fatalf("summary: %+v, %v\n%s", out, err, r.Stdout)
	}
	if out.Summary.FilesNew != 2 || out.Summary.SnapshotID == "" {
		t.Errorf("summary = %+v", out.Summary)
	}

	// a second machine in the same repository
	if r, _ := e.Run(ctx, "backup", "--host", "laptop", tr.dir); r.Code != 0 {
		t.Fatalf("second backup: %v", r.Err())
	}

	all, err := e.Snapshots(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	mine, err := e.Snapshots(ctx, e.Machine())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(mine) != 1 {
		t.Fatalf("%d snapshots in all, %d for desktop", len(all), len(mine))
	}
	s := mine[0]
	if s.ID != out.Summary.SnapshotID || s.ShortID != s.ID[:8] || s.Hostname != "desktop" ||
		len(s.Tags) != 1 || s.Tags[0] != "carryon" || len(s.Paths) != 1 || s.Paths[0] != tr.dir || time.Since(s.Time) > time.Hour {
		t.Errorf("snapshot = %+v", s)
	}

	nodes, err := e.Ls(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Node{}
	for _, n := range nodes {
		byPath[n.Path] = n
	}
	f, ok := byPath[filepath.Join(tr.dir, "etc/hostname")]
	if !ok || f.Type != "file" || f.Size != uint64(len("desktop\n")) || f.Name != "hostname" {
		t.Errorf("hostname node = %+v (found %v) in %d nodes", f, ok, len(nodes))
	}
	if d := byPath[filepath.Join(tr.dir, "etc")]; d.Type != "dir" {
		t.Errorf("etc node = %+v", d)
	}

	id, err := e.RepositoryID(ctx)
	if err != nil || len(id) != 64 {
		t.Errorf("RepositoryID = %q, %v", id, err)
	}
}

func TestResticWrongPassword(t *testing.T) {
	tr := newTestRepo(t)
	wrong := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong, []byte("not it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := *tr.e
	e.repo.PasswordFile = wrong
	_, err := e.Snapshots(context.Background(), "")
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	if re.Code != ExitBadPassword || !strings.Contains(re.Cause, "wrong password") || !strings.Contains(re.Fix, wrong) {
		t.Errorf("code %d, problem %+v", re.Code, re.Problem)
	}
}

func TestResticMissingRepository(t *testing.T) {
	tr := newTestRepo(t)
	e := *tr.e
	e.repo.URL = filepath.Join(t.TempDir(), "unplugged-drive")
	_, err := e.RepositoryID(context.Background())
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	if re.Code != ExitNoRepo || !strings.Contains(re.Cause, "repository does not exist") || !strings.Contains(re.Fix, "plug it in") {
		t.Errorf("code %d, problem %+v", re.Code, re.Problem)
	}
}

func TestResticPasswordCommand(t *testing.T) {
	tr := newTestRepo(t)
	e := *tr.e
	e.repo.PasswordCommand = "cat " + e.repo.PasswordFile
	e.repo.PasswordFile = ""
	if _, err := e.Snapshots(context.Background(), ""); err != nil {
		t.Fatalf("with a password command: %v", err)
	}
}

// restic must not need HOME from its parent: systemd services start without one.
func TestResticWithoutHome(t *testing.T) {
	tr := newTestRepo(t)
	e := *tr.e
	e.BaseEnv = []string{"PATH=" + os.Getenv("PATH")}
	if _, err := e.Snapshots(context.Background(), ""); err != nil {
		t.Fatalf("without HOME: %v", err)
	}
}

func TestResticInterrupted(t *testing.T) {
	tr := newTestRepo(t)
	big := filepath.Join(tr.dir, "big")
	if err := os.WriteFile(big, make([]byte, 64<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := *tr.e
	e.Runner = func(ctx context.Context, c Cmd) (int, error) {
		go func() { time.Sleep(300 * time.Millisecond); cancel() }()
		return ExecRunner(ctx, c)
	}
	r, err := e.Run(ctx, "backup", "--read-concurrency", "1", tr.dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Code == 0 {
		t.Skip("the backup finished before it could be interrupted")
	}
	if r.Code != ExitInterrupted && r.Code != 128+int(syscall.SIGINT) {
		t.Errorf("code = %d, want %d\n%s", r.Code, ExitInterrupted, r.Stderr)
	}
	// restic removed its lock, so the next run doesn't wait for it
	if r, _ := tr.e.Run(context.Background(), "list", "locks", "--no-lock"); strings.TrimSpace(r.Stdout) != "" {
		t.Errorf("locks left behind: %q", r.Stdout)
	}
}
