package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// MinVersion is the oldest restic Carryon works with: it needs restic's exit codes for a missing
// repository and a failed lock, which came in 0.17.
const MinVersion = "0.17.0"

var versionRE = regexp.MustCompile(`(?m)^restic (\d+)\.(\d+)\.(\d+)`)

// ParseVersion reads the version from `restic version` output ("restic 0.19.1 compiled with ...").
func ParseVersion(out string) (major, minor, patch int, err error) {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("can't tell restic's version from %q", out)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, nil
}

// CheckVersion runs `restic version` and returns the version, or an error if restic is missing or
// older than MinVersion.
func (e *Engine) CheckVersion(ctx context.Context) (string, error) {
	r, err := e.Run(ctx, "version")
	if err != nil {
		return "", err
	}
	if err := r.Err(); err != nil {
		return "", err
	}
	major, minor, patch, err := ParseVersion(r.Stdout)
	if err != nil {
		return "", err
	}
	v := fmt.Sprintf("%d.%d.%d", major, minor, patch)
	if major == 0 && minor < 17 {
		return v, &Error{Problem: Problem{
			Cause: "restic " + v + " is too old",
			Fix:   "install restic " + MinVersion + " or newer (on Arch: sudo pacman -Syu restic)",
		}, Code: -1, Args: []string{"version"}}
	}
	return v, nil
}
