package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/sysexec"
)

// hookServer records the bodies it receives and answers with status.
type hookServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newHookServer(t *testing.T, status int) *hookServer {
	t.Helper()
	h := &hookServer{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, r.Header.Get("Title")+"|"+string(b))
		h.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookServer) received() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

type stubErrors string

func (s stubErrors) LastError(context.Context, string) (string, error) { return string(s), nil }

// fakeNotify replaces the system carryon notify uses: runner f, and errText as the unit's errors.
func fakeNotify(t *testing.T, f *sysexec.Fake, errText string) {
	t.Helper()
	oldSys, oldErr := notifySystem, notifyErrors
	notifySystem = func() notify.System { return notify.System{Runner: f, Euid: 0, Path: "/usr/bin"} }
	notifyErrors = func(paths.Dirs, sysexec.Runner) notify.ErrorSource { return stubErrors(errText) }
	t.Cleanup(func() { notifySystem, notifyErrors = oldSys, oldErr })
}

func notifyConf(t *testing.T, notifySection string) paths.Dirs {
	t.Helper()
	dirs := confDir(t, "machine_name = \"desktop\"\n[repository]\nurl = \"/mnt/backup/carryon\"\n[notify]\n"+notifySection)
	if err := os.WriteFile(dirs.PasswordFile(), []byte("correct horse\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestNotifyUnit(t *testing.T) {
	ntfy := newHookServer(t, http.StatusOK)
	hook := newHookServer(t, http.StatusInternalServerError)
	dirs := notifyConf(t, "desktop = false\nntfy_url = \""+ntfy.URL+"/my-topic\"\nwebhook_url = \""+hook.URL+"/hook\"\n")
	fakeNotify(t, &sysexec.Fake{}, "Fatal: the password correct horse was refused by rest:https://alice:hunter2@example.com/")

	out, err := run(t, dirs, "notify", "carryon-backup.service")
	if err != nil {
		t.Fatalf("one channel delivered, so notify should succeed: %v\n%s", err, out)
	}
	got := ntfy.received()
	if len(got) != 1 || !strings.HasPrefix(got[0], "Backup failed|Fatal: the password *** was refused") || !strings.HasSuffix(got[0], "Details: carryon status") {
		t.Errorf("ntfy got %q", got)
	}
	for _, secret := range []string{"correct horse", "hunter2"} {
		if strings.Contains(strings.Join(append(got, hook.received()...), "\n"), secret) {
			t.Errorf("a secret was sent: %q", secret)
		}
	}
	if !strings.Contains(out, "notified through ntfy (127.0.0.1") || !strings.Contains(out, "xx couldn't notify through webhook (127.0.0.1") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "my-topic") {
		t.Errorf("the log shows the ntfy topic: %q", out)
	}
}

func TestNotifyUnitNothingDelivered(t *testing.T) {
	hook := newHookServer(t, http.StatusForbidden)
	dirs := notifyConf(t, "desktop = true\nwebhook_url = \""+hook.URL+"/hook\"\n")
	f := (&sysexec.Fake{}).On("loginctl list-sessions --no-legend", sysexec.Response{})
	fakeNotify(t, f, "")

	out, err := run(t, dirs, "notify", "carryon-verify.service")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	for _, want := range []string{"xx couldn't notify through desktop: nobody whose home is backed up", "xx couldn't notify through webhook", "403 Forbidden"} {
		if !strings.Contains(out, want) {
			t.Errorf("out = %q\nwant %q", out, want)
		}
	}
	if got := hook.received(); len(got) != 1 || !strings.HasPrefix(got[0], "|") {
		t.Errorf("webhook got %q", got) // JSON body, no Title header
	}
}

func TestNotifyTest(t *testing.T) {
	ntfy := newHookServer(t, http.StatusOK)
	dirs := notifyConf(t, "desktop = false\nntfy_url = \""+ntfy.URL+"/my-topic\"\n")
	fakeNotify(t, &sysexec.Fake{}, "")

	out, err := run(t, dirs, "notify", "--test")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "sent    ntfy (127.0.0.1") {
		t.Errorf("out = %q", out)
	}
	if got := ntfy.received(); len(got) != 1 || !strings.HasPrefix(got[0], "Carryon test notification|") {
		t.Errorf("ntfy got %q", got)
	}

	// any channel that doesn't work fails the test
	bad := newHookServer(t, http.StatusUnauthorized)
	dirs = notifyConf(t, "desktop = false\nntfy_url = \""+ntfy.URL+"/my-topic\"\nwebhook_url = \""+bad.URL+"/hook\"\n")
	out, err = run(t, dirs, "notify", "--test")
	if !errors.As(err, new(exitError)) || !strings.Contains(out, "failed  webhook (127.0.0.1") || !strings.Contains(out, "401 Unauthorized") {
		t.Errorf("err = %v, out = %q", err, out)
	}
}

func TestNotifyArguments(t *testing.T) {
	fakeNotify(t, &sysexec.Fake{}, "")
	dirs := notifyConf(t, "desktop = false\n")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"notify"}, "needs the name of the unit"},
		{[]string{"notify", "--test", "carryon-backup.service"}, "takes no unit"},
		{[]string{"notify", "a.service", "b.service"}, "needs the name of the unit"},
		{[]string{"notify", "carryon-backup.service"}, "no notification channel is set up"},
	}
	for _, tt := range tests {
		_, err := run(t, dirs, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want %q", tt.args, err, tt.want)
		}
	}
}

func TestNotifyWithoutConfig(t *testing.T) {
	// a missing config falls back to the defaults: desktop notifications for everyone
	f := (&sysexec.Fake{}).On("loginctl list-sessions --no-legend", sysexec.Response{})
	fakeNotify(t, f, "")
	out, err := run(t, paths.Dirs{Conf: t.TempDir()}, "notify", "carryon-backup.service")
	if !errors.As(err, new(exitError)) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "notifying with the default settings") || len(f.Commands()) != 1 {
		t.Errorf("out = %q, ran %q", out, f.Commands())
	}
}

func TestNotifyIsHidden(t *testing.T) {
	out, err := run(t, paths.Dirs{}, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "notify") {
		t.Errorf("help lists notify:\n%s", out)
	}
}
