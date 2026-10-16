package setup

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Evimore/carryon/internal/config"
	"github.com/Evimore/carryon/internal/rules"
)

// The prototype's files, relative to Env.SysbackupRoot.
const (
	sysbackupConfDir  = "etc/sysbackup"
	sysbackupStateDir = "var/lib/sysbackup"
	sysbackupHookFile = "etc/pacman.d/hooks/zz-sysbackup.hook"
)

// sysbackupTimers are the prototype's timers.
var sysbackupTimers = []string{"sysbackup.timer", "sysbackup-verify.timer"}

// parseSysbackupConf reads the prototype's KEY=VALUE settings the way its load_conf did: blank
// lines, comments and lines without "=" are skipped, and one pair of matching quotes is removed.
func parseSysbackupConf(data []byte) map[string]string {
	conf := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '"' || v[0] == '\'') {
			v = v[1 : len(v)-1]
		}
		conf[strings.TrimSpace(k)] = v
	}
	return conf
}

// sysbackupPasswordFile is the prototype's default password file, relative to the root.
const sysbackupPasswordFile = "/etc/sysbackup/restic-password"

// convertSysbackup applies the prototype's settings to cfg and returns notes on what changed or
// couldn't be carried over. The settings it doesn't have keep cfg's values.
func convertSysbackup(cfg config.Config, data []byte) (config.Config, []string) {
	conf := parseSysbackupConf(data)
	var notes []string
	note := func(format string, a ...any) { notes = append(notes, fmt.Sprintf(format, a...)) }

	user := conf["SYSBACKUP_USER"]
	if v := conf["RESTIC_REPOSITORY"]; v != "" {
		cfg.Repository.URL = v
	}
	if strings.HasPrefix(cfg.Repository.URL, "rclone:") && user != "" {
		cfg.Repository.RcloneUser = user
	}
	if user != "" {
		// The prototype backed up one user's home; keep it that way rather than widen the backup.
		cfg.Backup.Users = []string{user}
		note("Backing up %s's home, as the prototype did. To back up every user, run: sudo carryon config set backup.users '*'", user)
	}
	if v, ok := conf["EXTRA_PATHS"]; ok {
		cfg.Backup.ExtraPaths = strings.Fields(v)
	}
	if v := conf["AFTER_INSTALL_DELAY"]; v != "" {
		if _, err := config.ParseSpan(v); err == nil || v == "off" {
			cfg.Backup.AfterChangeDelay = v
		} else {
			note("AFTER_INSTALL_DELAY=%s isn't a time Carryon understands; keeping %s", v, cfg.Backup.AfterChangeDelay)
		}
	}
	if v := conf["KEEP"]; v != "" {
		var skipped []string
		cfg.Retention, skipped = convertKeep(cfg.Retention, v)
		if len(skipped) > 0 {
			note("KEEP: Carryon has no setting for %s; left out", strings.Join(skipped, " "))
		}
	}
	intSetting := func(key string, dst *int) {
		v := conf[key]
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			note("%s=%s isn't a number; keeping %d", key, v, *dst)
			return
		}
		*dst = n
	}
	intSetting("PRUNE_EVERY_DAYS", &cfg.Retention.PruneEveryDays)
	intSetting("MAX_AGE_HOURS", &cfg.Verify.MaxAgeHours)
	if v := conf["VERIFY_READ_SUBSET"]; v != "" {
		cfg.Verify.ReadSubset = v
	}
	if conf["TOOLKIT_MIRROR"] != "" {
		note("TOOLKIT_MIRROR isn't needed any more: Carryon keeps its settings inside every backup")
	}
	known := map[string]bool{
		"SYSBACKUP_USER": true, "RESTIC_REPOSITORY": true, "RESTIC_PASSWORD_FILE": true, "TOOLKIT_MIRROR": true,
		"EXTRA_PATHS": true, "KEEP": true, "PRUNE_EVERY_DAYS": true, "MAX_AGE_HOURS": true,
		"AFTER_INSTALL_DELAY": true, "VERIFY_READ_SUBSET": true,
	}
	for _, k := range sortedKeys(conf) {
		if !known[k] {
			note("%s isn't a prototype setting Carryon knows; left out", k)
		}
	}
	return cfg, notes
}

// convertKeep reads restic's --keep-* options from the prototype's KEEP setting. A --keep-* option
// replaces the defaults only when KEEP sets it; options Carryon has no setting for are returned.
func convertKeep(rt config.Retention, keep string) (config.Retention, []string) {
	f := strings.Fields(keep)
	out := config.Retention{PruneEveryDays: rt.PruneEveryDays}
	var skipped []string
	for i := 0; i < len(f); i++ {
		name, val, hasVal := strings.Cut(f[i], "=")
		if !hasVal && i+1 < len(f) && !strings.HasPrefix(f[i+1], "--") {
			val, i = f[i+1], i+1
		}
		n, err := strconv.Atoi(val)
		num := func(dst *int) {
			if err != nil {
				skipped = append(skipped, name+" "+val)
				return
			}
			*dst = n
		}
		switch name {
		case "--keep-within":
			out.KeepWithin = val
		case "--keep-daily":
			num(&out.KeepDaily)
		case "--keep-weekly":
			num(&out.KeepWeekly)
		case "--keep-monthly":
			num(&out.KeepMonthly)
		case "--keep-yearly":
			num(&out.KeepYearly)
		default:
			skipped = append(skipped, strings.TrimSpace(name+" "+val))
		}
	}
	return out, skipped
}

// ownExcludes returns the patterns of the prototype's excludes.txt that aren't among Carryon's
// defaults: the prototype's file held its defaults and the user's own lines together.
func ownExcludes(data []byte, defaults []string) []string {
	have := map[string]bool{}
	for _, d := range defaults {
		have[d] = true
	}
	var own []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || have[ln] {
			continue
		}
		have[ln] = true
		own = append(own, ln)
	}
	return own
}

// convertBundles turns the prototype's bundles.local into bundles.conf, which has the same format.
// It returns nil when the file has no rules, and the problems Carryon finds in it.
func convertBundles(data []byte, name string) ([]byte, []string, error) {
	r, problems, err := rules.ParseBundleRules(bytes.NewReader(data), name)
	if err != nil {
		return nil, nil, err
	}
	if len(r.Titles) == 0 && len(r.Whens) == 0 && len(r.Puts) == 0 && len(r.Drops) == 0 && len(problems) == 0 {
		return nil, nil, nil
	}
	head := "# Bundle rules carried over from the prototype's bundles.local.\n"
	return append([]byte(head), data...), problems, nil
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
