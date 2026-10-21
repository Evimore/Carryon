package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// shownFormat is the version of agent.json's layout.
const shownFormat = 1

// shown remembers which events were shown, so each shows once, across restarts too. It lives in
// the user's home (StateDir), never in Carryon's state folder.
type shown struct {
	Format int                  `json:"format"`
	Shown  map[string]time.Time `json:"shown"` // event key → when it was shown
	dirty  bool
}

// loadShown reads file. A missing file is nothing shown yet; a damaged one is an error and
// nothing shown, so the agent shows what is current again rather than nothing.
func loadShown(file string) (shown, error) {
	s := shown{Format: shownFormat, Shown: map[string]time.Time{}}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", file, err)
	}
	var got shown
	if err := json.Unmarshal(data, &got); err != nil {
		s.dirty = true
		return s, fmt.Errorf("%s is damaged (%v)", file, err)
	}
	if got.Shown != nil {
		s.Shown = got.Shown
	}
	return s, nil
}

func (s *shown) has(key string) bool {
	_, ok := s.Shown[key]
	return ok
}

func (s *shown) add(key string, at time.Time) {
	s.Shown[key] = at.UTC()
	s.dirty = true
}

// keep forgets every event that isn't in current.
func (s *shown) keep(current map[string]bool) {
	for k := range s.Shown {
		if !current[k] {
			delete(s.Shown, k)
			s.dirty = true
		}
	}
}

// save writes the file if something changed, readable by the user only, in one step.
func (s *shown) save(file string) error {
	if !s.dirty {
		return nil
	}
	s.Format = shownFormat
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remembering what was shown: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+".tmp-")
	if err != nil {
		return fmt.Errorf("remembering what was shown: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		return fmt.Errorf("remembering what was shown in %s: %w", file, err)
	}
	s.dirty = false
	return nil
}
