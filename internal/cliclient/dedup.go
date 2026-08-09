package cliclient

import (
	"fmt"
	"os"
	"path/filepath"
)

// pathDeduper drops repeated arguments that name the same local file
// (SPEC-0008 "Oversize and Duplicate File Handling": "cairn add a.log a.log
// ... MUST NOT upload a.log twice").
//
// Identity is os.SameFile — device plus inode — not the absolute path
// string, because two different strings routinely name one file. A symlinked
// parent directory is the common case: on macOS os.Getwd() reports the
// resolved /private/var/... form of a /var/... temp directory, so
// `cairn add a.log /var/.../a.log` compares two spellings of the same file
// and a string match sees no duplicate. Users hit the same thing through any
// symlinked path they pass by hand. Comparing file identity also makes
// hardlinked duplicates collapse, which is the behavior a user asking "don't
// upload it twice" expects.
//
// A path that cannot be stat'ed has no identity to compare, so it falls back
// to absolute-path string matching: the caller's own open/stat then reports
// the real error (missing file, permission denied) with its usual message
// rather than this de-duplication pass pre-empting it.
type pathDeduper struct {
	infos []os.FileInfo
	abs   map[string]bool
}

// seen reports whether p names a file an earlier call already accepted,
// recording p's identity when it does not. It errors only when the path
// cannot be made absolute at all.
func (d *pathDeduper) seen(p string) (bool, error) {
	info, statErr := os.Stat(p)
	if statErr == nil {
		for _, prev := range d.infos {
			if os.SameFile(prev, info) {
				return true, nil
			}
		}
		d.infos = append(d.infos, info)
		return false, nil
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", p, err)
	}
	if d.abs == nil {
		d.abs = make(map[string]bool)
	}
	if d.abs[abs] {
		return true, nil
	}
	d.abs[abs] = true
	return false, nil
}
