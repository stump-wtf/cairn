package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// MemberInput is one file to place in a bundle. Body is streamed and dedup'd
// independently like any other artifact body (ADR-0008).
type MemberInput struct {
	Name              string
	Body              io.Reader
	DeclaredMediaType string
}

// CreateBundleInput creates one bundle artifact with N ordered members.
type CreateBundleInput struct {
	Title      string
	Members    []MemberInput
	Provenance artifact.Provenance
	Access     artifact.AccessPolicy
	ExpiresAt  time.Time
	// Tags are client-asserted routing strings on the bundle as a whole
	// (ADR-0018); members carry none of their own.
	Tags []string
}

// MaxBundleMembers bounds a bundle's member count. Every member is spooled
// and staged before the bundle commits, so without a bound the count, not the
// per-member upload cap, would decide what one request can hold.
const MaxBundleMembers = 256

// membersField is the caller-facing name of a bundle's member list.
const membersField = "members"

// MemberField names one member's sub-field ("name" or "content") the way every
// surface reports it: members[i].name.
func MemberField(i int, part string) string {
	return fmt.Sprintf("%s[%d].%s", membersField, i, part)
}

// MemberNames checks bundle member names in member order: each must be
// non-empty and unique within the bundle. The multipart create and
// CreateBundle share it, so both report a bad name identically.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-4; SPEC-0002 REQ "Bundles with N
// Members"
type MemberNames struct {
	seen map[string]struct{}
}

// Check reports member i's name as a violation at loc, or nil when it is valid.
func (m *MemberNames) Check(i int, name string, loc errs.Location) *errs.Invalid {
	if name == "" {
		return errs.Violate(MemberField(i, "name"), loc, errs.ReasonRequired)
	}
	if _, dup := m.seen[name]; dup {
		return errs.Violate(MemberField(i, "name"), loc, errs.ReasonDuplicate, errs.WithValue(name))
	}
	if m.seen == nil {
		m.seen = map[string]struct{}{}
	}
	m.seen[name] = struct{}{}
	return nil
}

// MemberTooLarge is the violation for member i's content over the upload cap.
// The content is never echoed (SPEC-0019 VE-3).
func MemberTooLarge(i int, loc errs.Location, limit int64) *errs.Invalid {
	return errs.Violate(MemberField(i, "content"), loc, errs.ReasonTooLarge, errs.WithLimit(limit, errs.UnitBytes))
}

// TooManyMembers is the violation for a bundle of more than MaxBundleMembers.
func TooManyMembers(loc errs.Location) *errs.Invalid {
	return errs.Violate(membersField, loc, errs.ReasonTooMany, errs.WithLimit(MaxBundleMembers, errs.UnitCount))
}

// validate checks the input before any member streams. The member list and the
// title are the caller's, and every bad name is reported, not only the first
// (SPEC-0019 VE-4). Provenance, access and expiry are server-derived, so a gap
// there is an internal error rather than a client violation.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-4; SPEC-0002 REQ "Bundles with N
// Members"
func (in CreateBundleInput) validate() error {
	switch {
	case in.Provenance.Channel == "":
		return errors.New("bundle: provenance channel is required")
	case in.Provenance.ActorID == "":
		return errors.New("bundle: provenance actor is required")
	case in.Access.OwnerID == "":
		return errors.New("bundle: access owner is required")
	case in.Access.Visibility == "":
		return errors.New("bundle: access visibility is required")
	case in.ExpiresAt.IsZero():
		return errors.New("bundle: expiry is required")
	case len(in.Members) == 0:
		return errs.Violate(membersField, errs.LocBody, errs.ReasonRequired)
	case len(in.Members) > MaxBundleMembers:
		return TooManyMembers(errs.LocBody)
	}
	bad := []*errs.Invalid{artifact.CheckTitle(in.Title, "title", errs.LocBody)}
	var names MemberNames
	for i, m := range in.Members {
		if m.Body == nil {
			return fmt.Errorf("bundle: member %d has a nil body", i)
		}
		bad = append(bad, names.Check(i, m.Name, errs.LocBody))
	}
	if inv := errs.Join(bad...); inv != nil {
		return inv
	}
	return nil
}

