package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/sysexec"
)

const inv = "0123456789abcdef0123456789abcdef"

func journalFake(errLines, allLines string) *sysexec.Fake {
	return (&sysexec.Fake{}).
		On("systemctl show -p InvocationID --value carryon-backup.service", sysexec.Response{Stdout: inv + "\n"}).
		On("journalctl _SYSTEMD_INVOCATION_ID="+inv+" -p err -o cat --no-pager", sysexec.Response{Stdout: errLines}).
		On("journalctl _SYSTEMD_INVOCATION_ID="+inv+" -o cat --no-pager", sysexec.Response{Stdout: allLines})
}

func TestJournalLastError(t *testing.T) {
	tests := []struct {
		name         string
		errs, all    string
		want         string
		wantFallback bool
	}{
		{
			name: "error priority, last three lines, marker removed",
			errs: "xx first\nxx second\n\nxx third\nxx fourth\n",
			want: "second\nthird\nfourth",
		},
		{
			name:         "older runs logged without priorities",
			all:          ":: starting\nFatal: unable to open config file\nall fine here\nxx  rclone: error talking to the remote\n",
			want:         "Fatal: unable to open config file\nrclone: error talking to the remote",
			wantFallback: true,
		},
		{name: "nothing logged", wantFallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := journalFake(tt.errs, tt.all)
			got, err := Journal{Runner: f}.LastError(context.Background(), "carryon-backup.service")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if n := len(f.Commands()); (n == 3) != tt.wantFallback {
				t.Errorf("ran %q", f.Commands())
			}
		})
	}
}

func TestJournalNeverRan(t *testing.T) {
	f := (&sysexec.Fake{}).On("systemctl show -p InvocationID --value carryon-verify.service", sysexec.Response{Stdout: "\n"})
	got, err := Journal{Runner: f}.LastError(context.Background(), "carryon-verify.service")
	if got != "" || err != nil {
		t.Errorf("got %q, %v", got, err)
	}
}

// fixedErrors is an ErrorSource for tests.
type fixedErrors struct {
	text string
	err  error
}

func (f fixedErrors) LastError(context.Context, string) (string, error) { return f.text, f.err }

func TestFailure(t *testing.T) {
	local := config.Repository{URL: "/mnt/backup/carryon"}
	long := strings.Repeat("é", 200) // 400 bytes
	tests := []struct {
		name, unit string
		src        ErrorSource
		wantTitle  string
		wantBody   string
		wantWarn   bool
	}{
		{"backup", "carryon-backup.service", fixedErrors{text: "Fatal: disk full"}, "Backup failed", "Fatal: disk full\nDetails: carryon status", false},
		{"verify", "carryon-verify.service", fixedErrors{text: "pack 1a2b is damaged"}, "Backup check found problems", "pack 1a2b is damaged\nDetails: carryon status", false},
		{"no error lines", "carryon-backup.service", fixedErrors{}, "Backup failed", "Details: carryon status", false},
		{"error source fails", "carryon-backup.service", fixedErrors{err: errors.New("journal unreadable")}, "Backup failed", "Details: carryon status", true},
		{"long text is cut", "carryon-backup.service", fixedErrors{text: long}, "Backup failed", strings.Repeat("é", 150) + "...\nDetails: carryon status", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warned := false
			m := Failure(context.Background(), tt.unit, "desktop", local, tt.src, &RemoteCheck{}, func(string, ...any) { warned = true })
			if m.Title != tt.wantTitle || m.Body != tt.wantBody || m.Urgency != Critical || m.Unit != tt.unit || m.Host != "desktop" {
				t.Errorf("got %+v\nwant title %q, body %q", m, tt.wantTitle, tt.wantBody)
			}
			if warned != tt.wantWarn {
				t.Errorf("warned = %v", warned)
			}
		})
	}
}

