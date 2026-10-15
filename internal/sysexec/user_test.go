package sysexec

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestAsUser(t *testing.T) {
	alice := User{Name: "alice", UID: 1000, Home: "/home/alice"}
	wrapped := []string{
		"runuser", "-u", "alice", "--", "env",
		"HOME=/home/alice",
		"USER=alice",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"PATH=/home/alice/.cargo/bin:/home/alice/.local/bin:/usr/local/bin:/usr/bin",
		"gsettings", "list-recursively",
	}
	tests := []struct {
		name string
		user User
		root bool
		path string
		want []string
	}{
		{"root runs a user's command through runuser", alice, true, "/usr/local/bin:/usr/bin", wrapped},
		{"not root: runs directly", alice, false, "/usr/bin", []string{"gsettings", "list-recursively"}},
		{"the user is root: runs directly", User{Name: "root", UID: 0, Home: "/root"}, true, "/usr/bin",
			[]string{"gsettings", "list-recursively"}},
		{"empty PATH falls back to /usr/bin", alice, true, "",
			append(append([]string{}, wrapped[:9]...),
				"PATH=/home/alice/.cargo/bin:/home/alice/.local/bin:/usr/bin", "gsettings", "list-recursively")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &Fake{}
			f.On(Join(tt.want), Response{Stdout: "ok"})
			r := AsUser(f, tt.user, tt.root, tt.path)

			out, err := r.Output(context.Background(), "gsettings", "list-recursively")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != "ok" {
				t.Errorf("stdout = %q", out)
			}
			if err := r.Run(context.Background(), "gsettings", "list-recursively"); err != nil {
				t.Fatal(err)
			}
			calls := f.Calls()
			if len(calls) != 2 {
				t.Fatalf("calls = %v", calls)
			}
			for _, c := range calls {
				if !reflect.DeepEqual(c.Args, tt.want) {
					t.Errorf("argv = %q\nwant   %q", c.Args, tt.want)
				}
			}
			if !calls[1].Attached {
				t.Error("Run's call isn't marked attached")
			}
		})
	}
}

func TestAsUserNeedsNameAndHome(t *testing.T) {
	f := &Fake{}
	r := AsUser(f, User{UID: 1000}, true, "/usr/bin")
	if _, err := r.Output(context.Background(), "true"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("ran %v anyway", f.Commands())
	}
}

func TestAsUserExistsAsksTheInnerRunner(t *testing.T) {
	f := &Fake{Programs: []string{"gsettings"}}
	r := AsUser(f, User{Name: "alice", UID: 1000, Home: "/home/alice"}, true, "/usr/bin")
	if !r.Exists("gsettings") || r.Exists("dconf") {
		t.Error("Exists doesn't follow the inner runner")
	}
}

func TestFakeAsUser(t *testing.T) {
	alice := User{Name: "alice", UID: 1000, Home: "/home/alice"}

	plain := &Fake{}
	plain.On("dconf dump /", Response{})
	if _, err := plain.AsUser(alice).Output(context.Background(), "dconf", "dump", "/"); err != nil {
		t.Errorf("without Root the command should pass through unchanged: %v", err)
	}

	root := &Fake{Root: true, Path: "/usr/bin"}
	_, err := root.AsUser(alice).Output(context.Background(), "dconf", "dump", "/")
	if err == nil || !strings.HasPrefix(err.Error(), "unexpected command: runuser -u alice -- env HOME=/home/alice") {
		t.Errorf("err = %v", err)
	}
}

func TestExecAsUserWhenNotRoot(t *testing.T) {
	// Tests don't run as root on a developer's machine, but CI's Arch container does. Either way
	// nothing real runs here: only check that the wrapping matches the process's own user.
	e := New()
	r := e.AsUser(User{Name: "alice", UID: 1000, Home: "/home/alice"})
	if _, wrapped := r.(*userRunner); wrapped != isRoot() {
		t.Errorf("wrapped = %v, running as root = %v", wrapped, isRoot())
	}
}
