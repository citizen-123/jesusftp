package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jesusftp/jesusftp/internal/transfer"
)

func TestParseSizeBoundaries(t *testing.T) {
	for input, want := range map[string]int64{"0": 0, "2": 2, "500B": 500, "2KiB": 2048, "3MiB": 3145728, "2MB": 2000000, "9223372036854775807B": 9223372036854775807, "7 blessings": 7, "1bl": 1, "3Kbl": 3072, "2Mbl": 2097152, "1Gbl": 1 << 30, "1Tbl": 1 << 40} {
		got, err := parseSize(input)
		if err != nil || got != want {
			t.Errorf("parseSize(%q)=%d,%v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"-1B", "2garbageB", "9223372036854775808B", "9223372036854775807GiB", "12 34KiB", "NaNMiB", "", "8388608Tbl", "-1bl", "1.5Mbl"} {
		if got, err := parseSize(input); err == nil {
			t.Errorf("parseSize(%q) accepted as %d", input, got)
		}
	}
}

func TestEmptyManifestDoesNotSelectWholeServer(t *testing.T) {
	if paths, err := appendPaths(nil, "-", strings.NewReader("\n\r\n")); err == nil {
		t.Fatalf("empty explicit manifest falls back to URL root: %v", paths)
	}
}
func TestManifestValidationPrecedesPasswordPrompt(t *testing.T) {
	var stdout, stderr strings.Builder
	exitCode := Run(
		[]string{"--from", "-", "ftp://saint@example.org/private"},
		strings.NewReader("\n"),
		&stdout,
		&stderr,
	)
	if exitCode != 2 {
		t.Fatalf("Run() exit=%d, stderr=%q; want manifest error before password prompt", exitCode, stderr.String())
	}
}

func TestPasswordPromptSkipsURLPassword(t *testing.T) {
	password, set, err := passwordFor(context.Background(), "ftp://saint:secret@example.org/private", "")
	if err != nil || set || password != "" {
		t.Fatalf("passwordFor()=%q,%v,%v; want no prompt", password, set, err)
	}
}

func TestPasswordPromptSkipsAnonymousUser(t *testing.T) {
	password, set, err := passwordFor(context.Background(), "ftp://anonymous@example.org/private", "")
	if err != nil || set || password != "" {
		t.Fatalf("passwordFor()=%q,%v,%v; want no prompt", password, set, err)
	}
}

func TestPasswordPromptHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := passwordFor(ctx, "ftp://saint@example.org/private", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("passwordFor() error=%v; want context.Canceled", err)
	}
}

func TestDashboardShowsEachActiveFile(t *testing.T) {
	m := dashboard{
		ctx:      context.Background(),
		progress: &progressState{latest: transfer.Snapshot{ActiveFiles: []transfer.FileProgress{{Path: "/one.bin", Bytes: 1, TotalBytes: 4}, {Path: "/two.bin", Bytes: 3, TotalBytes: 6}}}},
		width:    80,
		height:   24,
		color:    false,
	}
	view := m.View()
	if !strings.Contains(view, "/one.bin") || !strings.Contains(view, "/two.bin") {
		t.Fatalf("dashboard omitted active file paths:\n%s", view)
	}
}
