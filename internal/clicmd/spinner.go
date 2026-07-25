package clicmd

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
)

// spinnerFrames is a small braille animation, including the glyph SPEC-0008
// names in its example ("⠿ uploading 4.2 KB…").
var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏', '⠿'}

// countingReader wraps an io.Reader, tracking bytes read through it with an
// atomic counter a concurrently-running spinner goroutine can poll without a
// data race (SPEC-0008 "Concurrency Safety": shared state must be updated
// race-free).
type countingReader struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.read.Add(int64(n))
	}
	return n, err
}

// spinner renders a single-line "⠿ uploading 4.2 KB…" progress indicator on
// out via carriage-return redraws (SPEC-0008 "Pipe and Path Ingest": the
// stdin-push spinner). Callers MUST only construct one when out is a TTY —
// spinner itself does not check, since the caller already knows (SPEC-0008
// "the CLI MUST NOT emit terminal control sequences to a non-TTY stream").
type spinner struct {
	out    io.Writer
	label  string
	reader *countingReader
	stop   chan struct{}
	done   chan struct{}
}

// newSpinner starts animating immediately in a background goroutine. label
// is a static prefix ("uploading"); the byte count comes from reader, which
// the caller streams the request body through.
func newSpinner(out io.Writer, label string, reader *countingReader) *spinner {
	s := &spinner{out: out, label: label, reader: reader, stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *spinner) run() {
	defer close(s.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			n := int64(0)
			if s.reader != nil {
				n = s.reader.read.Load()
			}
			fmt.Fprintf(s.out, "\r%c %s %s…\033[K", spinnerFrames[frame%len(spinnerFrames)], s.label, humanize.Bytes(uint64(n)))
			frame++
		}
	}
}

// Stop halts the animation and clears the spinner's line so subsequent
// output (the "✓ pushed" result) starts clean.
func (s *spinner) Stop() {
	close(s.stop)
	<-s.done
	fmt.Fprint(s.out, "\r\033[K")
}
