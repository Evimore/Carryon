package agent

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/Evimore/carryon/internal/sysexec"
)

// busConfig is a private session bus that anyone on it may use fully.
const busConfig = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=%s</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// privateBus starts a dbus-daemon of its own for the test and returns its address. It skips the
// test where dbus-daemon isn't installed.
func privateBus(t *testing.T) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon isn't installed (on Arch: pacman -S dbus), so the agent can't be tested on a private bus")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, []byte(strings.Replace(busConfig, "%s", filepath.Join(dir, "bus"), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, "--config-file="+conf, "--nofork", "--nosyslog", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("dbus-daemon didn't start: %v", err)
	}
	return strings.TrimSpace(addr)
}

func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// notificationService is a fake org.freedesktop.Notifications on the bus.
type notificationService struct {
	conn *dbus.Conn
	mu   sync.Mutex
	got  []shownNote
}

func newNotificationService(t *testing.T, addr string) *notificationService {
	t.Helper()
	s := &notificationService{conn: connect(t, addr)}
	if err := s.conn.Export(s, notifyPath, notifyIface); err != nil {
		t.Fatal(err)
	}
	reply, err := s.conn.RequestName(notifyName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("taking %s: %v (reply %d)", notifyName, err, reply)
	}
	return s
}

func (s *notificationService) GetCapabilities() ([]string, *dbus.Error) {
	return []string{"actions", "body"}, nil
}

func (s *notificationService) GetServerInformation() (string, string, string, string, *dbus.Error) {
	return "fake", "example.com", "1", "1.2", nil
}

func (s *notificationService) CloseNotification(uint32) *dbus.Error { return nil }

func (s *notificationService) Notify(app string, replaces uint32, icon, summary, body string, actions []string, hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := Notification{Title: summary, Body: body, Icon: icon}
	if u, ok := hints["urgency"].Value().(byte); ok {
		n.Urgency = u
	}
	for i := 0; i+1 < len(actions); i += 2 {
		n.Actions = append(n.Actions, Action{Key: actions[i], Label: actions[i+1]})
	}
	id := uint32(len(s.got) + 1)
	s.got = append(s.got, shownNote{id, n})
	return id, nil
}

func (s *notificationService) shown() []shownNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shownNote(nil), s.got...)
}

// press presses a button, as the user would.
func (s *notificationService) press(t *testing.T, id uint32, key string) {
	t.Helper()
	if err := s.conn.Emit(notifyPath, notifyIface+".ActionInvoked", id, key); err != nil {
		t.Fatal(err)
	}
	if err := s.conn.Emit(notifyPath, notifyIface+".NotificationClosed", id, uint32(2)); err != nil {
		t.Fatal(err)
	}
}

