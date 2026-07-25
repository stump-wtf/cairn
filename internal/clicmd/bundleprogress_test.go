package clicmd

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joestump/cairn/internal/cliclient"
)

// errOversizeForTest is a stand-in error value; the model only needs a
// non-nil error to exercise the error-row rendering path.
var errOversizeForTest = errors.New("payload too large")

// TestBundleModelStateMachine drives bundleModel.Update directly — the
// standard way to test a Bubble Tea model (Update is a pure (model, msg) ->
// (model, cmd) function) — through the queued -> in-flight -> done
// transitions SPEC-0008 names for `cairn add`'s per-file progress rows.
func TestBundleModelStateMachine(t *testing.T) {
	paths := []string{"a.log", "b.sql", "c.png"}
	m := newBundleModel(paths, nil)

	// All three start queued.
	view := m.View()
	for _, want := range []string{"a.log", "b.sql", "c.png"} {
		if !strings.Contains(view, want) {
			t.Errorf("initial view missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "queued") {
		t.Errorf("initial view should show queued rows:\n%s", view)
	}

	// a.log starts reading (in-flight): a partial progress observation.
	newModel, _ := m.Update(bundleProgressMsg(cliclient.FileProgress{Index: 0, Path: paths[0], Read: 50, Total: 100}))
	m = newModel.(bundleModel)
	if m.rows[0].done {
		t.Error("row 0 should not be done yet")
	}
	if m.rows[0].read != 50 || m.rows[0].total != 100 {
		t.Errorf("row 0 = %+v, want read=50 total=100", m.rows[0])
	}
	// b.sql and c.png remain queued/untouched.
	if m.rows[1].read != 0 || m.rows[2].read != 0 {
		t.Errorf("rows 1/2 should be untouched: %+v %+v", m.rows[1], m.rows[2])
	}

	// a.log finishes.
	newModel, _ = m.Update(bundleProgressMsg(cliclient.FileProgress{Index: 0, Path: paths[0], Read: 100, Total: 100, Done: true}))
	m = newModel.(bundleModel)
	if !m.rows[0].done {
		t.Error("row 0 should be done")
	}
	if !strings.Contains(m.View(), "✓") {
		t.Errorf("view should show a done glyph:\n%s", m.View())
	}

	// The bundle POST completes: a bundleResultMsg both records the result
	// and issues tea.Quit.
	art := &cliclient.Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1"}
	newModel, cmd := m.Update(bundleResultMsg{art: art})
	m = newModel.(bundleModel)
	if m.result == nil || m.result.art != art {
		t.Fatalf("result = %+v, want the artifact recorded", m.result)
	}
	if cmd == nil {
		t.Fatal("want a tea.Cmd (tea.Quit) after the result lands")
	}
}

// TestBundleModelPerFileError verifies a failed file renders distinctly and
// is recorded rather than silently dropped (SPEC-0008 "a failure of any
// constituent file MUST abort the bundle").
func TestBundleModelPerFileError(t *testing.T) {
	m := newBundleModel([]string{"big.bin"}, nil)
	newModel, _ := m.Update(bundleProgressMsg(cliclient.FileProgress{Index: 0, Path: "big.bin", Err: errOversizeForTest}))
	m = newModel.(bundleModel)
	if m.rows[0].err == nil {
		t.Fatal("want the error recorded on the row")
	}
	if !strings.Contains(m.View(), "✗") {
		t.Errorf("view should show an error glyph:\n%s", m.View())
	}
}

// TestBundleModelCtrlCCancelsAndQuits verifies Ctrl-C both invokes the
// caller's cancel function and marks the model interrupted, so the caller
// can map it to exit code 130 (SPEC-0008 "Ctrl-C mid-bundle") even when
// terminal raw mode prevents the OS SIGINT signal.NotifyContext relies on.
func TestBundleModelCtrlCCancelsAndQuits(t *testing.T) {
	var cancelled bool
	m := newBundleModel([]string{"a.log", "b.log"}, func() { cancelled = true })

	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = newModel.(bundleModel)

	if !cancelled {
		t.Error("Ctrl-C should invoke the cancel function")
	}
	if !m.interrupt {
		t.Error("Ctrl-C should mark the model interrupted")
	}
	if cmd == nil {
		t.Fatal("want a tea.Cmd (tea.Quit) after Ctrl-C")
	}
	if m.result != nil {
		t.Error("an interrupted model should carry no successful result")
	}
}
