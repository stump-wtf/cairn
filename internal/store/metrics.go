package store

// Scrape-time aggregates for the metrics gauges (SPEC-0014 REQ-2).
//
// These are the only two questions the metrics endpoint asks the database, and
// both are answered by one aggregate query each, at scrape time: keeping the
// gauges correct inline would need every delete path, TTL edit and failed
// upload to adjust them perfectly forever. The byte total is summed from the
// size recorded on each artifact row, not asked of the object store. The database is
// authoritative for what Cairn believes it stored, and divergence
// from the bucket's own accounting is a finding worth being able to see.
//
// Governing: ADR-0021, SPEC-0014 REQ-2; ADR-0008 (size recorded per row)
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"context"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/metrics"
)

// StorageUsageByType groups every live artifact row by share type, summing the
// size recorded on each row. Types with no live rows are absent from the
// result; the collector reports them as zero from the share-type registry's
// known set, so the label set stays complete (SPEC-0014 REQ-2).
func (s *Store) StorageUsageByType(ctx context.Context) (map[artifact.ShareType]metrics.TypeUsage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT share_type, count(*), coalesce(sum(size_bytes), 0)
		   FROM artifacts
		  GROUP BY share_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[artifact.ShareType]metrics.TypeUsage{}
	for rows.Next() {
		var (
			key   string
			usage metrics.TypeUsage
		)
		if err := rows.Scan(&key, &usage.Count, &usage.Bytes); err != nil {
			return nil, err
		}
		out[artifact.ShareType(key)] = usage
	}
	return out, rows.Err()
}

// CountArtifactsPastTTL counts the artifacts that still exist past their
// expiry: the load-bearing gauge, because it says the sweeper ran AND worked,
// where cairn_artifacts_expired_total only says it ran. The reaper deletes on
// the same `expires_at < now()` predicate, so a row counted here is one the
// next clean sweep should remove (SPEC-0014 REQ-2).
func (s *Store) CountArtifactsPastTTL(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM artifacts WHERE expires_at < now()`).Scan(&n)
	return n, err
}
