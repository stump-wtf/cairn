package cliclient

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// seenFiles de-duplicates bundle members by the file each path names, not by
// how the caller happened to spell it.
//
// SPEC-0008 "Oversize and Duplicate File Handling" says the CLI "MUST detect
// the duplicate argument and MUST NOT upload it twice". That requirement is
// about the file; the path is only how the user pointed at it. Two different
// strings routinely name one file:
//
//   - a symlinked directory — `cairn add logs/a.log /srv/current/logs/a.log`
//     where /srv/current -> /srv/releases/2026-07-26;
//   - a relative argument resolved against a physical working directory, when
//     the path the user typed goes through a symlink (this is why macOS, whose
//     /var is a symlink to /private/var, saw the bug and Linux CI did not);
//   - a hard link, which is the same file under two names by construction.
//
// Comparison is therefore os.SameFile — device plus inode — over os.Stat
// results, which needs no string canonicalisation and is symlink- and
// hardlink-correct. os.Stat follows symlinks, so identity is always the
// target's.
//
// Governing: SPEC-0008 REQ "Oversize and Duplicate File Handling"
type seenFiles struct {
	// infos holds one stat per distinct file admitted so far. os.SameFile is
	// pairwise, so this is a linear scan rather than a keyed lookup: a
	// device/inode key would mean reaching into info.Sys(), which is not
	// portable. Bundles are command-line sized, so the scan is not worth
	// trading portability for.
	infos []fs.FileInfo
	// paths is the fallback for arguments that cannot be stat'ed at all,
	// keyed on the absolute path — the old, string-based behaviour, kept only
	// where identity is genuinely unavailable.
	paths map[string]bool
}

// add reports whether p names a file not already admitted, so callers can
// `continue` on false.
//
// A path that cannot be stat'ed falls back to absolute-string matching rather
// than failing here: the caller's own os.Open or os.Stat runs immediately
// after and already reports the real problem — missing file, permission
// denied, a directory — with the message and wrapping it has always used.
// Reporting it here instead would change those errors for no gain.
func (s *seenFiles) add(p string) (bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		abs, absErr := filepath.Abs(p)
		if absErr != nil {
			return false, fmt.Errorf("resolve %s: %w", p, absErr)
		}
		if s.paths[abs] {
			return false, nil
		}
		if s.paths == nil {
			s.paths = make(map[string]bool)
		}
		s.paths[abs] = true
		return true, nil
	}

	for _, prev := range s.infos {
		if os.SameFile(prev, info) {
			return false, nil // same file, differently spelled — not uploaded twice
		}
	}
	s.infos = append(s.infos, info)
	return true, nil
}
