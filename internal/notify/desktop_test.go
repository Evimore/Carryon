package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/sysexec"
)

const listSessions = `     2 1000 alice seat0 tty2
     3 1001 bob   seat0 tty3
     4 1002 carol -     pts/0
     5  120 gdm   seat0 tty1
     6 1003 dave  seat0 tty4
     7 1000 alice -     -
     8 1004 eve   -     -
`

const showSessions = `Id=2
Name=alice
User=1000
Type=wayland
Class=user
State=active
Remote=no

Id=3
Name=bob
User=1001
Type=x11
Class=user
State=online
Remote=no

Id=4
Name=carol
User=1002
Type=tty
Class=user
State=active
Remote=yes

Id=5
Name=gdm
User=120
Type=wayland
Class=greeter
State=online
Remote=no

Id=6
Name=dave
User=1003
Type=wayland
Class=user
State=active
Remote=no

Id=7
Name=alice
User=1000
Type=x11
Class=user
State=online
Remote=no

Id=8
Name=eve
User=1004
Type=wayland
Class=user
State=closing
Remote=no
`

const showCmd = "loginctl show-session -p Id -p Name -p User -p Type -p Class -p State -p Remote 2 3 4 5 6 7 8"

func homes(name string) (sysexec.User, error) {
	if name == "nobody-here" {
		return sysexec.User{}, errors.New("unknown user")
	}
	return sysexec.User{Name: name, Home: "/home/" + name}, nil
}

// settingsOff switches desktop notifications off for the named users.
func settingsOff(names ...string) func(sysexec.User) (config.UserNotify, error) {
	return func(u sysexec.User) (config.UserNotify, error) {
		for _, n := range names {
			if u.Name == n {
				return config.UserNotify{Desktop: false}, nil
			}
		}
		return config.UserNotify{Desktop: true}, nil
	}
}

// asUser is the command line root runs to start argv as the user.
func asUser(name string, uid int, argv ...string) string {
	f := &sysexec.Fake{Root: true, Path: "/usr/bin"}
	f.AsUser(sysexec.User{Name: name, UID: uid, Home: "/home/" + name}).Output(context.Background(), argv[0], argv[1:]...)
	return f.Commands()[0]
}

func notifySend(name string, uid int, m Message) string {
	return asUser(name, uid, "notify-send", "-u", m.Urgency.String(), "-a", "Carryon", "-i", "dialog-warning", "--", m.Title, m.Body)
}

func sessionsFake() *sysexec.Fake {
	return (&sysexec.Fake{Root: true, Path: "/usr/bin", Programs: []string{"notify-send"}}).
		On("loginctl list-sessions --no-legend", sysexec.Response{Stdout: listSessions}).
		On(showCmd, sysexec.Response{Stdout: showSessions})
}

