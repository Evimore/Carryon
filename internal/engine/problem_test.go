package engine

import (
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
)

func TestClassify(t *testing.T) {
	local := config.Repository{URL: "/mnt/backup/carryon", PasswordFile: "/etc/carryon/repository-password"}
	cloud := config.Repository{URL: "rclone:my-cloud:backups", PasswordFile: "/etc/carryon/repository-password", RcloneUser: "alice"}
	rest := config.Repository{URL: "rest:https://example.com/carryon/", PasswordCommand: "pass show carryon"}

	tests := []struct {
		name      string
		code      int
		stderr    string
		repo      config.Repository
		wantCause string
		wantFix   []string // all must appear in the fix
	}{
		{
			name:      "wrong password, JSON exit message",
			code:      ExitBadPassword,
			stderr:    `{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}`,
			repo:      local,
			wantCause: "wrong password or no key found",
			wantFix:   []string{"password doesn't match", "/etc/carryon/repository-password"},
		},
		{
			name:      "wrong password, restic 0.17.0 exits 1",
			code:      ExitFatal,
			stderr:    "Fatal: wrong password or no key found\n",
			repo:      rest,
			wantCause: "wrong password or no key found",
			wantFix:   []string{"password command"},
		},
		{
			name: "locked",
			code: ExitLockFailed,
			stderr: "repo already locked, waiting up to 30m0s for the lock\n" +
				"unable to create lock in backend: repository is already locked by PID 4242 on desktop by root (UID 0, GID 0)\n" +
				"lock was created at 2026-10-15 03:00:01 (12m3s ago)\n" +
				"storage ID 1a2b3c4d\n" +
				"the `unlock` command can be used to remove stale locks\n",
			repo:      local,
			wantCause: "unable to create lock in backend: repository is already locked by PID 4242 on desktop by root (UID 0, GID 0)",
			wantFix:   []string{"still using the repository", "sudo carryon restic unlock"},
		},
		{
			name: "rclone login expired names the real remote and user",
			code: ExitFatal,
			stderr: `rclone: 2026/10/15 03:00:02 Failed to create file system for "my-cloud:backups": couldn't fetch token: invalid_grant: maybe token expired? - try refreshing with "rclone config reconnect my-cloud:"` + "\n" +
				"Fatal: unable to open repository at rclone:my-cloud:backups: error talking HTTP to rclone: exit status 1\n",
			repo:      cloud,
			wantCause: "unable to open repository at rclone:my-cloud:backups: error talking HTTP to rclone: exit status 1",
			wantFix:   []string{"as alice", "rclone reconnect my-cloud:"},
		},
		{
			name:      "rclone unauthorized",
			code:      ExitFatal,
			stderr:    "rclone: 2026/10/15 03:00:02 ERROR : : error listing: HTTP error 401 (401 Unauthorized)\nFatal: unable to open repository at rclone:my-cloud:backups: error talking HTTP to rclone: exit status 1\n",
			repo:      cloud,
			wantCause: "unable to open repository at rclone:my-cloud:backups: error talking HTTP to rclone: exit status 1",
			wantFix:   []string{"rclone reconnect my-cloud:"},
		},
		{
			name:      "rclone not installed",
			code:      ExitFatal,
			stderr:    `Fatal: unable to open repository at rclone:my-cloud:backups: exec: "rclone": executable file not found in $PATH` + "\n",
			repo:      config.Repository{URL: "rclone:my-cloud:backups"},
			wantCause: `unable to open repository at rclone:my-cloud:backups: exec: "rclone": executable file not found in $PATH`,
			wantFix:   []string{"rclone isn't installed"},
		},
		{
			name:      "repository missing",
			code:      ExitNoRepo,
			stderr:    `{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file: stat /mnt/backup/carryon/config: no such file or directory\nIs there a repository at the following location?\n/mnt/backup/carryon"}` + "\n",
			repo:      local,
			wantCause: "repository does not exist: unable to open config file: stat /mnt/backup/carryon/config: no such file or directory",
			wantFix:   []string{"no backup repository at /mnt/backup/carryon", "plug it in"},
		},
		{
			name:      "disk full",
			code:      ExitFatal,
			stderr:    "Fatal: unable to save snapshot: Save(<data/1a2b3c4d>) returned error, retrying after 1s: write /mnt/backup/carryon/data/1a/1a2b: no space left on device\n",
			repo:      local,
			wantCause: "unable to save snapshot: Save(<data/1a2b3c4d>) returned error, retrying after 1s: write /mnt/backup/carryon/data/1a/1a2b: no space left on device",
			wantFix:   []string{"a disk is full"},
		},
		{
			name:      "network unreachable",
			code:      ExitFatal,
			stderr:    `Fatal: unable to open repository at rest:https://example.com/carryon/: Get "https://example.com/carryon/config": dial tcp: lookup example.com: no such host` + "\n",
			repo:      rest,
			wantCause: `unable to open repository at rest:https://example.com/carryon/: Get "https://example.com/carryon/config": dial tcp: lookup example.com: no such host`,
			wantFix:   []string{"can't be reached", "network"},
		},
		{
			name:      "interrupted",
			code:      ExitInterrupted,
			stderr:    "signal interrupt received, cleaning up\n",
			repo:      local,
			wantCause: "signal interrupt received, cleaning up",
			wantFix:   []string{"next run"},
		},
		{
			name:      "some files unreadable",
			code:      ExitPartial,
			stderr:    "error: open /home/alice/.cache/locked: permission denied\nWarning: at least one source file could not be read\n",
			repo:      local,
			wantCause: "Warning: at least one source file could not be read",
			wantFix:   []string{"saved without the files"},
		},
		{
			name:      "nothing said",
			code:      ExitFatal,
			repo:      local,
			wantCause: "restic failed without saying why (exit code 1)",
			wantFix:   []string{"full output"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Classify(tt.code, tt.stderr, tt.repo)
			if p.Cause != tt.wantCause {
				t.Errorf("cause = %q\nwant    %q", p.Cause, tt.wantCause)
			}
			for _, w := range tt.wantFix {
				if !strings.Contains(p.Fix, w) {
					t.Errorf("fix %q lacks %q", p.Fix, w)
				}
			}
		})
	}
}

