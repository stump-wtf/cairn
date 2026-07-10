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
	"strings"
	"sync"
)

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
}

// Memory is an in-memory ObjectStore for tests. It is safe for concurrent use
// and honors context cancellation so cancel-path tests are meaningful.
type Memory struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMemory returns an empty in-memory object store.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string][]byte)}
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
	m.objects[key] = buf.Bytes()
	m.mu.Unlock()
	return nil
}

func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	b, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, ErrNotExist)
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return io.NopCloser(bytes.NewReader(cp)), nil
}

func (m *Memory) Copy(ctx context.Context, srcKey, dstKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[srcKey]
	if !ok {
		return fmt.Errorf("objectstore: copy %s: %w", srcKey, ErrNotExist)
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	m.objects[dstKey] = cp
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

// ErrNotExist is returned when reading or copying a missing object.
var ErrNotExist = fmt.Errorf("object does not exist")
