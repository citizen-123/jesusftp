package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/citizen-123/jesusftp/internal/transfer"
)

type engineDone struct {
	snapshot transfer.Snapshot
	err      error
}
type pulse time.Time

type progressState struct {
	mu     sync.RWMutex
	latest transfer.Snapshot
}

type dashboard struct {
	ctx           context.Context
	cancel        context.CancelFunc
	progress      *progressState
	done          <-chan engineDone
	result        engineDone
	finished      bool
	fileOffset    int
	width, height int
	color         bool
	dryRun        bool
}

func runTUI(ctx context.Context, cancel context.CancelFunc, cfg transfer.Config, input io.Reader, output io.Writer, color bool) (transfer.Snapshot, error) {
	done := make(chan engineDone, 1)
	progress := &progressState{latest: transfer.Snapshot{Listing: true}}
	m := dashboard{ctx: ctx, cancel: cancel, progress: progress, done: done, color: color, dryRun: cfg.DryRun}
	go func() {
		s, err := transfer.Run(ctx, cfg, func(s transfer.Snapshot) { progress.mu.Lock(); progress.latest = s; progress.mu.Unlock() })
		done <- engineDone{s, err}
	}()
	p := tea.NewProgram(m, tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		cancel()
		r := <-done
		if r.err != nil {
			return r.snapshot, r.err
		}
		return r.snapshot, err
	}
	model := final.(dashboard)
	if !model.finished { // The terminal may close before Bubble Tea receives a key.
		cancel()
		r := <-done
		return r.snapshot, r.err
	}
	return model.result.snapshot, model.result.err
}

func (m dashboard) Init() tea.Cmd { return tick() }
func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return pulse(t) })
}
func (m dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.fileOffset = m.clampedFileOffset()
	case tea.KeyMsg:
		switch v.String() {
		case "q", "ctrl+c":
			m.cancel()
		case "up", "k":
			m.fileOffset--
		case "down", "j":
			m.fileOffset++
		case "pgup":
			m.fileOffset -= m.filePageSize()
		case "pgdown":
			m.fileOffset += m.filePageSize()
		}
		m.fileOffset = m.clampedFileOffset()
	case pulse:
		select {
		case r := <-m.done:
			m.result, m.finished = r, true
			return m, tea.Quit
		default:
			m.fileOffset = m.clampedFileOffset()
			// Sampling at ten frames per second keeps transfer workers off the UI path.
			return m, tick()
		}
	}
	return m, nil
}
func (m dashboard) snapshot() transfer.Snapshot {
	m.progress.mu.RLock()
	defer m.progress.mu.RUnlock()
	return m.progress.latest
}

func (m dashboard) filePageSize() int {
	if m.height < 3 {
		return 1
	}
	headers := 1
	if m.height >= 4 {
		headers = 2
	}
	if m.height >= 6 {
		headers = 4
	}
	return max(1, m.height-1-headers-1)
}

func (m dashboard) clampedFileOffset() int {
	files := len(m.snapshot().ActiveFiles)
	return max(0, min(m.fileOffset, max(0, files-m.filePageSize())))
}

func (m dashboard) View() string {
	s := m.snapshot()
	if m.width < 1 || m.height < 1 {
		return ""
	}
	width := min(m.width, 92)
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("141")).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	good := lipgloss.NewStyle().Foreground(lipgloss.Color("79"))
	bad := lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	if !m.color {
		accent, muted, good, bad = lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle(), lipgloss.NewStyle()
	}
	state := dashboardMessage(s, m.ctx.Err() != nil)
	if m.dryRun && m.ctx.Err() == nil {
		state = "Previewing remote files (dry run)"
	}
	if m.finished {
		switch {
		case errors.Is(m.result.err, context.Canceled):
			state = "Cancelled"
		case m.result.err != nil || s.Failed > 0:
			state = "Finished with errors"
		case m.dryRun:
			state = "Dry run complete · no files downloaded"
		default:
			state = "Complete · It is finished"
		}
	}
	counts := fmt.Sprintf("Downloaded: %d/%d · Skipped: %d", s.Completed, s.Discovered, s.Skipped)
	if m.dryRun {
		counts = fmt.Sprintf("Selected files: %d · Skipped: %d", s.Discovered, s.Skipped)
	}
	if s.Failed > 0 {
		label := "Failed"
		if m.ctx.Err() != nil {
			label = "Failed/stopped"
		}
		counts += bad.Render(fmt.Sprintf(" · %s: %d", label, s.Failed))
	}
	metrics := fmt.Sprintf("Received: %s · Avg: %s · Workers: %d/%d", formatBytes(s.Bytes), rate(s), s.Active, s.Workers)
	if m.dryRun {
		metrics = fmt.Sprintf("Selected size: %s · no files downloaded", formatBytes(s.TotalBytes))
	}
	headers := []string{accent.Render(state)}
	if m.height >= 4 {
		headers = append(headers, counts)
	}
	if m.height >= 6 {
		headers = append([]string{
			accent.Render("JESUSFTP") + muted.Render("   LET THERE BE FILES"),
			accent.Render(state),
			counts,
			metrics,
		})
	}
	if width < 60 && len(headers) == 4 && !m.dryRun {
		headers[3] = fmt.Sprintf("%s received · %s avg · %d active", formatBytes(s.Bytes), rate(s), s.Active)
	}
	footer := muted.Render(fmt.Sprintf("q/ctrl+c: cancel · %s elapsed", s.Elapsed.Round(time.Second)))
	contentHeight := m.height - 1
	if m.height <= 2 && len(s.ActiveFiles) > 0 {
		headers = nil
	}
	lines := headers[:min(len(headers), contentHeight)]
	available := contentHeight - len(lines)
	if len(s.ActiveFiles) > 0 && available > 0 {
		pageSize := available
		if len(s.ActiveFiles) > pageSize {
			pageSize--
		}
		pageSize = max(1, pageSize)
		offset := max(0, min(m.fileOffset, max(0, len(s.ActiveFiles)-pageSize)))
		end := min(len(s.ActiveFiles), offset+pageSize)
		for _, file := range s.ActiveFiles[offset:end] {
			lines = append(lines, fileProgressLine(file, width, good, muted))
		}
		if len(s.ActiveFiles) > pageSize && len(lines) < contentHeight {
			lines = append(lines, muted.Render(fmt.Sprintf("FILES %d-%d/%d · ↑↓ PgUp/PgDn: scroll", offset+1, end, len(s.ActiveFiles))))
		}
	} else if len(s.ActiveFiles) == 0 && available > 0 && s.Current != "" {
		lines = append(lines, muted.Render("LAST PATH  ")+safeText(s.Current))
	}
	if len(s.ActiveFiles) == 0 && len(lines) < contentHeight && len(s.Errors) > 0 {
		lines = append(lines, bad.Render("ERROR  ")+safeText(s.Errors[len(s.Errors)-1]))
	}
	lines = append(lines, footer)
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

