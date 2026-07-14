// Package objectstore abstracts the S3-compatible byte bucket that holds raw
// artifact bodies, content-addressed by SHA-256 (ADR-0008). No metadata is
// authoritative here; Postgres is the source of truth. The interface is small
// and streaming so tens-of-MB bodies move without buffering, and an in-memory
// implementation backs unit tests.
//
// Governing: ADR-0008 (Storage & Content Model),
// SPEC-0002 REQ "Content Addressing and Blobs"
package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// ObjectInfo is one enumerated object: its key and last-modified time. The
// SPEC-0009 orphan-object scan uses LastModified as a defense-in-depth grace
// window so it never races a just-uploaded body of an in-flight create.
type ObjectInfo struct {
	Key          string
	LastModified time.Time
}

// ObjectStore is a content-addressed byte bucket. Every method takes a context
// so a cancelled request releases in-flight work (SPEC-0002 "Concurrency").
type ObjectStore interface {
	// Put streams r to key. size is a hint (-1 when unknown, e.g. streamed
	// ingest). contentType is advisory metadata on the object.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens key for reading; the caller must Close the returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Copy server-side-copies srcKey to dstKey.
	Copy(ctx context.Context, srcKey, dstKey string) error
	// Remove deletes key. Removing a missing key is not an error.
	Remove(ctx context.Context, key string) error
	// Stat reports whether key exists.
	Stat(ctx context.Context, key string) (bool, error)
	// List enumerates every object whose key begins with prefix, in ascending
	// key order. It powers the SPEC-0009 object-storage orphan-scan backstop
	// (list blobs/, delete those with no live DB reference). The full listing is
	// materialized; callers bound their own work per cycle.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// Memory is an in-memory ObjectStore for tests. It is safe for concurrent use
// and honors context cancellation so cancel-path tests are meaningful.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject
	// now overrides the modification clock so a test can age an object into the
	// orphan-scan grace window without sleeping. Nil means time.Now.
	now func() time.Time
}

type memObject struct {
	data     []byte
	modified time.Time
}

// NewMemory returns an empty in-memory object store.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string]memObject)}
}

// SetClock overrides the modification clock (test helper) so orphan-scan
// grace-window tests can backdate an object's LastModified deterministically.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	m.now = now
	m.mu.Unlock()
}

func (m *Memory) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Memory) Put(ctx context.Context, key string, r io.Reader, _ int64, _ string) error {
	// Read incrementally so a cancelled context aborts the copy rather than
	// buffering the whole (potentially huge) body first.
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("objectstore: put %s: %w", key, err)
		}
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("objectstore: put %s: %w", key, err)
		}
	}
	m.mu.Lock()
	m.objects[key] = memObject{data: buf.Bytes(), modified: m.clock()}
	m.mu.Unlock()
	return nil
}

func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	o, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, ErrNotExist)
	}
	cp := make([]byte, len(o.data))
	copy(cp, o.data)
	return io.NopCloser(bytes.NewReader(cp)), nil
}

func (m *Memory) Copy(ctx context.Context, srcKey, dstKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[srcKey]
	if !ok {
		return fmt.Errorf("objectstore: copy %s: %w", srcKey, ErrNotExist)
	}
	cp := make([]byte, len(o.data))
	copy(cp, o.data)
	m.objects[dstKey] = memObject{data: cp, modified: m.clock()}
	return nil
}

func (m *Memory) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objects, key)
	m.mu.Unlock()
	return nil
}

func (m *Memory) Stat(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.RLock()
	_, ok := m.objects[key]
	m.mu.RUnlock()
	return ok, nil
}

// Len reports the number of stored objects (test helper).
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

// KeysWithPrefix returns the stored keys that begin with prefix (test helper).
// It lets ingest tests assert that no orphan staging/<rand> object survives a
// successful create — the leak SPEC-0002 content-addressing forbids.
func (m *Memory) KeysWithPrefix(prefix string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// List enumerates objects under prefix in ascending key order (ObjectStore).
func (m *Memory) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	out := make([]ObjectInfo, 0)
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, ObjectInfo{Key: k, LastModified: o.modified})
		}
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ErrNotExist is returned when reading or copying a missing object.
var ErrNotExist = fmt.Errorf("object does not exist")