// Against a notification service on a private bus, each event shows once with the right buttons,
// and each button starts the right unit.
func TestOnABus(t *testing.T) {
	addr := privateBus(t)
	service := newNotificationService(t, addr)

	u := testUser(t)
	d := testDirs(t)
	f := &sysexec.Fake{}
	var conn *dbus.Conn
	newAgent := func() *Agent {
		conn = connect(t, addr)
		bus, err := NewBus(conn)
		if err != nil {
			t.Fatal(err)
		}
		return &Agent{Dirs: d, User: u, Desktop: bus, Runner: f, StateDir: StateDir(u.Home, ""), Settings: settingsOn}
	}

	add, ignore := pathEntry("/var/lib/jellyfin", "jellyfin-server", 3<<30, false), pathEntry("/srv/media", "", 0, false)
	offer := restoreEntry("app-obs-studio", "obs-studio")
	writeStatus(t, d, failedStatus(now))
	putEntries(t, d, add, ignore, offer)
	units := map[string]string{ // what each notification's button starts
		"Backup failed":                 "carryon-backup.service",
		"Back up /var/lib/jellyfin?":    "carryon-add@" + add.ID + ".service",
		"Back up /srv/media?":           "carryon-ignore@" + ignore.ID + ".service",
		"Saved settings for obs-studio": "carryon-restore@app-obs-studio.service",
	}
	press := map[string]string{
		"Backup failed": keyBackup, "Back up /var/lib/jellyfin?": keyAdd,
		"Back up /srv/media?": keyIgnore, "Saved settings for obs-studio": keyRestore,
	}
	buttons := map[string][]string{
		"Backup failed":                 {"backup=Back up now"},
		"Back up /var/lib/jellyfin?":    {"add=Back it up", "ignore=Ignore"},
		"Back up /srv/media?":           {"add=Back it up", "ignore=Ignore"},
		"Saved settings for obs-studio": {"restore=Restore", "later=Not now"},
	}
	for _, unit := range units {
		f.On("systemctl start --no-block "+unit, sysexec.Response{})
	}

	r := start(t, newAgent())
	eventually(t, "four notifications", func() bool { return len(service.shown()) == 4 })
	r.waitEval(t, 3)
	notes := service.shown()
	if len(notes) != 4 {
		t.Fatalf("shown %d notifications, want 4", len(notes))
	}
	var want []string
	for _, n := range notes {
		if got := n.buttons(); !reflect.DeepEqual(got, buttons[n.Title]) {
			t.Errorf("%q has buttons %q, want %q", n.Title, got, buttons[n.Title])
		}
		if n.Title == "Backup failed" && n.Urgency != urgencyCritical {
			t.Errorf("a failed backup has urgency %d", n.Urgency)
		}
		service.press(t, n.ID, press[n.Title])
		want = append(want, "systemctl start --no-block "+units[n.Title])
		eventually(t, units[n.Title]+" to start", func() bool { return len(f.Commands()) == len(want) })
	}
	if got := f.Commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("started %q\nwant %q", got, want)
	}
	r.stop()

	// Started again, it shows nothing it showed before, and a new suggestion once.
	r = start(t, newAgent())
	r.waitEval(t, 3)
	if n := len(service.shown()); n != 4 {
		t.Fatalf("after a restart: %d notifications, want still 4", n)
	}
	putEntries(t, d, pathEntry("/var/opt/game-server", "", 0, false))
	eventually(t, "the new suggestion", func() bool { return len(service.shown()) == 5 })
	r.waitEval(t, 3)
	notes = service.shown()
	if len(notes) != 5 || notes[4].Title != "Back up /var/opt/game-server?" {
		t.Errorf("shown %+v", notes[4:])
	}

	// A button press from anyone but the notification service is ignored.
	other := connect(t, addr)
	if err := other.Emit(notifyPath, notifyIface+".ActionInvoked", notes[4].ID, keyAdd); err != nil {
		t.Fatal(err)
	}
	r.waitEval(t, 3)
	if n := len(f.Commands()); n != 4 {
		t.Errorf("a stranger's signal started a unit: %q", f.Commands())
	}

	// Without its bus, the agent stops with an error, so systemd starts it again.
	conn.Close()
	<-r.done
	if r.err == nil || !strings.Contains(r.err.Error(), "lost the connection") {
		t.Errorf("err = %v", r.err)
	}
}

// Nothing the agent writes lands outside the user's home.
func TestWritesOnlyInTheHome(t *testing.T) {
	desk := newFakeDesktop("actions")
	a, _ := newAgent(t, desk)
	writeStatus(t, a.Dirs, failedStatus(now))
	putEntries(t, a.Dirs, pathEntry("/var/lib/jellyfin", "", 0, false))
	before := listFiles(t, filepath.Dir(a.Dirs.State))
	start(t, a).waitEval(t, 3)
	if after := listFiles(t, filepath.Dir(a.Dirs.State)); !reflect.DeepEqual(before, after) {
		t.Errorf("Carryon's folders changed:\nbefore %q\nafter  %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(a.User.Home, ".local", "state", "carryon", StateFile)); err != nil {
		t.Error(err)
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		files = append(files, p+" "+fi.ModTime().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
