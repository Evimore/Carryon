//go:build linux

package ui

import (
	"os"
	"syscall"
)

// stderrIsJournal is true only when stderr is the stream systemd connected to the journal.
func stderrIsJournal() bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(2, &st); err != nil {
		return false
	}
	return journalStreamMatches(os.Getenv("JOURNAL_STREAM"), uint64(st.Dev), st.Ino) //nolint:unconvert // Dev's type differs by architecture
}
