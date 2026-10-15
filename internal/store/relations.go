package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
)

// MaxReferencedBy caps how many reverse relations one read returns, newest
// first. The bound is what keeps the reverse lookup a bounded query on an
// artifact many later artifacts point at; the flag says there was more.
//
// Governing: SPEC-0024 REQ "Relations on Read".
const MaxReferencedBy = 50

// Reader is the principal whose read authorization decides which relation
// targets a read may show. Today that authorization is the one a read already
// applies — the artifact exists and has not expired — so every reader sees the
// same set; when SPEC-0023's authorizeRead lands it lands here, and every
// relation surface tightens with it, because they all resolve targets through
// readableTargets.
//
// Governing: SPEC-0023 REQ "Read Authorization", SPEC-0024 REQ "Relations on
// Read".
type Reader struct {
	UserID string
}

// RelationView is one relation as a read returns it. When the reader cannot open
// the target, PublicID and Title are empty and Available is false: the reader
// learns that a link exists and of what kind, and nothing about what it points
// at.
type RelationView struct {
	Kind      artifact.RelationKind
	PublicID  string
	Title     string
	Available bool
}

// RelationsResult is one artifact's relations from both ends: what it declares,
// and what names it.
type RelationsResult struct {
	Relations             []RelationView
	ReferencedBy          []RelationView
	ReferencedByTruncated bool
}

// target is a resolved relation target: as much of the row behind a public id
// as resolving one is allowed to use.
type target struct {
	id          int64
	title       string
	ownerUserID string
	ownerTeamID string
}

// querier is the read half of a pgx connection: the pool for a plain read, a tx
// when the resolution has to agree with the write it guards.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// readableTargets resolves public ids to the artifacts the reader can open, in
// one query. An id missing from the map is a target the reader cannot open:
// unknown, expired, or (once SPEC-0023 lands) not theirs. The three are never
// distinguished, because which one it was is exactly the answer a probe is
// looking for.
//
// This is the one function that decides relation readability: create fails when
// a target is missing from its result, and every read filters through it as the
// reader. SPEC-0024 names it so SPEC-0023 replaces one body rather than eight
// call sites.
//
// Governing: SPEC-0024 REQ "Relation Targets Must Be Readable by the Creator",
// REQ "Relations on Read".
func readableTargets(ctx context.Context, q querier, _ Reader, publicIDs []string) (map[string]target, error) {
	if len(publicIDs) == 0 {
		return map[string]target{}, nil
	}
	rows, err := q.Query(ctx, `
		SELECT a.public_id, a.id, a.title,
		       COALESCE(a.owner_user_id::text, ''), COALESCE(a.owner_team_id::text, '')
		FROM artifacts a
		WHERE a.public_id = ANY($1) AND a.expires_at > now()`, publicIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve relation targets: %w", err)
	}
	defer rows.Close()

	out := make(map[string]target, len(publicIDs))
	for rows.Next() {
		var (
			publicID string
			t        target
		)
		if err := rows.Scan(&publicID, &t.id, &t.title, &t.ownerUserID, &t.ownerTeamID); err != nil {
			return nil, fmt.Errorf("scan relation target: %w", err)
		}
		out[publicID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read relation targets: %w", err)
	}
	return out, nil
}

// relationEdges checks a create's declared relations against what the creating
// principal can open, and returns the rows to write plus the subset belonging to
// the created artifact's workspace, which is the only subset artifact.created
// may carry.
//
// A target the creator cannot open fails the whole create with the one error
// every unusable target produces. Nothing is persisted: the caller writes inside
// the creating transaction, and this returns before any row is written.
//
// Governing: SPEC-0024 REQ "Relation Targets Must Be Readable by the Creator",
// REQ "Relations in artifact.created".
func relationEdges(ctx context.Context, q querier, art *artifact.Artifact) ([]relationRow, []event.Relation, error) {
	if len(art.Relations) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, len(art.Relations))
	for i, rel := range art.Relations {
		ids[i] = rel.ID
	}
	// The check is the creating principal's read, not the owner's: a relation to
	// something the creator could not open would be a way to learn about it
	// through someone else's artifact.
	found, err := readableTargets(ctx, q, Reader{UserID: art.Provenance.CreatedByUserID}, ids)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]relationRow, 0, len(art.Relations))
	var evts []event.Relation
	for i, rel := range art.Relations {
		t, ok := found[rel.ID]
		if !ok {
			// No id, no reason: which kind of miss this was is the very thing a
			// probe is asking about, so all three answer identically.
			return nil, nil, fmt.Errorf("create: relation target unreadable: %w", artifact.ErrRelationTargetNotFound)
		}
		rows = append(rows, relationRow{kind: rel.Kind, publicID: rel.ID, targetID: t.id, position: i})
		// Same workspace only: until teams exist that is the owning user
		// (SPEC-0023 REQ "Events Go Only to the Artifact's Workspace").
		if sameWorkspace(art, t) {
			evts = append(evts, event.Relation{Kind: string(rel.Kind), ID: rel.ID})
		}
	}
	return rows, evts, nil
}

// sameWorkspace reports whether a target belongs to the workspace that owns the
// artifact being created: the same user, or the same team.
func sameWorkspace(art *artifact.Artifact, t target) bool {
	if art.Access.OwnerUserID != "" {
		return t.ownerUserID == art.Access.OwnerUserID
	}
	return art.Access.OwnerTeamID != "" && t.ownerTeamID == art.Access.OwnerTeamID
}

