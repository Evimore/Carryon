package setup

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Evimore/carryon/internal/engine"
)

func TestGeneratePassword(t *testing.T) {
	a, err := GeneratePassword(randReader())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GeneratePassword(randReader())
	if len(a) != PasswordLength || a == b {
		t.Errorf("passwords %q and %q", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune(passwordChars, r) {
			t.Errorf("%q has %q", a, r)
		}
	}
	// Bytes past the last whole multiple of 62 are skipped, so every character is equally likely.
	pw, err := GeneratePassword(bytes.NewReader(append(bytes.Repeat([]byte{255}, 64), bytes.Repeat([]byte{63}, 64)...)))
	if err != nil || pw != strings.Repeat("B", PasswordLength) {
		t.Errorf("got %q, %v", pw, err)
	}
}

func TestMachines(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 1, h, 0, 0, 0, time.UTC) }
	got := machines([]engine.Snapshot{
		{ID: "a1", Hostname: "desktop", Time: at(1)},
		{ID: "b1", Hostname: "laptop", Time: at(5)},
		{ID: "a2", Hostname: "desktop", Time: at(3)},
	})
	want := []Machine{
		{Name: "laptop", Newest: at(5), Snapshot: "b1", Count: 1},
		{Name: "desktop", Newest: at(3), Snapshot: "a2", Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestKitForAnRcloneRepository(t *testing.T) {
	var b bytes.Buffer
	Kit{URL: "rclone:my-remote:backups/carryon", MachineName: "desktop", Password: "pw", RcloneUser: "alice"}.Write(&b)
	for _, s := range []string{"rclone login:  alice's rclone remote my-remote:", "set up the same rclone remote: rclone config",
		"restic -r rclone:my-remote:backups/carryon restore latest --host desktop"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("kit lacks %q:\n%s", s, b.String())
		}
	}
}
