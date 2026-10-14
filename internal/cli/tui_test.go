package cli

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/citizen-123/jesusftp/internal/transfer"
)

func testDashboard(width, height int, files []transfer.FileProgress) dashboard {
	return dashboard{
		ctx:      context.Background(),
		progress: &progressState{latest: transfer.Snapshot{ActiveFiles: files}},
		width:    width,
		height:   height,
	}
}

func TestDashboardViewFitsTerminal(t *testing.T) {
	files := []transfer.FileProgress{{Path: "長い名前のファイル.txt", TotalBytes: 10, Bytes: 5}}
	for _, size := range [][2]int{{1, 1}, {8, 2}, {20, 3}, {30, 6}, {32, 8}, {52, 12}} {
		view := testDashboard(size[0], size[1], files).View()
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Fatalf("%dx%d view has %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if width := ansi.StringWidth(line); width > size[0] {
				t.Fatalf("%dx%d line width is %d: %q", size[0], size[1], width, line)
			}
		}
	}
}

func TestDashboardPagingReachesLastFile(t *testing.T) {
	files := make([]transfer.FileProgress, 5)
	for i := range files {
		files[i] = transfer.FileProgress{Path: "file-" + string(rune('a'+i)), TotalBytes: 1}
	}
	m := testDashboard(80, 7, files)
	for range files {
		model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = model.(dashboard)
	}
	if m.fileOffset != len(files)-m.filePageSize() {
		t.Fatalf("offset = %d, want final page", m.fileOffset)
	}
	view := m.View()
	if !strings.Contains(view, "file-e") {
		t.Fatalf("last file page missing from %q", view)
	}
}

func TestDashboardPagingClampsAfterActiveFilesChangeAndResize(t *testing.T) {
	files := make([]transfer.FileProgress, 5)
	for i := range files {
		files[i] = transfer.FileProgress{Path: "file", TotalBytes: 1}
	}
	m := testDashboard(80, 7, files)
	for range files {
		model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = model.(dashboard)
	}
	m.progress.mu.Lock()
	m.progress.latest.ActiveFiles = files[:2]
	m.progress.mu.Unlock()
	model, _ := m.Update(pulse{})
	m = model.(dashboard)
	if m.fileOffset != 1 {
		t.Fatalf("offset after completion = %d, want 1", m.fileOffset)
	}
	model, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m = model.(dashboard)
	if m.fileOffset != 0 {
		t.Fatalf("offset after resize = %d, want 0", m.fileOffset)
	}
}

func TestFileProgressLineTreatsZeroByteFileAsComplete(t *testing.T) {
	line := fileProgressLine(transfer.FileProgress{Path: "empty", TotalBytes: 0}, 40, lipgloss.NewStyle(), lipgloss.NewStyle())
	if !strings.Contains(line, "100%") {
		t.Fatalf("zero-byte file looks incomplete: %q", line)
	}
}

func TestFileLabelKeepsBasenameWhenItFits(t *testing.T) {
	label := fileLabel("/deeply/nested/path/report.csv", 14)
	if !strings.Contains(label, "report.csv") {
		t.Fatalf("basename missing from %q", label)
	}
}