// stagedMember pairs a streamed blob with its member name and ordinal.
type stagedMember struct {
	ordinal int
	name    string
	blob    *StagedBlob
}

// CreateBundle streams every member to storage (each hashed, size-limited, and
// dedup'd independently), then finalizes atomically: upsert each member blob,
// insert the bundle artifact (share type `bundle`, body_sha256 NULL), and insert
// one ordered bundle_members row per file. Members are addressable within the
// bundle as <bundle_id>/<name> but carry no public id of their own — the bundle
// owns the single short URL.
//
// Governing: ADR-0008 (bundles with N members), SPEC-0002 REQ "Bundles with N
// Members", REQ "Database Operation Standards".
func (s *Store) CreateBundle(ctx context.Context, in CreateBundleInput) (*artifact.Artifact, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	// Normalized before any member streams, as CreateArtifact does.
	//
	// Governing: ADR-0018, SPEC-0002 REQ "Artifact Tags"
	tags, err := artifact.NormalizeTags(in.Tags)
	if err != nil {
		return nil, err
	}

	// Stream every member to a staging object first. Each member's staging object
	// is transient on EVERY path (committed OR rolled back); reclaim each
	// unconditionally on the way out so no staging/<rand> orphan survives.
	// Promotion to the content-addressed key is deferred to CommitBlob, under the
	// blob-row lock, so it serializes against the SPEC-0009 reaper (§2).
	//
	// Governing: SPEC-0002 REQ "Content Addressing and Blobs".
	staged := make([]stagedMember, 0, len(in.Members))
	defer func() {
		for _, m := range staged {
			m.blob.Discard(ctx, s.obj)
		}
	}()

	// A member over the upload cap does not stop the others streaming, so every
	// oversize member is named in one rejection (SPEC-0019 VE-4). The cap is
	// enforced as each streams, so a skipped member costs at most one byte past
	// it.
	var totalSize int64
	var tooLarge []*errs.Invalid
	for i, m := range in.Members {
		sb, err := StageBlob(ctx, s.obj, m.Body, s.maxBytes, m.DeclaredMediaType)
		if errors.Is(err, errs.ErrTooLarge) {
			tooLarge = append(tooLarge, MemberTooLarge(i, errs.LocBody, s.maxBytes))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: stream member %q: %w", m.Name, err)
		}
		staged = append(staged, stagedMember{ordinal: i, name: m.Name, blob: sb})
		totalSize += sb.Size
	}
	if inv := errs.Join(tooLarge...); inv != nil {
		return nil, fmt.Errorf("bundle: stream members: %w", inv)
	}

	art := &artifact.Artifact{
		ShareType:   artifact.TypeBundle,
		Title:       in.Title,
		Size:        totalSize,
		MediaType:   "application/vnd.cairn.bundle",
		Previewable: s.registry.Resolve(artifact.TypeBundle).PreviewableMedia(""),
		Provenance:  in.Provenance,
		Access:      in.Access,
		Tags:        tags,
		ExpiresAt:   in.ExpiresAt,
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("bundle: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, m := range staged {
		if err := CommitBlob(ctx, tx, s.obj, m.blob); err != nil {
			return nil, fmt.Errorf("bundle: %w", err)
		}
	}

	// Insert the bundle artifact (body_sha256 NULL) with public-id retry.
	if err := s.insertArtifact(ctx, tx, art, nil); err != nil {
		return nil, err
	}

	for _, m := range staged {
		if err := insertBundleMember(ctx, tx, art.ID, m); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bundle: commit: %w", err)
	}
	s.emitCreated(art)
	return art, nil
}

func insertBundleMember(ctx context.Context, tx pgx.Tx, bundleID int64, m stagedMember) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO bundle_members (bundle_id, ordinal, name, blob_sha256, media_type, size_bytes)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		bundleID, m.ordinal, m.name, m.blob.SHA256, m.blob.MediaType, m.blob.Size,
	); err != nil {
		return fmt.Errorf("bundle: insert member %q: %w", m.name, err)
	}
	return nil
}