// The prototype named iCloud in every message; Carryon must not assume a provider.
func TestClassifyNamesNoProvider(t *testing.T) {
	for _, code := range []int{1, 3, 10, 11, 12, 130} {
		for _, stderr := range []string{"", "rclone: 401 Unauthorized", "no space left on device", "dial tcp: i/o timeout"} {
			p := Classify(code, stderr, config.Repository{URL: "rclone:my-cloud:backups"})
			if s := strings.ToLower(p.String()); strings.Contains(s, "icloud") {
				t.Errorf("Classify(%d, %q) mentions iCloud: %s", code, stderr, p)
			}
		}
	}
}

func TestRemoteName(t *testing.T) {
	for url, want := range map[string]string{
		"rclone:my-cloud:backups":    "my-cloud",
		"rclone:my-cloud:":           "my-cloud",
		"rclone:b2-work:my-bucket/x": "b2-work",
		"rclone::local:/srv":         "REMOTE",
	} {
		if got := remoteName(url); got != want {
			t.Errorf("remoteName(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestIsNoRepoAndIsBadPassword(t *testing.T) {
	for _, c := range []struct {
		code             int
		stderr           string
		noRepo, password bool
	}{
		{ExitNoRepo, "Fatal: repository does not exist: unable to open config file", true, false},
		{ExitFatal, "Fatal: unable to open config file: stat /mnt/backup/config: no such file or directory\nIs there a repository at the following location?", true, false},
		{ExitBadPassword, "Fatal: wrong password or no key found", false, true},
		{ExitFatal, "Fatal: wrong password or no key found", false, true}, // restic 0.17.0
		{ExitFatal, "Fatal: unable to open repository: dial tcp: no such host", false, false},
		{ExitOK, "", false, false},
	} {
		if got := IsNoRepo(c.code, c.stderr); got != c.noRepo {
			t.Errorf("IsNoRepo(%d, %q) = %v", c.code, c.stderr, got)
		}
		if got := IsBadPassword(c.code, c.stderr); got != c.password {
			t.Errorf("IsBadPassword(%d, %q) = %v", c.code, c.stderr, got)
		}
	}
}

func TestIsRefused(t *testing.T) {
	local := config.Repository{URL: "/mnt/backup/carryon"}
	cloud := config.Repository{URL: "rclone:my-cloud:backups"}
	for _, c := range []struct {
		name   string
		code   int
		stderr string
		repo   config.Repository
		want   bool
	}{
		{"wrong password", ExitBadPassword, "Fatal: wrong password or no key found", local, true},
		{"wrong password, restic 0.17.0", ExitFatal, "Fatal: wrong password or no key found", local, true},
		{"no repository", ExitNoRepo, "Fatal: repository does not exist: unable to open config file", local, true},
		{"offline", ExitFatal, "Fatal: unable to open config file: Stat: dial tcp: lookup example.com: no such host\nIs there a repository at the following location?", local, false},
		{"another error opening it", ExitFatal, "Fatal: unable to open config file: Stat: 503 Service Unavailable\nIs there a repository at the following location?", local, false},
		{"rclone login expired", ExitFatal, "rclone: Failed to create file system: couldn't fetch token: invalid_grant\nFatal: unable to open repository", cloud, true},
		{"rclone missing", ExitFatal, `Fatal: unable to open repository: exec: "rclone": executable file not found in $PATH`, cloud, true},
		{"rclone offline", ExitFatal, "rclone: dial tcp: lookup www.example.com: no such host\nFatal: unable to open repository", cloud, false},
		{"locked", ExitLockFailed, "Fatal: unable to create lock in backend: repository is already locked", local, false},
		{"ok", ExitOK, "", local, false},
	} {
		if got := IsRefused(c.code, c.stderr, c.repo); got != c.want {
			t.Errorf("%s: IsRefused = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsExpiredLogin(t *testing.T) {
	for stderr, want := range map[string]bool{
		"ERROR : couldn't list: token expired":                                          true,
		"Failed to create file system for \"my-cloud:\": oauth2: invalid_grant":         true,
		"couldn't list directory: Get \"https://example.com/\": dial tcp: no such host": false,
		"directory not found": false,
	} {
		if got := IsExpiredLogin(stderr); got != want {
			t.Errorf("IsExpiredLogin(%q) = %v, want %v", stderr, got, want)
		}
	}
}
