package cliclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func writeUploadTestFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestPrepareBundleFilesConcurrentReadsAllFiles(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		paths = append(paths, writeUploadTestFile(t, dir, fmt.Sprintf("f%d.log", i), 4096*(i+1)))
	}

	var progressCount atomic.Int64
	files, err := PrepareBundleFilesConcurrent(context.Background(), paths, 2, func(fp FileProgress) {
		progressCount.Add(1)
	})
	if err != nil {
		t.Fatalf("PrepareBundleFilesConcurrent: %v", err)
	}
	if len(files) != len(paths) {
		t.Fatalf("got %d files, want %d", len(files), len(paths))
	}
	if progressCount.Load() == 0 {
		t.Error("expected at least one progress observation")
	}
	for i, f := range files {
		if f.Path != paths[i] {
			t.Errorf("files[%d].Path = %q, want %q (order preserved)", i, f.Path, paths[i])
		}
		if f.Size != int64(4096*(i+1)) {
			t.Errorf("files[%d].Size = %d, want %d", i, f.Size, 4096*(i+1))
		}
	}
}

func TestPrepareBundleFilesConcurrentDeduplicates(t *testing.T) {
	dir := t.TempDir()
	a := writeUploadTestFile(t, dir, "a.log", 128)

	files, err := PrepareBundleFilesConcurrent(context.Background(), []string{a, a}, 4, nil)
	if err != nil {
		t.Fatalf("PrepareBundleFilesConcurrent: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1 (deduplicated)", len(files))
	}
}

// TestPrepareBundleFilesConcurrentDeduplicatesThroughSymlinkedDir covers the
// path `cairn add` actually runs (ValidateBundlePaths →
// PrepareBundleFilesConcurrent, both via dedupPaths) for the same
// identity-not-string de-duplication contract asserted in
// TestOpenBundleFilesDeduplicatesThroughSymlinkedDir.
func TestPrepareBundleFilesConcurrentDeduplicatesThroughSymlinkedDir(t *testing.T) {
	real, viaLink := symlinkedDuplicate(t)

	files, err := PrepareBundleFilesConcurrent(context.Background(), []string{real, viaLink}, 4, nil)
	if err != nil {
		t.Fatalf("PrepareBundleFilesConcurrent: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1 (same file via a symlinked dir)", len(files))
	}

	deduped, err := ValidateBundlePaths([]string{real, viaLink})
	if err != nil {
		t.Fatalf("ValidateBundlePaths: %v", err)
	}
	if len(deduped) != 1 {
		t.Fatalf("ValidateBundlePaths returned %d paths, want 1", len(deduped))
	}
}

func TestPrepareBundleFilesConcurrentMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := PrepareBundleFilesConcurrent(context.Background(), []string{filepath.Join(dir, "nope.log")}, 2, nil)
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestPrepareBundleFilesConcurrentDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := PrepareBundleFilesConcurrent(context.Background(), []string{dir}, 2, nil)
	if err == nil {
		t.Fatal("want an error for a directory path")
	}
}

// TestPrepareBundleFilesConcurrentBounded verifies no more than the
// configured concurrency limit of files are being read at once (SPEC-0008
// "Bounded concurrency": "no more than the configured number of uploads
// MUST be in flight at once"). It uses real files large enough that reads
// take multiple chunks, and tracks the high-water mark of simultaneously
// in-progress (started, not yet done) files via the progress callback.
func TestPrepareBundleFilesConcurrentBounded(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	const concurrency = 2
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// Large enough (relative to the 32KiB read chunk) that several
		// progress callbacks fire per file, widening the window in which
		// an over-bounded implementation would be caught.
		paths = append(paths, writeUploadTestFile(t, dir, fmt.Sprintf("big%d.bin", i), 256*1024))
	}

	var mu sync.Mutex
	inFlight := map[int]bool{}
	var current, maxSeen int

	_, err := PrepareBundleFilesConcurrent(context.Background(), paths, concurrency, func(fp FileProgress) {
		mu.Lock()
		defer mu.Unlock()
		if fp.Done {
			if inFlight[fp.Index] {
				delete(inFlight, fp.Index)
				current--
			}
			return
		}
		if !inFlight[fp.Index] {
			inFlight[fp.Index] = true
			current++
			if current > maxSeen {
				maxSeen = current
			}
		}
	})
	if err != nil {
		t.Fatalf("PrepareBundleFilesConcurrent: %v", err)
	}
	if maxSeen > concurrency {
		t.Errorf("max concurrent in-flight files = %d, want <= %d", maxSeen, concurrency)
	}
}

func TestPrepareBundleFilesConcurrentCancelAborts(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		paths = append(paths, writeUploadTestFile(t, dir, fmt.Sprintf("c%d.log", i), 64*1024))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the pool starts

	_, err := PrepareBundleFilesConcurrent(ctx, paths, 1, nil)
	if err == nil {
		t.Fatal("want an error when ctx is already cancelled")
	}
}

func TestPrepareBundleFilesConcurrentValidateBundlePaths(t *testing.T) {
	dir := t.TempDir()
	a := writeUploadTestFile(t, dir, "a.log", 8)
	b := writeUploadTestFile(t, dir, "b.log", 8)

	deduped, err := ValidateBundlePaths([]string{a, b, a})
	if err != nil {
		t.Fatalf("ValidateBundlePaths: %v", err)
	}
	if len(deduped) != 2 {
		t.Fatalf("got %d paths, want 2 (deduplicated)", len(deduped))
	}

	if _, err := ValidateBundlePaths([]string{filepath.Join(dir, "missing.log")}); err == nil {
		t.Error("want error for missing path")
	}
	if _, err := ValidateBundlePaths([]string{dir}); err == nil {
		t.Error("want error for directory path")
	}
}