// Member is one bundle member's metadata (no body), in bundle order. The bundle
// viewer projects these into the file rail and delegates each back through the
// registry to its own viewer (SPEC-0003 REQ "Bundle Viewer").
type Member struct {
	Ordinal   int
	Name      string
	MediaType string
	Size      int64
	SHA256    string
}

// ListMembers returns a bundle's members in bundle order (ordinal ASC). A
// non-bundle artifact yields no members (empty slice, no error): the caller
// resolves the viewer from the artifact's own type, so "not a bundle" is simply
// "no member rail", never a failure. Unknown/expired ids surface as not-found
// via GetByPublicID, keeping probing uniform (ADR-0007).
//
// Governing: SPEC-0002 REQ "Bundles with N Members" (ordered members),
// SPEC-0003 REQ "Bundle Viewer".
func (s *Store) ListMembers(ctx context.Context, publicID string) ([]Member, error) {
	a, err := s.GetByPublicID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	if a.ShareType != artifact.TypeBundle {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT ordinal, name, media_type, size_bytes, blob_sha256
		 FROM bundle_members
		 WHERE bundle_id = $1
		 ORDER BY ordinal ASC`,
		a.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("list members %s: %w", publicID, err)
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Ordinal, &m.Name, &m.MediaType, &m.Size, &m.SHA256); err != nil {
			return nil, fmt.Errorf("scan member %s: %w", publicID, err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members %s: %w", publicID, err)
	}
	return members, nil
}

// OpenMember opens a bundle member's body by <bundle public id>/<name>. Unknown
// bundles, non-bundle artifacts, and unknown member names all return a uniform
// not-found. The returned SHA-256 lets a reader re-verify integrity.
//
// Governing: SPEC-0002 REQ "Bundles with N Members" (member addressable, not
// independently shareable), REQ "Artifact Lifecycle — Read".
func (s *Store) OpenMember(ctx context.Context, publicID, name string) (io.ReadCloser, BodyInfo, error) {
	a, err := s.GetByPublicID(ctx, publicID)
	if err != nil {
		return nil, BodyInfo{}, err
	}
	if a.ShareType != artifact.TypeBundle {
		// A member of a non-bundle does not exist; keep the not-found uniform.
		return nil, BodyInfo{}, fmt.Errorf("member %s/%s: %w", publicID, name, errs.ErrNotFound)
	}

	var (
		info       BodyInfo
		storageKey string
	)
	err = s.pool.QueryRow(ctx,
		`SELECT b.sha256, b.size_bytes, m.media_type, b.storage_key
		 FROM bundle_members m
		 JOIN blobs b ON b.sha256 = m.blob_sha256
		 WHERE m.bundle_id = $1 AND m.name = $2`,
		a.ID, name,
	).Scan(&info.SHA256, &info.Size, &info.MediaType, &storageKey)
	if err != nil {
		// Only a genuine miss is not-found; a transient DB fault must surface as
		// an internal error, not a spurious 404 (mirror GetByPublicID).
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, BodyInfo{}, fmt.Errorf("member %s/%s: %w", publicID, name, errs.ErrNotFound)
		}
		return nil, BodyInfo{}, fmt.Errorf("open member %s/%s: %w", publicID, name, err)
	}

	rc, err := s.obj.Get(ctx, storageKey)
	if err != nil {
		return nil, BodyInfo{}, fmt.Errorf("open member %s/%s: %w", publicID, name, err)
	}
	return rc, info, nil
}
