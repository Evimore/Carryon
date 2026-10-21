package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Evimore/carryon/internal/agent"
	"github.com/Evimore/carryon/internal/paths"
	"github.com/Evimore/carryon/internal/state"
)

// The units and the pacman hook in dist/ run commands that exist, and their conditions name the
// flags Carryon writes.
func TestDistRunsRealCommands(t *testing.T) {
	files, _ := filepath.Glob("../../dist/*/*")
	if len(files) == 0 {
		t.Fatal("no files in dist/")
	}
	def := paths.Dirs{State: "/var/lib/carryon"}
	root := NewRoot(nil, paths.Default())
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// %I unescapes "-" to "/", so carryon-add@-etc-shadow.service would pass /etc/shadow.
		if strings.Contains(string(data), "%I") {
			t.Errorf("%s uses %%I: instance names reach Carryon as they are, with %%i", f)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			switch key {
			case "ExecStart", "Exec":
				words := strings.Fields(val)
				if words[0] != "/usr/bin/carryon" || len(words) < 2 {
					t.Errorf("%s: %s runs %q", f, key, val)
					continue
				}
				cmd, _, err := root.Find(words[1:2])
				if err != nil || cmd == root {
					t.Errorf("%s: carryon has no %q command", f, words[1])
					continue
				}
				args := strings.NewReplacer("%i", "path-3fa9c2d1e0", "%n", "carryon-backup.service").Replace(strings.Join(words[2:], " "))
				if err := cmd.ParseFlags(strings.Fields(args)); err != nil {
					t.Errorf("%s: %s: %v", f, key, err)
				}
			case "ConditionPathExists":
				p := strings.TrimPrefix(val, "!")
				if p != def.PendingFlag() && p != def.RestoringFlag() {
					t.Errorf("%s: condition on %s, which Carryon doesn't write", f, p)
				}
			}
		}
	}
}

// polkitHarness runs dist/polkit/carryon.rules with a fake polkit and answers each case read
// from standard input, one JSON object per line, with the rule's result ("" when it doesn't
// handle the case).
const polkitHarness = `
const fs = require("fs");
const rules = [];
const polkit = {
	Result: {NO: "no", YES: "yes", AUTH_SELF: "auth_self", AUTH_SELF_KEEP: "auth_self_keep",
		AUTH_ADMIN: "auth_admin", AUTH_ADMIN_KEEP: "auth_admin_keep", NOT_HANDLED: null},
	addRule: function (f) { rules.push(f); },
	addAdminRule: function () {},
	log: function () {},
};
new Function("polkit", fs.readFileSync(process.argv[1], "utf8"))(polkit);
for (const line of fs.readFileSync(0, "utf8").split("\n")) {
	if (!line) continue;
	const c = JSON.parse(line);
	const action = {id: c.action, lookup: function (k) { return c.details[k]; }};
	const subject = {user: "alice", local: c.local, active: c.active,
		isInGroup: function (g) { return c.groups.indexOf(g) >= 0; }};
	let result = null;
	for (const f of rules) {
		result = f(action, subject);
		if (result !== null && result !== undefined) break;
	}
	console.log(result === null || result === undefined ? "" : result);
}
`

