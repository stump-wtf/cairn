package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MemoryStore is an in-process session Store for tests and single-node runs
// without a database. It keeps the same hash-at-rest and expiry semantics as the
// Postgres store so a test exercising the memory store proves the same behavior.
type MemoryStore struct {
	mu     sync.Mutex
	byHash map[string]*Session
	now    func() time.Time
}

// NewMemoryStore builds an empty in-memory session store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byHash: make(map[string]*Session), now: time.Now}
}

// Create mints and stores a session, returning it with its raw secrets.
func (s *MemoryStore) Create(_ context.Context, issuer, subject, actorID, userID, operatorGroup string, ttl time.Duration) (*Session, error) {
	if actorID == "" {
		return nil, errors.New("session: actor id is required")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	csrf, err := newToken()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	sess := &Session{
		Token:     token,
		ActorID:   actorID,
		UserID:    userID,
		Issuer:    issuer,
		Subject:   subject,
		CSRFToken: csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),

		OperatorGroup: operatorGroup,
	}
	s.mu.Lock()
	// Store a copy without the raw token, mirroring the Postgres store which
	// retains only the hash.
	stored := *sess
	stored.Token = ""
	s.byHash[hashToken(token)] = &stored
	s.mu.Unlock()
	return sess, nil
}

// Get resolves a raw token to a live session, treating an expired record as
// absent (and evicting it).
func (s *MemoryStore) Get(_ context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byHash[h]
	if !ok {
		return nil, ErrNotFound
	}
	if !rec.ExpiresAt.After(s.now().UTC()) {
		delete(s.byHash, h)
		return nil, ErrNotFound
	}
	out := *rec
	out.Token = token
	return &out, nil
}

// Delete revokes a session by its raw token; deleting an absent token is a no-op.
func (s *MemoryStore) Delete(_ context.Context, token string) error {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.byHash, hashToken(token))
	s.mu.Unlock()
	return nil
}
