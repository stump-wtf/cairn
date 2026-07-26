package cliclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// FileProgress is one point-in-time observation of a bundle member's local
// read progress, delivered as PrepareBundleFilesConcurrent's bounded worker
// pool reads files (SPEC-0008 "Concurrency Safety": "a configurable
// concurrency limit with a sane default"). Read == Total (with Done set)
// means the file is fully buffered and ready to send; Err is set when
// reading that file failed — the caller MUST abort the whole bundle rather
// than send a partial one (SPEC-0008 "Bundle Creation": "a failure of any
// constituent file MUST abort the bundle so no partial bundle is shared").
type FileProgress struct {
	Index int
	Path  string
	Read  int64
	Total int64
	Done  bool
	Err   error
}

// ProgressFunc receives FileProgress observations from
// PrepareBundleFilesConcurrent. It may be invoked concurrently from any of
// the pool's worker goroutines — it carries no synchronization of its own.
// A caller that needs a single-threaded view (e.g. a Bubble Tea program)
// MUST hand updates off through a channel itself; tea.Program.Send is safe
// to call from any goroutine and is the intended receiver.
type ProgressFunc func(FileProgress)

// dedupPaths drops later paths that name a file an earlier path already
// named, preserving first-seen order and the caller's original (possibly
// relative) string — the same de-duplication contract as OpenBundleFiles
// (SPEC-0008 "Oversize and Duplicate File Handling": "cairn add a.log a.log
// ... MUST NOT upload a.log twice"). Identity, not the path string, is what
// makes two arguments duplicates; see seenFiles.
func dedupPaths(paths []string) ([]string, error) {
	var seen seenFiles
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		fresh, err := seen.add(p)
		if err != nil {
			return nil, err
		}
		if !fresh {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ValidateBundlePaths de-duplicates paths (see dedupPaths) and stats each
// survivor, rejecting a missing path or a directory, WITHOUT opening a
// lingering file handle — the concurrent prepare phase
// (PrepareBundleFilesConcurrent) opens and reads each file itself. Callers
// use this for fast, local, pre-auth validation (SPEC-0008 "Oversize and
// Duplicate File Handling", "Command Surface": usage errors are checked
// before any network call) and pass the returned, deduplicated list on to
// PrepareBundleFilesConcurrent.
func ValidateBundlePaths(paths []string) ([]string, error) {
	deduped, err := dedupPaths(paths)
	if err != nil {
		return nil, err
	}
	for _, p := range deduped {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory; pass files, not directories", p)
		}
	}
	return deduped, nil
}

// PrepareBundleFilesConcurrent reads each de-duplicated path fully into
// memory using a bounded worker pool of size concurrency (SPEC-0008 "cairn
// add MUST upload multiple files using a bounded worker pool"; a
// concurrency < 1 is treated as 1). Local disk reads — not the eventual
// network write — are where wall-clock concurrency actually helps: the
// bundle itself is always sent as one atomic multipart POST (cli/design.md
// "concurrent, bounded, atomic bundle upload": the whole point is that the
// server confirms the *complete* set before the CLI ever reports success),
// so per-file network concurrency isn't meaningful against a single-request
// API. This bounds and reports the read phase exactly like an upload would
// be reported, and CreateBundle then streams the now-buffered files into the
// one request in order.
//
// ctx cancellation (SIGINT, SPEC-0008 "Ctrl-C mid-bundle") aborts in-flight
// reads promptly: no more workers are dispatched, running reads stop at
// their next chunk boundary, and the function returns ctx.Err() with no
// partial result.
func PrepareBundleFilesConcurrent(ctx context.Context, paths []string, concurrency int, progress ProgressFunc) ([]BundleFile, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	deduped, err := dedupPaths(paths)
	if err != nil {
		return nil, err
	}

	type result struct {
		file BundleFile
		err  error
	}

	results := make([]result, len(deduped))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, p := range deduped {
		select {
		case <-ctx.Done():
			results[i] = result{err: ctx.Err()}
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			f, err := readFileWithProgress(ctx, i, p, progress)
			results[i] = result{file: f, err: err}
		}(i, p)
	}
	wg.Wait()

	files := make([]BundleFile, 0, len(deduped))
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		files = append(files, r.file)
	}
	return files, nil
}

// readFileWithProgress buffers path fully into memory in fixed-size chunks,
// reporting a FileProgress after every chunk so a caller can render a
// smoothly-advancing per-file progress bar (SPEC-0008 "per-file progress
// rows"). It checks ctx between chunks so a cancellation lands within one
// chunk's latency rather than waiting for the whole file.
func readFileWithProgress(ctx context.Context, index int, path string, progress ProgressFunc) (BundleFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return BundleFile{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return BundleFile{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return BundleFile{}, fmt.Errorf("%s is a directory; pass files, not directories", path)
	}
	total := info.Size()

	buf := bytes.NewBuffer(make([]byte, 0, total))
	chunk := make([]byte, 32*1024)
	var read int64
	for {
		select {
		case <-ctx.Done():
			return BundleFile{}, fmt.Errorf("read %s: %w", path, ctx.Err())
		default:
		}
		n, rerr := f.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			read += int64(n)
			if progress != nil {
				progress(FileProgress{Index: index, Path: path, Read: read, Total: total})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return BundleFile{}, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	if progress != nil {
		progress(FileProgress{Index: index, Path: path, Read: read, Total: total, Done: true})
	}
	return BundleFile{Path: path, Body: bytes.NewReader(buf.Bytes()), Size: read}, nil
}