func TestFailureRcloneRemote(t *testing.T) {
	lsf := asUser("alice", 1000, "rclone", "lsf", "--max-depth", "1", "my-cloud:")
	tests := []struct {
		name      string
		repo      config.Repository
		resp      sysexec.Response
		wantTitle string
		wantBody  []string
	}{
		{
			name:      "remote answers: the run's own errors",
			repo:      config.Repository{URL: "rclone:my-cloud:backups", RcloneUser: "alice"},
			wantTitle: "Backup failed",
			wantBody:  []string{"Fatal: something else\nDetails: carryon status"},
		},
		{
			name:      "login expired",
			repo:      config.Repository{URL: "rclone:my-cloud:backups", RcloneUser: "alice"},
			resp:      sysexec.Response{Code: 1, Stderr: "Failed to lsf: couldn't fetch token: invalid_grant: token expired"},
			wantTitle: "Backups can't reach my-cloud:",
			wantBody: []string{
				"The rclone remote my-cloud: didn't answer: Failed to lsf: couldn't fetch token: invalid_grant: token expired",
				"as alice (not root), run: rclone reconnect my-cloud:",
				"Details: carryon status",
			},
		},
		{
			name:      "offline",
			repo:      config.Repository{URL: "rclone:my-cloud:backups", RcloneUser: "alice"},
			resp:      sysexec.Response{Code: 1, Stderr: "dial tcp: lookup example.com: no such host"},
			wantTitle: "Backups can't reach my-cloud:",
			wantBody:  []string{"Check you're online. If the login expired, run (as alice, not root): rclone reconnect my-cloud:"},
		},
		{
			name:      "rclone missing",
			repo:      config.Repository{URL: "rclone:my-cloud:backups", RcloneUser: "alice"},
			resp:      sysexec.Response{Missing: true},
			wantTitle: "Backups can't reach my-cloud:",
			wantBody:  []string{"rclone isn't installed", "sudo pacman -S rclone"},
		},
		{
			name:      "rclone_user unknown",
			repo:      config.Repository{URL: "rclone:my-cloud:backups", RcloneUser: "nobody-here"},
			wantTitle: "Backups can't reach my-cloud:",
			wantBody:  []string{`repository.rclone_user "nobody-here" isn't a user`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := (&sysexec.Fake{Root: true, Path: "/usr/bin"}).On(lsf, tt.resp)
			rc := &RemoteCheck{Runner: f, Euid: 0, Path: "/usr/bin", LookupUser: func(n string) (sysexec.User, error) {
				u, err := homes(n)
				u.UID = 1000
				return u, err
			}}
			m := Failure(context.Background(), "carryon-backup.service", "desktop", tt.repo, fixedErrors{text: "Fatal: something else"}, rc, nil)
			if m.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", m.Title, tt.wantTitle)
			}
			for _, w := range tt.wantBody {
				if !strings.Contains(m.Body, w) {
					t.Errorf("body = %q\nwant it to contain %q", m.Body, w)
				}
			}
		})
	}
}

func TestRemoteCheckWithoutRcloneUser(t *testing.T) {
	f := (&sysexec.Fake{Root: true}).On("rclone lsf --max-depth 1 :s3,provider=AWS:", sysexec.Response{})
	if p := (RemoteCheck{Runner: f}).Check(context.Background(), config.Repository{URL: "rclone::s3,provider=AWS:bucket"}); p != nil {
		t.Errorf("problem = %+v", p)
	}
	if got := f.Commands(); len(got) != 1 {
		t.Errorf("ran %q", got)
	}
	// not rclone: nothing runs
	if p := (RemoteCheck{Runner: f}).Check(context.Background(), config.Repository{URL: "sftp:alice@example.com:/srv"}); p != nil || len(f.Commands()) != 1 {
		t.Errorf("checked a non-rclone repository: %+v", p)
	}
}

func TestChannels(t *testing.T) {
	cfg := config.Default()
	if got := names(Channels(cfg, System{})); got != "desktop" {
		t.Errorf("default = %q", got)
	}
	cfg.Notify = config.Notify{Desktop: false, NtfyURL: "https://ntfy.sh/my-topic", WebhookURL: "https://example.com/hook"}
	if got := names(Channels(cfg, System{})); got != "ntfy (ntfy.sh), webhook (example.com)" {
		t.Errorf("got %q", got)
	}
	cfg.Notify = config.Notify{}
	if got := Channels(cfg, System{}); len(got) != 0 {
		t.Errorf("all off: %q", names(got))
	}
}

func names(senders []Sender) string {
	var n []string
	for _, s := range senders {
		n = append(n, s.Name())
	}
	return strings.Join(n, ", ")
}

// stub is a Sender for SendAll tests.
type stub struct {
	name string
	err  error
}

func (s stub) Name() string                                  { return s.name }
func (s stub) Send(context.Context, Message) (string, error) { return s.name + " user", s.err }

func TestSendAll(t *testing.T) {
	results := SendAll(context.Background(), Message{}, []Sender{stub{name: "a", err: errors.New("down")}, stub{name: "b"}})
	if len(results) != 2 || results[0].Err == nil || results[1].To != "b user" || !Delivered(results) {
		t.Errorf("results = %+v", results)
	}
	if Delivered(results[:1]) || Delivered(nil) {
		t.Error("Delivered with no success")
	}
}
