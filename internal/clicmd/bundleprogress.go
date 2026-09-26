package clicmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// defaultUploadConcurrency is `cairn add`'s bounded worker pool size when
// --concurrency is not set (SPEC-0008 "a configurable concurrency limit
// with a sane default").
const defaultUploadConcurrency = 4

var (
	doneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	queuedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // dim gray
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	progressBarW = 24
)

// fileRow is one line of cairn add's per-file progress display (SPEC-0008
// "per-file progress rows (✓ done / progress bar in-flight / · queued)").
type fileRow struct {
	path  string
	total int64
	read  int64
	done  bool
	err   error
}

// bundleProgressMsg funnels a cliclient.FileProgress observation — reported
// by the bounded worker pool, possibly from several goroutines at once —
// into the single-threaded Bubble Tea event loop. tea.Program.Send is safe
// to call concurrently, so this is the one place cliclient.FileProgress
// crosses from "many goroutines" to "one owner" (SPEC-0008 "Concurrency
// Safety": "shared state ... MUST be updated race-free").
type bundleProgressMsg cliclient.FileProgress

// bundleResultMsg carries the final CreateBundle outcome (or a prepare-phase
// error) into the event loop and ends the program.
type bundleResultMsg struct {
	art *cliclient.Artifact
	err error
}

// bundleInterruptMsg is sent when the user presses Ctrl-C inside the TUI;
// unlike a plain quit it distinguishes "the human cancelled" from "the
// upload finished" so the caller can map it to exit code 130 (SPEC-0008
// "Ctrl-C mid-bundle") even when raw terminal mode ate the OS-level SIGINT
// before signal.NotifyContext ever saw it.
type bundleInterruptMsg struct{}

// bundleModel is the Bubble Tea model driving `cairn add`'s interactive
// progress display. It is a pure function of (model, msg) -> (model, cmd)
// per the Bubble Tea contract, which makes its state machine directly
// testable without a real terminal (SPEC-0008 acceptance: "progress state
// machine" test).
type bundleModel struct {
	rows      []fileRow
	bar       progress.Model
	cancel    func()
	result    *bundleResultMsg
	interrupt bool
}

// newBundleModel seeds one queued row per path, in the order they'll be
// uploaded. cancel is invoked on Ctrl-C so the shared upload context is
// cancelled even if the terminal's raw mode suppressed the OS SIGINT
// signal.NotifyContext would otherwise have caught.
func newBundleModel(paths []string, cancel func()) bundleModel {
	rows := make([]fileRow, len(paths))
	for i, p := range paths {
		rows[i] = fileRow{path: p}
	}
	return bundleModel{
		rows:   rows,
		bar:    progress.New(progress.WithDefaultGradient(), progress.WithWidth(progressBarW)),
		cancel: cancel,
	}
}

func (m bundleModel) Init() tea.Cmd { return nil }

func (m bundleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case bundleProgressMsg:
		if msg.Index >= 0 && msg.Index < len(m.rows) {
			m.rows[msg.Index] = fileRow{path: msg.Path, total: msg.Total, read: msg.Read, done: msg.Done, err: msg.Err}
		}
		return m, nil
	case bundleResultMsg:
		res := msg
		m.result = &res
		return m, tea.Quit
	case bundleInterruptMsg:
		m.interrupt = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m.Update(bundleInterruptMsg{})
		}
	}
	return m, nil
}

func (m bundleModel) View() string {
	var b strings.Builder
	for _, r := range m.rows {
		b.WriteString(renderFileRow(m.bar, r))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderFileRow renders one row in whichever of the three states SPEC-0008
// names: done ("✓"), in-flight (a Lip Gloss/Bubbles progress bar), or queued
// ("·"). A per-file error renders inline so `cairn add` can show which
// member failed before the bundle aborts.
func renderFileRow(bar progress.Model, r fileRow) string {
	name := filepath.Base(r.path)
	switch {
	case r.err != nil:
		return errStyle.Render(fmt.Sprintf("✗ %s  %v", name, r.err))
	case r.done:
		return doneStyle.Render(fmt.Sprintf("✓ %s", name))
	case r.total > 0 && r.read > 0:
		pct := float64(r.read) / float64(r.total)
		if pct > 1 {
			pct = 1
		}
		return fmt.Sprintf("%s %s", bar.ViewAs(pct), name)
	default:
		return queuedStyle.Render(fmt.Sprintf("· %s (queued)", name))
	}
}
