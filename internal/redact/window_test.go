package redact

// Windowed Scan Tests
//
// SPEC-0017 RD-7: fields over 1 MiB are scanned from staging in overlapping
// windows, a token across a window boundary is detected exactly once, and the
// cap is enforced even when the stream is longer than its declared size. Also
// the benchmark the issue asks for: MB/s at 1 MiB and 16 MiB.
//
// Governing: ADR-0023, SPEC-0017 RD-7, REQ "Concurrency Safety"
//
// @joestump 09/25/2026 - Added for cairn#289.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/objectstore"
)

// filler is inert log text: no rule fires on it.
func filler(n int) string {
	const line = "2026-09-25T10:00:00Z info request served path=/v1/artifacts status=200\n"
	return strings.Repeat(line, n/len(line)+1)[:n]
}

func stage(t testing.TB, obj *objectstore.Memory, body string) string {
	t.Helper()
	key := "staging/test-" + fmt.Sprint(len(body))
	if err := obj.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatal(err)
	}
	return key
}

func readObject(t testing.TB, obj *objectstore.Memory, key string) string {
	t.Helper()
	rc, err := obj.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWindowStraddleCountedOnce sweeps a token across every kind of window
// boundary with a small window: the start of the next window, the ownership
// hand-off in the middle of the overlap, and the end of the current window.
// Each placement is detected exactly once, masked, and located correctly.
func TestWindowStraddleCountedOnce(t *testing.T) {
	s := newTestScanner(t)
	s.window, s.overlap = 8<<10, 4<<10 // RD-7's minimum overlap
	tok := pat(21)
	adv := s.window - s.overlap
	var offsets []int
	for _, edge := range []int{adv, s.window - s.overlap/2, s.window, 2 * adv, 2*adv + s.overlap/2} {
		for _, d := range []int{-len(tok) - 2, -len(tok), -len(tok) + 1, -len(tok) / 2, -2, -1, 0, 1} {
			offsets = append(offsets, edge+d)
		}
	}
	for _, off := range offsets {
		body := filler(off) + " " + tok + " \n" + filler(s.window)
		for _, mode := range []Mode{ModeMask, ModeReject} {
			got, o, err := s.Text(context.Background(), "body", body, mode)
			if mode == ModeReject {
				var rej *Rejection
				if !errors.As(err, &rej) || len(rej.Findings) != 1 {
					t.Fatalf("offset %d reject: err = %v, want exactly one finding", off, err)
				}
				wantLine := strings.Count(body[:off+1], "\n") + 1
				if rej.Findings[0].Line != wantLine {
					t.Errorf("offset %d: line %d, want %d", off, rej.Findings[0].Line, wantLine)
				}
				continue
			}
			if err != nil || o.Count != 1 || o.Rules["github-pat"] != 1 {
				t.Fatalf("offset %d: %+v, %v; want github-pat exactly once", off, o, err)
			}
			if strings.Contains(got, tok) || got != strings.Replace(body, tok, Mask, 1) {
				t.Fatalf("offset %d: masked body differs from the expected single replacement", off)
			}
		}
	}
}

// TestStagedStraddleProductionWindow: the same property at the production
// window and overlap, through the object store, for a body over 1 MiB.
func TestStagedStraddleProductionWindow(t *testing.T) {
	s := newTestScanner(t)
	tok := pat(22)
	adv := s.window - s.overlap
	for _, off := range []int{adv - 10, s.window - s.overlap/2 - 10, s.window - 10} {
		obj := objectstore.NewMemory()
		body := filler(off) + tok + "\n" + filler(s.overlap)
		key := stage(t, obj, body)
		dst, o, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeMask)
		if err != nil || o.Status != StatusMasked || o.Count != 1 {
			t.Fatalf("offset %d: %q, %+v, %v", off, dst, o, err)
		}
		if dst == "" || dst == key || !strings.HasPrefix(dst, "staging/") {
			t.Fatalf("masked key = %q, want a new staging key", dst)
		}
		if got := readObject(t, obj, dst); got != strings.Replace(body, tok, Mask, 1) {
			t.Errorf("offset %d: masked copy is wrong (len %d)", off, len(got))
		}
		if got := readObject(t, obj, key); got != body {
			t.Error("the original staging object was modified")
		}
	}
}

// TestStagedClean: a clean body writes nothing.
func TestStagedClean(t *testing.T) {
	s := newTestScanner(t)
	obj := objectstore.NewMemory()
	body := filler(1<<20 + 1<<18)
	key := stage(t, obj, body)
	dst, o, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeMask)
	if err != nil || dst != "" || o.Status != StatusClean || obj.Len() != 1 {
		t.Errorf("clean: %q, %+v, %v, %d objects", dst, o, err, obj.Len())
	}
}

// TestStagedReject: reject mode writes nothing, and the rejection names the
// rule and location but not the value.
func TestStagedReject(t *testing.T) {
	s := newTestScanner(t)
	obj := objectstore.NewMemory()
	tok := pat(23)
	body := filler(1<<20+1<<18) + "key " + tok + "\n"
	key := stage(t, obj, body)
	dst, _, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeReject)
	var rej *Rejection
	if !errors.As(err, &rej) || dst != "" || obj.Len() != 1 {
		t.Fatalf("reject: %q, %v, %d objects", dst, err, obj.Len())
	}
	if errs.CodeOf(err) != errs.CodeValidation || strings.Contains(err.Error(), tok) {
		t.Errorf("err = %q", err)
	}
	if rej.Findings[0].Line != strings.Count(body, "\n") {
		t.Errorf("line = %d, want %d", rej.Findings[0].Line, strings.Count(body, "\n"))
	}
}