// The polkit rules let anyone at the machine start a backup, let wheel restore a bundle or settle
// a suggestion and ask others for an administrator's password, and leave everything else alone:
// other verbs and units, instance names that aren't IDs, and remote or inactive sessions. Needs
// node, which polkit's Duktape stands in for; the CI job on Ubuntu has it.
func TestPolkitRules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	const manage = "org.freedesktop.systemd1.manage-units"
	type tc struct {
		unit, verb    string
		wheel, remote bool
		inactive      bool
		action        string
		want          string
	}
	add := agent.AddUnit(state.EntryID(state.KindPath, "/var/lib/jellyfin"))
	cases := []tc{
		{unit: "carryon-backup.service", want: "yes"},
		{unit: "carryon-backup.service", wheel: true, want: "yes"},
		{unit: "carryon-backup.service", remote: true},
		{unit: "carryon-backup.service", inactive: true},
		{unit: "carryon-backup.service", verb: "stop"},
		{unit: "carryon-backup.service", verb: "restart"},
		{unit: "carryon-backup.service", action: "org.freedesktop.systemd1.manage-unit-files"},
		{unit: add, wheel: true, want: "yes"},
		{unit: add, want: "auth_admin_keep"},
		{unit: add, wheel: true, remote: true},
		{unit: add, wheel: true, verb: "stop"},
		{unit: agent.IgnoreUnit("path-3fa9c2d1e0"), want: "auth_admin_keep"},
		{unit: agent.BackupUnit, want: "yes"},
		{unit: "carryon-ignore@restore-3fa9c2d1e0.service", wheel: true, want: "yes"},
		{unit: "carryon-restore@app-obs-studio.service", wheel: true, want: "yes"},
		{unit: "carryon-restore@app-obs-studio.service", want: "auth_admin_keep"},
		{unit: `carryon-restore@app-gtk\x2b.service`, want: "auth_admin_keep"},
		{unit: `carryon-restore@app\x2dobs\x2dstudio.service`, want: "auth_admin_keep"},
		{unit: `carryon-restore@app-foo\x40bar.service`, wheel: true, want: "yes"},
		{unit: agent.RestoreUnit("app-gtk+@2"), wheel: true, want: "yes"},
		// Not IDs: fall through to the defaults.
		{unit: "carryon-add@-etc-shadow.service", wheel: true},
		{unit: "carryon-ignore@-etc.service", wheel: true},
		{unit: "carryon-add@path-3FA9C2D1E0.service", wheel: true},
		{unit: "carryon-add@path-3fa9c2d1e0a.service", wheel: true},
		{unit: "carryon-add@path-3fa9c2d1e.service", wheel: true},
		{unit: "carryon-add@app-obs-studio.service", wheel: true},
		{unit: "carryon-add@path-3fa9c2d1e0.timer", wheel: true},
		{unit: "carryon-add@.service", wheel: true},
		{unit: "carryon-restore@-etc-shadow.service", wheel: true},
		{unit: `carryon-restore@\x2fetc.service`, wheel: true},
		{unit: `carryon-restore@app\x2fetc.service`, wheel: true},
		{unit: `carryon-restore@app\x20x.service`, wheel: true},
		{unit: `carryon-restore@app\x2e\x2e.service`, wheel: true},
		{unit: `carryon-restore@\x40app.service`, wheel: true},
		{unit: "carryon-restore@.service", wheel: true},
		{unit: "carryon-restore@" + strings.Repeat("a", 201) + ".service", wheel: true},
		{unit: "carryon-notify@carryon-backup.service.service", wheel: true},
		{unit: "carryon-verify.service", wheel: true},
		{unit: "carryon-settle.service"},
		{unit: "xcarryon-backup.service"},
		{unit: "carryon-backup.service.d"},
		{unit: ""},
	}
	var in bytes.Buffer
	for _, c := range cases {
		if c.verb == "" {
			c.verb = "start"
		}
		if c.action == "" {
			c.action = manage
		}
		groups := []string{"alice"}
		if c.wheel {
			groups = append(groups, "wheel")
		}
		line, _ := json.Marshal(map[string]any{
			"action": c.action, "details": map[string]string{"unit": c.unit, "verb": c.verb},
			"local": !c.remote, "active": !c.inactive, "groups": groups,
		})
		in.Write(append(line, '\n'))
	}
	cmd := exec.Command(node, "-e", polkitHarness, "../../dist/polkit/carryon.rules")
	cmd.Stdin = &in
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(cases) {
		t.Fatalf("%d answers for %d cases:\n%s", len(got), len(cases), out)
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("%+v: got %q, want %q", c, got[i], c.want)
		}
	}
}
