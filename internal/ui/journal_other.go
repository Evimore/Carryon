//go:build !linux

package ui

func stderrIsJournal() bool { return false }