// TestStagedOversize: over the cap by declared size, or by a stream longer
// than declared, is too_large_to_scan; store_unscanned stores it as is.
func TestStagedOversize(t *testing.T) {
	obj := objectstore.NewMemory()
	body := filler(64 << 10)
	key := stage(t, obj, body)
	s, err := New(Config{MaxScanBytes: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, declared := range []int64{int64(len(body)), 100} {
		_, _, err := s.Staged(context.Background(), obj, key, declared, ModeMask)
		if !errors.Is(err, ErrTooLargeToScan) || errs.CodeOf(err) != errs.CodeValidation {
			t.Errorf("declared %d: err = %v, want too_large_to_scan", declared, err)
		}
	}
	s, err = New(Config{MaxScanBytes: 32 << 10, Oversize: OversizeStoreUnscanned})
	if err != nil {
		t.Fatal(err)
	}
	dst, o, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeMask)
	if err != nil || dst != "" || o.Status != StatusNotScannedOversize {
		t.Errorf("store_unscanned: %q, %+v, %v", dst, o, err)
	}
}

// TestStagedCancelled: a cancelled context fails the scan closed.
func TestStagedCancelled(t *testing.T) {
	s := newTestScanner(t)
	obj := objectstore.NewMemory()
	body := filler(1<<20 + 1<<18)
	key := stage(t, obj, body)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.Staged(ctx, obj, key, int64(len(body)), ModeMask)
	if !errors.Is(err, ErrScanFailed) || errs.CodeOf(err) != errs.CodeInternal {
		t.Errorf("err = %v, want an internal failure", err)
	}
}

// TestWindowsStreamWholeInput: the windows cover every byte exactly once in
// their owned ranges, for sizes around the window arithmetic.
func TestWindowsStreamWholeInput(t *testing.T) {
	s := newTestScanner(t)
	s.window, s.overlap = 64, 16
	for n := 0; n < 300; n++ {
		in := strings.Repeat("x", n)
		var owned int
		var last int64 = 0
		err := s.windows(context.Background(), source{r: strings.NewReader(in)}, func(w window) error {
			if w.base+int64(w.lo) != last {
				return fmt.Errorf("gap or overlap at %d", w.base+int64(w.lo))
			}
			owned += w.hi - w.lo
			last = w.base + int64(w.hi)
			return nil
		})
		if err != nil || owned != n {
			t.Fatalf("n=%d: owned %d, err %v", n, owned, err)
		}
	}
}

func benchmarkText(b *testing.B, size int) {
	s := newTestScanner(b)
	s.maxBytes = int64(size) + 1
	body := filler(size - 64)
	body += "\nAuthorization: token " + strings.Repeat("0123456789abcdef", 2) + "01234567\n"
	body += strings.Repeat(" ", size-len(body))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, o, err := s.Text(context.Background(), "body", body, ModeMask); err != nil || o.Count != 1 {
			b.Fatalf("%+v, %v", o, err)
		}
	}
}

// BenchmarkText1MiB and BenchmarkText16MiB record MB/s of a mask-mode scan
// with one finding (so the masking and re-scan pass are included).
func BenchmarkText1MiB(b *testing.B)  { benchmarkText(b, 1<<20) }
func BenchmarkText16MiB(b *testing.B) { benchmarkText(b, 16<<20) }

// BenchmarkStaged16MiB is the same through the object store read-back.
func BenchmarkStaged16MiB(b *testing.B) {
	s := newTestScanner(b)
	obj := objectstore.NewMemory()
	body := filler(16 << 20)
	key := stage(b, obj, body)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, o, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeMask); err != nil || o.Status != StatusClean {
			b.Fatalf("%+v, %v", o, err)
		}
	}
}

// TestStagedMaskGrowthAtCap: a body at the cap whose values are shorter than
// Mask grows when masked. The re-scan of that masked copy is Cairn's own
// output, not an upload, so it must not trip the upload cap and turn a
// successful mask into an internal failure.
//
// @joestump 09/26/2026 - Added in review of cairn#382: the re-scan read the
// masked copy through the same capReader as the upload, so a body within
// len(Mask) bytes of the cap failed with ErrScanFailed.
func TestStagedMaskGrowthAtCap(t *testing.T) {
	obj := objectstore.NewMemory()
	secret := "passw" + "ord=" + "hunt" + "er2"
	body := secret + "\n"
	body += filler(64<<10 - len(body))
	key := stage(t, obj, body)
	s, err := New(Config{MaxScanBytes: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	s.window, s.overlap = 8<<10, 4<<10
	dst, o, err := s.Staged(context.Background(), obj, key, int64(len(body)), ModeMask)
	if err != nil || o.Status != StatusMasked {
		t.Fatalf("Staged at the cap = %q, %+v, %v; want masked", dst, o, err)
	}
	got := readObject(t, obj, dst)
	if want := strings.Replace(body, "hunt"+"er2", Mask, 1); got != want {
		t.Errorf("masked copy is wrong (len %d, want %d)", len(got), len(want))
	}
	if len(got) <= len(body) {
		t.Fatalf("masked copy did not grow (%d <= %d): the test no longer covers the cap", len(got), len(body))
	}
}
