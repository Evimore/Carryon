// Package version holds build information, set at link time:
//
//	go build -ldflags "-X github.com/Evimore/carryon/internal/version.Version=0.1.0"
package version

var (
	// Version is the release version, "dev" for local builds.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = ""
	// Date is the build date (RFC 3339).
	Date = ""
)

// String describes the build in one line.
func String() string {
	s := "carryon " + Version
	if Commit != "" {
		s += " (" + Commit
		if Date != "" {
			s += ", " + Date
		}
		s += ")"
	}
	return s
}
