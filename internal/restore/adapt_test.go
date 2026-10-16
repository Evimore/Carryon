package restore

import (
	"testing"

	"github.com/Evimore/carryon/internal/record"
)

// The expectations are what the prototype's adapt gives for the same input.
func TestAdapter(t *testing.T) {
	toBob := []UserMap{{From: record.User{Name: "alice", Home: "/home/alice"}, To: LocalUser{Name: "bob", Home: "/srv/home/bob"}}}
	renamed := []UserMap{{From: record.User{Name: "alice", Home: "/home/x"}, To: LocalUser{Name: "bob", Home: "/home/x"}}}
	same := []UserMap{{From: record.User{Name: "alice", Home: "/home/alice"}, To: LocalUser{Name: "alice", Home: "/home/alice"}}}
	noHome := []UserMap{{From: record.User{Name: "alice"}, To: LocalUser{Name: "bob", Home: "/home/bob"}}}
	tests := []struct {
		users    []UserMap
		in, want string
	}{
		{toBob, "/home/alice/x", "/srv/home/bob/x"},
		{toBob, "/home/alice2/x", "/home/alice2/x"},
		{toBob, "/home/alice.bak /home/alice-old /home/alice_x", "/home/alice.bak /home/alice-old /home/alice_x"},
		{toBob, "cd /home/alice", "cd /srv/home/bob"},
		{toBob, "PATH=/home/alice:/home/alice/bin", "PATH=/srv/home/bob:/srv/home/bob/bin"},
		{toBob, "User=alice\n", "User=bob\n"},
		{toBob, "  user = alice  \r\nx", "  user = bob  \r\nx"},
		{toBob, "AutoUser=ALICE\n", "AutoUser=bob\n"},
		{toBob, "Username=alice extra\n", "Username=alice extra\n"},
		{toBob, "MyUser=alice\n", "MyUser=alice\n"},
		{toBob, "User=alicex\n", "User=alicex\n"},
		{toBob, "\x00/home/alice", "\x00/home/alice"},
		{toBob, "[a]\nUser=alice\nHome=/home/alice/\n", "[a]\nUser=bob\nHome=/srv/home/bob/\n"},
		{toBob, "/home/alicé", "/home/alicé"},
		{renamed, "User=alice /home/x/y", "User=alice /home/x/y"},
		{renamed, "User=alice\n/home/x/y", "User=bob\n/home/x/y"},
		{same, "User=alice /home/alice", "User=alice /home/alice"},
		{noHome, "User=alice", "User=alice"},
	}
	for _, tt := range tests {
		if got := NewAdapter(tt.users).String(tt.in); got != tt.want {
			t.Errorf("adapt(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Several users are rewritten in one pass, so alice -> bob and bob -> carol don't chain.
func TestAdapterSeveralUsers(t *testing.T) {
	a := NewAdapter([]UserMap{
		{From: record.User{Name: "alice", Home: "/home/alice"}, To: LocalUser{Name: "bob", Home: "/home/bob"}},
		{From: record.User{Name: "bob", Home: "/home/bob"}, To: LocalUser{Name: "carol", Home: "/home/carol"}},
		{From: record.User{Name: "al", Home: "/home/al"}, To: LocalUser{Name: "al", Home: "/home/al"}},
	})
	in := "/home/alice/a /home/bob/b /home/al/c\nUser=alice\nUser=bob\n"
	want := "/home/bob/a /home/carol/b /home/al/c\nUser=bob\nUser=carol\n"
	if got := a.String(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if NewAdapter(nil).Active() || (*Adapter)(nil).Active() {
		t.Error("an empty adapter is active")
	}
}
