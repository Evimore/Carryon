package sysexec_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/Evimore/carryon/internal/sysexec"
)

// explicitPackages is code under test: it takes a Runner, so tests can hand it a Fake.
func explicitPackages(ctx context.Context, r sysexec.Runner) ([]string, error) {
	out, err := r.Output(ctx, "pacman", "-Qqe")
	if err != nil {
		return nil, fmt.Errorf("listing packages: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// A table test registers what each command prints, then checks the result and the commands run.
func ExampleFake() {
	tests := []struct {
		name      string
		responses map[string]sysexec.Response
	}{
		{"two packages", map[string]sysexec.Response{
			"pacman -Qqe": {Stdout: "base\nvim\n"},
		}},
		{"pacman fails", map[string]sysexec.Response{
			"pacman -Qqe": {Stderr: "error: could not open database", Code: 1},
		}},
		{"nothing registered", nil},
	}
	for _, tt := range tests {
		f := &sysexec.Fake{Responses: tt.responses}
		pkgs, err := explicitPackages(context.Background(), f)
		fmt.Printf("%s: %q, %v, ran %q\n", tt.name, pkgs, err, f.Commands())
	}
	// Output:
	// two packages: ["base" "vim"], <nil>, ran ["pacman -Qqe"]
	// pacman fails: [], listing packages: pacman -Qqe failed (exit status 1):
	// error: could not open database, ran ["pacman -Qqe"]
	// nothing registered: [], listing packages: unexpected command: pacman -Qqe, ran ["pacman -Qqe"]
}

// Commands for the backed-up user go through AsUser. With Root set, the fake shows the runuser
// wrapping a root process would add.
func ExampleFake_AsUser() {
	f := &sysexec.Fake{Root: true, Path: "/usr/bin"}
	alice := sysexec.User{Name: "alice", UID: 1000, Home: "/home/alice"}
	_, err := f.AsUser(alice).Output(context.Background(), "dconf", "dump", "/")
	fmt.Println(err)
	// Output:
	// unexpected command: runuser -u alice -- env HOME=/home/alice USER=alice XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus PATH=/home/alice/.cargo/bin:/home/alice/.local/bin:/usr/bin dconf dump /
}