// relationRow is one relation row to insert.
type relationRow struct {
	kind     artifact.RelationKind
	publicID string
	targetID int64
	position int
}

// insertRelations writes resolved relations for an artifact inside its creating
// transaction, so an artifact and the edges it declares commit together or not
// at all.
//
// Governing: SPEC-0024 REQ "Database Operation Standards".
func insertRelations(ctx context.Context, tx pgx.Tx, artifactID int64, rows []relationRow) error {
	if len(rows) == 0 {
		return nil
	}
	const insertSQL = `
		INSERT INTO artifact_relations (artifact_id, kind, target_artifact_id, target_public_id, position)
		VALUES ($1, $2, $3, $4, $5)`
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(insertSQL, artifactID, string(r.kind), r.targetID, r.publicID, r.position)
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return fmt.Errorf("create: insert relations: %w", err)
	}
	return nil
}

// Relations returns one artifact's relations as a reader sees them, plus the
// artifacts that name it. A declared target the reader cannot open is reported
// as a relation with no id and no title, keeping its kind and its place in the
// list: dropping the entry would leak, by counting entries, that a target exists
// somewhere.
//
// Governing: SPEC-0024 REQ "Relations on Read".
func (s *Store) Relations(ctx context.Context, r Reader, publicID string) (RelationsResult, error) {
	var selfID int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM artifacts WHERE public_id = $1 AND expires_at > now()`, publicID,
	).Scan(&selfID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelationsResult{}, fmt.Errorf("resolve artifact %s: %w", publicID, errs.ErrNotFound)
		}
		return RelationsResult{}, fmt.Errorf("resolve artifact %s: %w", publicID, err)
	}
	out, err := s.declaredRelations(ctx, r, selfID)
	if err != nil {
		return RelationsResult{}, err
	}
	return s.addReferencedBy(ctx, r, selfID, out)
}

// declaredRelations reads the relations an artifact declares, in the order it
// declared them.
func (s *Store) declaredRelations(ctx context.Context, r Reader, artifactID int64) (RelationsResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, target_public_id
		FROM artifact_relations
		WHERE artifact_id = $1
		ORDER BY position`, artifactID)
	if err != nil {
		return RelationsResult{}, fmt.Errorf("read relations: %w", err)
	}
	defer rows.Close()

	var kinds []artifact.RelationKind
	var targets []string
	for rows.Next() {
		var (
			k  string
			id string
		)
		if err := rows.Scan(&k, &id); err != nil {
			return RelationsResult{}, fmt.Errorf("scan relation: %w", err)
		}
		kinds = append(kinds, artifact.RelationKind(k))
		targets = append(targets, id)
	}
	if err := rows.Err(); err != nil {
		return RelationsResult{}, fmt.Errorf("read relations: %w", err)
	}

	found, err := readableTargets(ctx, s.pool, r, targets)
	if err != nil {
		return RelationsResult{}, err
	}
	out := RelationsResult{Relations: make([]RelationView, 0, len(kinds))}
	for i, k := range kinds {
		t, ok := found[targets[i]]
		if !ok {
			out.Relations = append(out.Relations, RelationView{Kind: k})
			continue
		}
		out.Relations = append(out.Relations, RelationView{
			Kind: k, PublicID: targets[i], Title: t.title, Available: true,
		})
	}
	if len(out.Relations) == 0 {
		out.Relations = nil
	}
	return out, nil
}

// addReferencedBy appends the artifacts that name this one, newest first, capped
// at MaxReferencedBy with a flag saying there were more. One edge per artifact
// is listed: an artifact that relates twice is one entry in a "replies and
// derivatives" list, not two.
func (s *Store) addReferencedBy(ctx context.Context, r Reader, artifactID int64, in RelationsResult) (RelationsResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ar.kind, a.public_id
		FROM artifact_relations ar
		JOIN artifacts a ON a.id = ar.artifact_id
		WHERE ar.target_artifact_id = $1 AND a.expires_at > now()
		ORDER BY a.id DESC, ar.position
		LIMIT $2`, artifactID, MaxReferencedBy+1)
	if err != nil {
		return RelationsResult{}, fmt.Errorf("read referenced_by: %w", err)
	}
	defer rows.Close()

	type edge struct {
		kind     artifact.RelationKind
		publicID string
	}
	var edges []edge
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.kind, &e.publicID); err != nil {
			return RelationsResult{}, fmt.Errorf("scan referenced_by: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return RelationsResult{}, fmt.Errorf("read referenced_by: %w", err)
	}

	// One edge past the cap means there was more to look at, whether or not the
	// cap is what cut it off.
	more := len(edges) > MaxReferencedBy
	if more {
		edges = edges[:MaxReferencedBy]
	}

	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.publicID)
	}
	found, err := readableTargets(ctx, s.pool, r, ids)
	if err != nil {
		return RelationsResult{}, err
	}
	seen := make(map[int64]struct{}, len(edges))
	in.ReferencedBy = make([]RelationView, 0, len(edges))
	for _, e := range edges {
		t, ok := found[e.publicID]
		if !ok {
			continue // the reader cannot open the referrer, so it is not listed
		}
		if _, dup := seen[t.id]; dup {
			continue
		}
		seen[t.id] = struct{}{}
		in.ReferencedBy = append(in.ReferencedBy, RelationView{
			Kind: e.kind, PublicID: e.publicID, Title: t.title, Available: true,
		})
	}
	if len(in.ReferencedBy) == 0 {
		in.ReferencedBy = nil
	}
	in.ReferencedByTruncated = more
	return in, nil
}