func fileProgressLine(file transfer.FileProgress, width int, good, muted lipgloss.Style) string {
	label := fileLabel(file.Path, max(1, min(36, width/2)))
	barWidth := max(1, width-ansi.StringWidth(label)-8)
	fraction := 0.0
	if file.TotalBytes > 0 {
		fraction = min(1.0, float64(file.Bytes)/float64(file.TotalBytes))
	} else {
		// A zero-length file is already complete; it must not look stalled.
		fraction = 1
	}
	filled := int(fraction * float64(barWidth))
	bar := good.Render(strings.Repeat("━", filled)) + muted.Render(strings.Repeat("─", barWidth-filled))
	return fmt.Sprintf("%s %s %3d%%", label, bar, int(fraction*100))
}

func fileLabel(path string, width int) string {
	label := safeText(path)
	if ansi.StringWidth(label) <= width {
		return label
	}
	if slash := strings.LastIndex(label, "/"); slash >= 0 {
		base := label[slash+1:]
		if ansi.StringWidth(base) <= width {
			if ansi.StringWidth(base)+2 <= width {
				return "…/" + base
			}
			return base
		}
		return ansi.Truncate(base, width, "…")
	}
	return ansi.Truncate(label, width, "…")
}
func dashboardMessage(s transfer.Snapshot, cancelling bool) string {
	if cancelling {
		return "Stopping downloads safely"
	}
	if s.Listing {
		if s.Active > 0 {
			return "Listing and downloading"
		}
		return "Listing remote files"
	}
	for _, file := range s.ActiveFiles {
		if file.ResumeOffset > 0 {
			return "Resuming downloads · Lazarus, come forth"
		}
	}
	if s.Active > 0 {
		if s.Failed > 0 {
			return "Downloading · some transfers failed"
		}
		return "Downloading · Let there be files"
	}
	return "Waiting for transfers to finish"
}

func rate(s transfer.Snapshot) string {
	if s.Elapsed <= 0 {
		return "— blessings/s"
	}
	return formatBytes(int64(float64(s.Bytes)/s.Elapsed.Seconds())) + "/s"
}

func formatBytes(n int64) string {
	const unit = 1024
	if n == 1 {
		return "1 blessing"
	}
	if n < unit {
		return fmt.Sprintf("%d blessings", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	names := []string{"Kbl", "Mbl", "Gbl", "Tbl"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), names[exp])
}

func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

func printSummary(w io.Writer, s transfer.Snapshot, err error, dry bool) {
	count, amount := s.Completed, s.Bytes
	status, fileLabel, measure := "Complete · It is finished", "files downloaded", "received"
	if dry {
		count, amount, status, fileLabel, measure = s.Discovered, s.TotalBytes, "Dry run complete", "files selected", "total"
	}
	switch {
	case errors.Is(err, context.Canceled):
		status = "Cancelled"
	case err != nil || s.Failed > 0:
		status = "Finished with errors"
	}
	if count == 1 {
		fileLabel = "file" + strings.TrimPrefix(fileLabel, "files")
	}
	fmt.Fprintf(w, "%s: %d %s, %s %s in %s", status, count, fileLabel, formatBytes(amount), measure, s.Elapsed.Round(time.Millisecond))
	if dry {
		fmt.Fprint(w, "; dry run, no files downloaded")
	}
	if s.Skipped > 0 {
		label := "entries"
		if s.Skipped == 1 {
			label = "entry"
		}
		fmt.Fprintf(w, "; %d %s skipped", s.Skipped, label)
	}
	if s.Failed > 0 {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(w, "; %d failed or stopped", s.Failed)
		} else {
			fmt.Fprintf(w, "; %d failed", s.Failed)
		}
	}
	if s.Resumed > 0 {
		fmt.Fprintf(w, "; %d resumed", s.Resumed)
	}
	fmt.Fprintln(w)
	if err != nil {
		fmt.Fprintf(w, "jesusftp: %s\n", safeText(err.Error()))
	}
}
