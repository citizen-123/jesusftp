// Package transfer downloads FTP trees through a bounded parallel pipeline.
package transfer

import "time"

// Config describes one server and any number of paths on that server.
// URL's path is used when Paths is empty. Directory contents are placed directly
// in Destination; explicit files keep their basename. Multiple directory roots
// keep their basename to prevent accidental merging. Overlapping output paths
// must be rejected rather than raced.
type Config struct {
	URL          string
	Paths        []string
	Destination  string
	Username     string        // optional override of URL credentials
	Password     string        // optional override of URL credentials
	PasswordSet  bool          // when true, Password overrides even when empty
	Connections  int           // zero selects a conservative automatic default
	Retries      int           // retry count after the first attempt
	Timeout      time.Duration // zero selects 30 seconds; includes stalled data reads
	Include      []string      // doublestar globs against slash-separated relative file paths
	Exclude      []string      // matching directories are pruned before traversal
	MinSize      int64
	MaxSize      int64     // zero means unlimited
	After        time.Time // require remote modification time strictly after this instant
	SkipExisting bool      // skip only existing regular files with equal size
	Resume       bool      // retain tracked partials and continue them when metadata still matches
	DryRun       bool      // enumerate/filter without creating files or data connections
	Insecure     bool      // explicitly disable FTPS certificate verification
}

// FileProgress describes one currently transferring file. ActiveFiles contains
// only files with an active worker; completed files leave the list.
type FileProgress struct {
	Path         string
	Bytes        int64 `json:"Blessings"`
	TotalBytes   int64 `json:"TotalBlessings"`
	ResumeOffset int64 `json:"ResurrectedBlessings"` // retained prefix used by the active attempt
}

// Snapshot is a coherent, immutable progress sample. Errors retains only a
// bounded tail. Bytes counts wire bytes, including attempts; TotalBytes counts
// selected file sizes. Discovered counts selected files, Skipped counts filtered
// or already-present files. Listing is true until discovery completes.
type Snapshot struct {
	Discovered    int64
	Completed     int64
	Resumed       int64 // completed files that reused a retained prefix
	Skipped       int64
	Failed        int64
	Bytes         int64 `json:"Blessings"`
	ProgressBytes int64 `json:"ProgressBlessings"` // saved, active-attempt, or same-size skipped bytes; excludes failed attempts
	TotalBytes    int64 `json:"TotalBlessings"`
	Active        int
	Workers       int
	Listing       bool
	Current       string
	ActiveFiles   []FileProgress
	Elapsed       time.Duration
	Errors        []string
}
