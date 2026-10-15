package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Snapshot is one entry of `restic snapshots --json`.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

// ParseSnapshots reads `restic snapshots --json` output, oldest first.
func ParseSnapshots(data []byte) ([]Snapshot, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var snaps []Snapshot
	if err := json.Unmarshal(data, &snaps); err != nil {
		return nil, fmt.Errorf("reading restic's snapshot list: %w", err)
	}
	for i := range snaps {
		if snaps[i].ShortID == "" { // deprecated in restic, so it may go away
			snaps[i].ShortID = snaps[i].ID[:min(8, len(snaps[i].ID))]
		}
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	return snaps, nil
}

// Snapshots lists the repository's snapshots, oldest first. A non-empty host lists only that
// machine's.
func (e *Engine) Snapshots(ctx context.Context, host string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if host != "" {
		args = append(args, "--host", host)
	}
	r, err := e.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return ParseSnapshots([]byte(r.Stdout))
}

// Node is one file, folder or link in a snapshot, from `restic ls --json`.
type Node struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // file, dir, symlink, dev, chardev, fifo, socket
	Path  string    `json:"path"`
	UID   uint32    `json:"uid"`
	GID   uint32    `json:"gid"`
	Size  uint64    `json:"size"`
	Mode  uint32    `json:"mode"` // Go's os.FileMode bits
	MTime time.Time `json:"mtime"`
}

// lsLine is one line of `restic ls --json`. restic 0.17 marks lines with struct_type; later
// versions add message_type.
type lsLine struct {
	MessageType string `json:"message_type"`
	StructType  string `json:"struct_type"`
	Node
}

// ParseLs reads the node stream of `restic ls --json`, calling fn for each node. The snapshot line
// that starts the stream is skipped.
func ParseLs(r io.Reader, fn func(Node) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l lsLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return fmt.Errorf("reading restic's file list, line %d: %w", n, err)
		}
		if l.MessageType != "node" && l.StructType != "node" {
			continue
		}
		if err := fn(l.Node); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading restic's file list: %w", err)
	}
	return nil
}

// Ls lists the nodes of a snapshot ("latest" works), limited to paths when given.
func (e *Engine) Ls(ctx context.Context, snapshot string, paths ...string) ([]Node, error) {
	r, err := e.Run(ctx, append([]string{"ls", "--json", snapshot}, paths...)...)
	if err != nil {
		return nil, err
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	var nodes []Node
	err = ParseLs(strings.NewReader(r.Stdout), func(n Node) error {
		nodes = append(nodes, n)
		return nil
	})
	return nodes, err
}

// Summary is the summary message of `restic backup --json`.
type Summary struct {
	DryRun              bool      `json:"dry_run"`
	FilesNew            uint64    `json:"files_new"`
	FilesChanged        uint64    `json:"files_changed"`
	FilesUnmodified     uint64    `json:"files_unmodified"`
	DirsNew             uint64    `json:"dirs_new"`
	DirsChanged         uint64    `json:"dirs_changed"`
	DirsUnmodified      uint64    `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           uint64    `json:"data_added"`
	DataAddedPacked     uint64    `json:"data_added_packed"`
	TotalFilesProcessed uint64    `json:"total_files_processed"`
	TotalBytesProcessed uint64    `json:"total_bytes_processed"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	TotalDuration       float64   `json:"total_duration"` // seconds
	SnapshotID          string    `json:"snapshot_id"`    // empty for a dry run, or when nothing changed and --skip-if-unchanged is set
}

// BackupError is an `error` message of `restic backup --json`: a file that couldn't be read.
type BackupError struct {
	Item    string `json:"item"`
	During  string `json:"during"`
	Message string // the error text
}

// BackupOutput is what ParseBackup found in `restic backup --json` output.
type BackupOutput struct {
	Summary *Summary // nil if restic didn't finish
	Errors  []BackupError
}

// ParseBackup reads the JSON lines of `restic backup --json`, keeping the summary and the files
// restic couldn't read. Status lines and lines that aren't JSON are skipped.
func ParseBackup(r io.Reader) (BackupOutput, error) {
	var out BackupOutput
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var head struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal([]byte(line), &head) != nil {
			continue
		}
		switch head.MessageType {
		case "summary":
			var s Summary
			if err := json.Unmarshal([]byte(line), &s); err != nil {
				return out, fmt.Errorf("reading restic's backup summary: %w", err)
			}
			out.Summary = &s
		case "error":
			var m struct {
				Error  json.RawMessage `json:"error"`
				Item   string          `json:"item"`
				During string          `json:"during"`
			}
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			out.Errors = append(out.Errors, BackupError{Item: m.Item, During: m.During, Message: errorText(m.Error)})
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("reading restic's backup output: %w", err)
	}
	return out, nil
}

// errorText reads the error field of a restic JSON message: {"message": "..."} in restic 0.17 and
// newer, a plain string in some older outputs.
func errorText(raw json.RawMessage) string {
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
		return obj.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// RepoConfig is the repository's config file, from `restic cat config`.
type RepoConfig struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// ParseRepoConfig reads the output of `restic cat config`.
func ParseRepoConfig(data []byte) (RepoConfig, error) {
	var c RepoConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return RepoConfig{}, fmt.Errorf("reading the repository's config: %w", err)
	}
	if c.ID == "" {
		return RepoConfig{}, fmt.Errorf("the repository's config has no id")
	}
	return c, nil
}

// RepositoryID returns the repository's unique ID, which tells one repository from another even
// when they're at the same path.
func (e *Engine) RepositoryID(ctx context.Context) (string, error) {
	r, err := e.Run(ctx, "cat", "config")
	if err != nil {
		return "", err
	}
	if err := r.Err(); err != nil {
		return "", err
	}
	c, err := ParseRepoConfig([]byte(r.Stdout))
	return c.ID, err
}
