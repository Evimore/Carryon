package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/agent"
	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/notify"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
	"github.com/Evimore/carryon/internal/sysexec"
)

// goneDesktop is a session that ends at once.
type goneDesktop struct{ signals chan agent.Signal }

func (g goneDesktop) Capabilities(context.Context) ([]string, error) { return nil, nil }
func (g goneDesktop) Notify(context.Context, agent.Notification) (uint32, error) {
	return 0, nil
}
func (g goneDesktop) Signals() <-chan agent.Signal { return g.signals }

// fakeAgent replaces the session, the user and the runner carryon agent uses.
func fakeAgent(t *testing.T, home string) *bool {
	t.Helper()
	connected := false
	oldDesk, oldUser, oldRun := agentDesktop, agentUser, agentRunner
	agentDesktop = func() (agent.Desktop, error) {
		connected = true
		ch := make(chan agent.Signal)
		close(ch)
		return goneDesktop{ch}, nil
	}
	agentUser = func() (sysexec.User, error) { return sysexec.User{Name: "alice", UID: 1000, Home: home}, nil }
	agentRunner = func() sysexec.Runner { return &sysexec.Fake{} }
	t.Cleanup(func() { agentDesktop, agentUser, agentRunner = oldDesk, oldUser, oldRun })
	return &connected
}

func TestAgentRefusesRoot(t *testing.T) {
	asUser(t, 0)
	connected := fakeAgent(t, t.TempDir())
	_, err := run(t, paths.Dirs{State: t.TempDir()}, "agent")
	if err == nil || !strings.Contains(err.Error(), "never runs as root") {
		t.Fatalf("err = %v", err)
	}
	if *connected {
		t.Error("it connected to a session as root")
	}
}

func TestAgentRuns(t *testing.T) {
	asUser(t, 1000)
	connected := fakeAgent(t, t.TempDir())
	_, err := run(t, paths.Dirs{State: t.TempDir(), Run: t.TempDir(), Conf: t.TempDir()}, "agent")
	if !*connected {
		t.Error("it didn't connect to the session")
	}
	// The fake session ends at once, which the agent reports, so systemd starts it again.
	if err == nil || !strings.Contains(err.Error(), "lost the connection to the desktop session") {
		t.Fatalf("err = %v", err)
	}
}

// While alice's agent runs, it shows her a failure in the run history; root notifies only bob.
func TestNotifyLeavesItToTheAgent(t *testing.T) {
	dirs := notifyConf(t, "desktop = true\n")
	dirs.State = t.TempDir()
	if err := state.AppendRun(dirs, state.Run{
		Kind: state.KindBackup, Trigger: state.TriggerTimer, Unit: "carryon-backup.service",
		Start: time.Now().Add(-time.Minute), End: time.Now(), Result: state.ResultFailed, Error: "unable to open the repository",
	}); err != nil {
		t.Fatal(err)
	}
	as := func(name string, uid int, argv ...string) string {
		f := &sysexec.Fake{}
		sysexec.AsUser(f, sysexec.User{Name: name, UID: uid, Home: "/home/" + name}, true, "/usr/bin").Output(context.Background(), argv[0], argv[1:]...)
		return f.Commands()[0]
	}
	isActive := []string{"systemctl", "--user", "is-active", "--quiet", notify.AgentUnit}
	f := (&sysexec.Fake{Programs: []string{"notify-send"}}).
		On("loginctl list-sessions --no-legend", sysexec.Response{Stdout: "2 1000 alice seat0 tty2\n3 1001 bob seat0 tty3\n"}).
		On("loginctl show-session -p Id -p Name -p User -p Type -p Class -p State -p Remote 2 3", sysexec.Response{
			Stdout: "Id=2\nName=alice\nUser=1000\nType=wayland\nClass=user\nState=active\nRemote=no\n\n" +
				"Id=3\nName=bob\nUser=1001\nType=x11\nClass=user\nState=active\nRemote=no\n",
		}).
		On("systemctl show -p InvocationID --value carryon-backup.service", sysexec.Response{}).
		On(as("alice", 1000, isActive...), sysexec.Response{}).
		On(as("bob", 1001, isActive...), sysexec.Response{Code: 3})
	old := notifySystem
	notifySystem = func() notify.System {
		return notify.System{
			Runner: f, Euid: 0, Path: "/usr/bin",
			LookupUser: func(name string) (sysexec.User, error) {
				return sysexec.User{Name: name, Home: "/home/" + name}, nil
			},
			Settings: func(sysexec.User) (config.UserNotify, error) { return config.UserNotify{Desktop: true}, nil },
		}
	}
	t.Cleanup(func() { notifySystem = old })
	f.On(as("bob", 1001, "notify-send", "-u", "critical", "-a", "Carryon", "-i", "dialog-warning", "--",
		"Backup failed", "unable to open the repository\nDetails: carryon status"), sysexec.Response{})

	out, err := run(t, dirs, "notify", "carryon-backup.service")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "notified through desktop: alice (left to the agent), bob") {
		t.Errorf("out = %q", out)
	}
	for _, c := range f.Commands() {
		if strings.Contains(c, "notify-send") && strings.Contains(c, "-u alice") {
			t.Errorf("root notified alice too: %q", c)
		}
	}

	t.Run("a failure the history doesn't have", func(t *testing.T) {
		dirs.State = t.TempDir() // the agent can't know of it, so root tells everyone
		f.On(as("alice", 1000, "notify-send", "-u", "critical", "-a", "Carryon", "-i", "dialog-warning", "--",
			"Backup failed", "Details: carryon status"), sysexec.Response{}).
			On(as("bob", 1001, "notify-send", "-u", "critical", "-a", "Carryon", "-i", "dialog-warning", "--",
				"Backup failed", "Details: carryon status"), sysexec.Response{})
		old := notifyErrors
		notifyErrors = func(paths.Dirs, sysexec.Runner) notify.ErrorSource { return stubErrors("") }
		t.Cleanup(func() { notifyErrors = old })
		out, err := run(t, dirs, "notify", "carryon-backup.service")
		if err != nil || !strings.Contains(out, "notified through desktop: alice, bob") {
			t.Errorf("out = %q, err = %v", out, err)
		}
	})
}
