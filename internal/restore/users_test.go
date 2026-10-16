package restore

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/record"
	"github.com/Evimore/carryon/internal/ui"
)

func TestReadUsers(t *testing.T) {
	root := buildTree(t, "here-bob.txt")
	users, err := ReadUsers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("users = %+v", users)
	}
	bob := users[2]
	if bob.Name != "bob" || bob.UID != 1001 || bob.Home != "/srv/home/bob" || bob.Shell != "/usr/bin/zsh" ||
		!slices.Equal(bob.Groups, []string{"wheel"}) || !bob.human() || users[1].human() {
		t.Errorf("users = %+v", users)
	}
	if _, err := ReadUsers(t.TempDir()); err == nil {
		t.Error("no passwd file, no error")
	}
}

func TestMapUsers(t *testing.T) {
	here := []LocalUser{
		{Name: "root", UID: 0, Home: "/root", Shell: "/usr/bin/bash"},
		{Name: "alice", UID: 1000, Home: "/home/alice", Shell: "/usr/bin/zsh"},
		{Name: "carol", UID: 1002, Home: "/home/carol", Shell: "/usr/bin/bash"},
		{Name: "jellyfin", UID: 970, Home: "/var/lib/jellyfin", Shell: "/usr/bin/nologin"},
	}
	one := record.Meta{Format: 1, User: "alice", Home: "/home/alice"}
	two := record.Meta{Format: 2, User: "alice", Home: "/home/alice",
		Users: []record.User{{Name: "alice", Home: "/home/alice"}, {Name: "bob", Home: "/home/bob"}}}
	onlyBob := record.Meta{Format: 1, User: "bob", Home: "/home/bob"}
	tests := []struct {
		name    string
		meta    record.Meta
		here    []LocalUser
		opt     MapOptions
		answers []any
		want    string // FROM>TO pairs
		wantErr string
	}{
		{name: "same name", meta: one, here: here, want: "alice>alice"},
		{name: "given", meta: one, here: here, opt: MapOptions{Given: map[string]string{"alice": "carol"}}, want: "alice>carol"},
		{name: "given user missing", meta: one, here: here, opt: MapOptions{Given: map[string]string{"alice": "dave"}},
			wantErr: "there is no user dave here"},
		{name: "given for nobody in the backup", meta: one, here: here, opt: MapOptions{Given: map[string]string{"zoe": "carol"}},
			wantErr: "the backup has no user zoe"},
		{name: "missing user, asked", meta: two, here: here, answers: []any{"carol"}, want: "alice>alice bob>carol"},
		{name: "missing user, no default", meta: two, here: here, answers: []any{ui.UseDefault},
			wantErr: "--user bob=NAME"},
		{name: "one user, goes to whoever runs the restore", meta: onlyBob, here: here,
			opt: MapOptions{Invoking: "carol"}, answers: []any{ui.UseDefault}, want: "bob>carol"},
		{name: "one person here", meta: onlyBob, here: here[:2], answers: []any{ui.UseDefault}, want: "bob>alice"},
		{name: "nobody here", meta: onlyBob, here: here[:1], wantErr: "create one first (sudo useradd -m bob)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ask := &ui.Script{Answers: tt.answers}
			got, err := MapUsers(tt.meta, tt.here, ask, tt.opt)
			if len(ask.Answers) != 0 {
				t.Errorf("unused answers %v", ask.Answers)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var pairs []string
			for _, m := range got {
				pairs = append(pairs, m.From.Name+">"+m.To.Name)
			}
			if strings.Join(pairs, " ") != tt.want {
				t.Errorf("mapping = %v, want %s", pairs, tt.want)
			}
		})
	}
}

func TestMapUsersMenu(t *testing.T) {
	here := []LocalUser{{Name: "carol", UID: 1002, Home: "/home/carol", Shell: "/bin/bash"}}
	ask := &recordingPrompter{answer: "carol"}
	if _, err := MapUsers(record.Meta{Format: 1, User: "alice", Home: "/home/alice"}, here, ask, MapOptions{}); err != nil {
		t.Fatal(err)
	}
	m := ask.menu
	if m.Title != "Who gets alice's files?" || m.Default != "carol" || m.Flag != "--user alice=NAME" ||
		!strings.Contains(m.Text, "alice (/home/alice) has no account here") || m.Options[0].Note != "/home/carol" {
		t.Errorf("menu = %+v", m)
	}
}

func TestMapUsersCancelled(t *testing.T) {
	here := []LocalUser{{Name: "carol", UID: 1002, Home: "/home/carol", Shell: "/bin/bash"}}
	ask := &ui.Script{Answers: []any{ui.ErrCancelled}}
	_, err := MapUsers(record.Meta{Format: 1, User: "alice", Home: "/home/alice"}, here, ask, MapOptions{})
	if !errors.Is(err, ui.ErrCancelled) {
		t.Errorf("err = %v", err)
	}
}

func TestParseUserFlags(t *testing.T) {
	got, err := ParseUserFlags([]string{"alice=bob", "carol=dave"})
	if err != nil || len(got) != 2 || got["alice"] != "bob" || got["carol"] != "dave" {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"alice", "=bob", "alice="} {
		if _, err := ParseUserFlags([]string{bad}); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