func TestDesktopRecipients(t *testing.T) {
	tests := []struct {
		name  string
		users []string
		off   []string
		euid  int
		want  []string
	}{
		// carol is remote, gdm is the login screen, eve is signing out; alice has two sessions
		{"everyone", []string{"*"}, nil, 0, []string{"alice", "bob", "dave"}},
		{"backup.users limits it", []string{"alice", "carol", "eve"}, nil, 0, []string{"alice"}},
		{"own setting switches it off", []string{"*"}, []string{"bob"}, 0, []string{"alice", "dave"}},
		{"not root: only yourself", []string{"*"}, nil, 1001, []string{"bob"}},
		{"the login screen isn't a user", []string{"gdm", "alice"}, nil, 0, []string{"alice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Desktop{Runner: sessionsFake(), Users: tt.users, Euid: tt.euid, LookupUser: homes, Settings: settingsOff(tt.off...)}
			users, err := d.Recipients(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, u := range users {
				names = append(names, u.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("recipients = %q, want %q", names, tt.want)
			}
		})
	}
}

func TestDesktopSendsAsEachUser(t *testing.T) {
	m := Message{Title: "Backup failed", Body: "-x looks like an option\nDetails: carryon status", Urgency: Critical}
	f := sessionsFake().
		On(notifySend("alice", 1000, m), sysexec.Response{}).
		On(notifySend("bob", 1001, m), sysexec.Response{})
	d := Desktop{Runner: f, Users: []string{"alice", "bob"}, Euid: 0, Path: "/usr/bin", LookupUser: homes, Settings: settingsOff()}
	to, err := d.Send(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if to != "alice, bob" {
		t.Errorf("to = %q", to)
	}
	for _, c := range f.Commands()[2:] {
		if !strings.HasPrefix(c, "runuser -u ") || !strings.Contains(c, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/100") {
			t.Errorf("%q doesn't run as the user, on their bus", c)
		}
	}
}

func TestDesktopFallsBackToBusctl(t *testing.T) {
	m := Test("desktop")
	busctl := asUser("alice", 1000, "busctl", "--user", "--", "call",
		"org.freedesktop.Notifications", "/org/freedesktop/Notifications", "org.freedesktop.Notifications",
		"Notify", "susssasa{sv}i", "Carryon", "0", "dialog-warning", m.Title, m.Body, "0", "1", "urgency", "y", "1", "-1")
	f := sessionsFake().On(busctl, sysexec.Response{})
	f.Programs = nil // no notify-send
	d := Desktop{Runner: f, Users: []string{"alice"}, Path: "/usr/bin", LookupUser: homes, Settings: settingsOff()}
	if _, err := d.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := f.Commands()[2]; got != busctl {
		t.Errorf("ran %q", got)
	}
}

func TestDesktopFailures(t *testing.T) {
	m := Message{Title: "Backup failed", Body: "boom", Urgency: Critical}
	t.Run("nobody signed in", func(t *testing.T) {
		f := (&sysexec.Fake{}).On("loginctl list-sessions --no-legend", sysexec.Response{})
		_, err := Desktop{Runner: f, Users: []string{"*"}}.Send(context.Background(), m)
		if err == nil || !strings.Contains(err.Error(), "nobody whose home is backed up") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("loginctl fails", func(t *testing.T) {
		f := (&sysexec.Fake{}).On("loginctl list-sessions --no-legend", sysexec.Response{Code: 1, Stderr: "Failed to connect to bus"})
		_, err := Desktop{Runner: f, Users: []string{"*"}}.Send(context.Background(), m)
		if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("one user fails, the other gets it", func(t *testing.T) {
		f := sessionsFake().
			On(notifySend("alice", 1000, m), sysexec.Response{Code: 1, Stderr: "GDBus.Error: no notification server"}).
			On(notifySend("bob", 1001, m), sysexec.Response{})
		var warned []string
		d := Desktop{Runner: f, Users: []string{"alice", "bob"}, Path: "/usr/bin", LookupUser: homes, Settings: settingsOff(),
			Warn: func(format string, a ...any) { warned = append(warned, fmt.Sprintf(format, a...)) }}
		to, err := d.Send(context.Background(), m)
		if err != nil || to != "bob" {
			t.Fatalf("to = %q, err = %v", to, err)
		}
		if len(warned) != 1 || !strings.Contains(warned[0], "alice") || !strings.Contains(warned[0], "no notification server") {
			t.Errorf("warnings = %q", warned)
		}
	})
	t.Run("everyone fails", func(t *testing.T) {
		f := sessionsFake().On(notifySend("alice", 1000, m), sysexec.Response{Code: 1, Stderr: "no server"})
		d := Desktop{Runner: f, Users: []string{"alice"}, Path: "/usr/bin", LookupUser: homes, Settings: settingsOff()}
		if _, err := d.Send(context.Background(), m); err == nil || !strings.Contains(err.Error(), "alice") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestParseSessionsWithoutBlankLines(t *testing.T) {
	got := parseSessions("Id=2\nName=alice\nUser=1000\nId=3\nName=bob\nUser=x\n")
	want := []Session{{ID: "2", Name: "alice", UID: 1000}, {ID: "3", Name: "bob", UID: -1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	if covered([]string{"*"}, "bob", -1) {
		t.Error("a session without a user ID is covered")
	}
}

func TestUserSettings(t *testing.T) {
	home := t.TempDir()
	u := sysexec.User{Name: "alice", UID: os.Getuid(), Home: home}
	n, err := UserSettings(u)
	if err != nil || !n.Desktop {
		t.Fatalf("without a file: %+v, %v", n, err)
	}
	file := filepath.Join(home, config.UserConfigPath)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("[notify]\ndesktop = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := UserSettings(u); err != nil || n.Desktop {
		t.Errorf("switched off: %+v, %v", n, err)
	}
	if err := os.WriteFile(file, []byte("[notify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := UserSettings(u); err == nil || !n.Desktop {
		t.Errorf("a broken file must not silence alerts: %+v, %v", n, err)
	}
}
