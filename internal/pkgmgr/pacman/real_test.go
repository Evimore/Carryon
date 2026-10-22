package pacman

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// execRunner runs real programs, for the read-only tests below.
type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func (execRunner) Run(context.Context, string, ...string) error {
	panic("the real-pacman tests must not change the system")
}

func (execRunner) Exists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// realPacman returns a backend on the real system, or skips the test where there's no pacman (the
// Ubuntu CI job, for example). The CI job "Test (Arch Linux)" runs these. They only read.
func realPacman(t *testing.T) *Pacman {
	t.Helper()
	p := New(execRunner{}, Options{TempDir: t.TempDir()})
	if !p.Present() {
		t.Skip("pacman isn't installed")
	}
	return p
}

func TestRealQueries(t *testing.T) {
	p := realPacman(t)
	installed, err := p.Installed(ctx)
	if err != nil || !slices.Contains(installed, "pacman") {
		t.Fatalf("Installed = %d packages, %v; want pacman among them", len(installed), err)
	}
	if _, err := p.Packages(ctx); err != nil {
		t.Errorf("Packages: %v", err)
	}
	if owned, err := p.OwnedFiles(ctx); err != nil || !slices.Contains(owned, "/usr/bin/pacman") {
		t.Errorf("OwnedFiles: %d paths, %v; want /usr/bin/pacman among them", len(owned), err)
	}
	files, err := p.FilesOf(ctx, "pacman")
	if err != nil || !slices.Contains(files["pacman"], "/usr/bin/pacman") {
		t.Errorf("FilesOf(pacman) = %d paths, %v", len(files["pacman"]), err)
	}
	if repos, err := p.ConfiguredRepos(ctx); err != nil || !slices.Contains(repos, "core") {
		t.Errorf("ConfiguredRepos = %v, %v", repos, err)
	}
	if _, err := p.ExtraRepos(ctx); err != nil {
		t.Errorf("ExtraRepos: %v", err)
	}
}

func TestRealOwners(t *testing.T) {
	p := realPacman(t)
	paths := []string{"/usr/bin/pacman", "/etc/pacman.conf", "/usr/bin", "/etc/carryon-test-not-owned"}
	if _, err := os.Lstat("/bin"); err == nil {
		paths = append(paths, "/bin/pacman") // /bin is a link to usr/bin on Arch
	}
	got, err := p.Owners(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/usr/bin/pacman", "/etc/pacman.conf"} {
		if got[path] != "pacman" {
			t.Errorf("owner of %s = %q, want pacman", path, got[path])
		}
	}
	if len(paths) == 5 && got["/bin/pacman"] != "pacman" {
		t.Errorf("owner of /bin/pacman = %q, want pacman", got["/bin/pacman"])
	}
	if got["/usr/bin"] == "" {
		t.Error("/usr/bin has no owner")
	}
	if o, ok := got["/etc/carryon-test-not-owned"]; ok {
		t.Errorf("a missing file has owner %q", o)
	}
}

// TestRealOutputFormats checks the formats the parsers expect against the installed pacman.
func TestRealOutputFormats(t *testing.T) {
	p := realPacman(t)
	out, err := p.pacmanC(ctx, "-Qii", "pacman")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range parseBackupFiles(out) {
		found = found || b.path == "/etc/pacman.conf"
	}
	if !found {
		t.Errorf("no status for /etc/pacman.conf in pacman -Qii pacman:\n%s", out)
	}
	if _, err := p.ModifiedConfigs(ctx); err != nil {
		t.Errorf("ModifiedConfigs: %v", err)
	}

	// the full check takes minutes; one package shows the format
	out, err = execRunner{}.Output(ctx, "env", "LC_ALL=C", "sh", "-c", "exec pacman -Qkk pacman 2>&1")
	if err != nil && exitCode(err) <= 0 {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "pacman: ") || !strings.Contains(string(out), " total file") {
		t.Errorf("unexpected pacman -Qkk output:\n%s", out)
	}
	for _, e := range parseCheck(out) {
		if e.Package != "pacman" || e.Path == "/etc/pacman.conf" || e.Path == "/etc/makepkg.conf" {
			t.Errorf("not an edited file: %+v", e)
		}
	}
}
