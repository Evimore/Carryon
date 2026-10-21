package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Evimore/carryon/internal/sysexec"
)

// fakeProc makes a /proc with one process per entry: its exe link, comm and command line.
func fakeProc(t *testing.T, procs ...[3]string) string {
	t.Helper()
	root := t.TempDir()
	for i, p := range procs {
		dir := filepath.Join(root, string(rune('1'+i)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if p[0] != "" {
			if err := os.Symlink(p[0], filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(p[1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(p[2]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a process.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAppsRunning(t *testing.T) {
	pacman := (&sysexec.Fake{}).
		On("pacman -Qlq -- obs-studio", sysexec.Response{Stdout: "/usr/\n/usr/bin/\n/usr/bin/obs\n/usr/share/obs/libobs/data.txt\n"}).
		On("pacman -Qlq -- jellyfin-server", sysexec.Response{Code: 1, Stderr: "error: package 'jellyfin-server' was not found"}).
		On("pacman -Qlq -- quodlibet", sysexec.Response{Stdout: "/usr/bin/quodlibet\n"}).
		On("pacman -Qlq -- telegram-desktop", sysexec.Response{Stdout: "/usr/bin/telegram-desktop\n"})
	tests := []struct {
		name  string
		procs [][3]string
		app   string
		want  bool
	}{
		{"its program runs", [][3]string{{"/usr/bin/bash", "bash", "bash\x00"}, {"/usr/bin/obs", "obs", "obs\x00"}}, "obs-studio", true},
		{"upgraded while running", [][3]string{{"/usr/bin/obs (deleted)", "obs", "obs\x00"}}, "obs-studio", true},
		{"not running", [][3]string{{"/usr/bin/bash", "bash", "bash\x00"}}, "obs-studio", false},
		{"named like the package", [][3]string{{"/usr/lib/jellyfin/jellyfin-server", "jellyfin-server", ""}}, "jellyfin-server", true},
		{"a script", [][3]string{{"/usr/bin/python3.13", "python3", "/usr/bin/python3\x00/usr/bin/quodlibet\x00--run\x00"}}, "quodlibet", true},
		{"a long name the kernel cut", [][3]string{{"", "telegram-deskto", ""}}, "telegram-desktop", true},
		{"no app", [][3]string{{"/usr/bin/obs", "obs", ""}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Apps{Runner: pacman, Proc: fakeProc(t, tt.procs...), UID: os.Getuid()}
			if got := a.Running(context.Background(), tt.app); got != tt.want {
				t.Errorf("Running(%q) = %v, want %v", tt.app, got, tt.want)
			}
		})
	}
	t.Run("someone else's process", func(t *testing.T) {
		a := Apps{Runner: pacman, Proc: fakeProc(t, [3]string{"/usr/bin/obs", "obs", "obs\x00"}), UID: os.Getuid() + 1}
		if a.Running(context.Background(), "obs-studio") {
			t.Error("another user's process counted")
		}
	})
}
