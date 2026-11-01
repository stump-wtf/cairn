package metrics

// Scrape-time storage and expiry gauges (SPEC-0014 REQ-2, REQ-6).
//
// These gauges are read from the database at scrape time rather than maintained
// inline: keeping them correct through every delete path, TTL edit and failed
// upload would need every one of those paths to adjust a gauge perfectly
// forever, and one missed adjustment yields a permanently wrong number that
// still looks plausible (SPEC-0014 design). The same reasoning is why a
// collector that cannot compute a value omits it and increments
// cairn_metrics_collection_errors_total instead of reporting zero: for
// cairn_artifacts_past_ttl in particular, zero is the healthy reading, so a
// blind collector reporting zero would assert health at the exact moment it
// went blind.
//
// cairn_storage_bytes is summed from the size recorded on each artifact row.
// The database is authoritative for what Cairn believes it stored; the object
// store's own accounting is a separate measurement, and the divergence between
// the two is a finding worth being able to see, so neither is derived from the
// other.
//
// Governing: ADR-0021, SPEC-0014 REQ-2, REQ-5, REQ-6
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"context"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/cairn/internal/artifact"
)

// Storage collector names: the cairn_metrics_collection_errors_total
// {collector} label values. Closed, because the counter's children are
// initialized from it at startup.
const (
	CollectorStorageCounts = "storage_counts"
	CollectorPastTTL       = "past_ttl"
)

// StorageCollectors is the whole collector label set.
var StorageCollectors = []string{CollectorStorageCounts, CollectorPastTTL}

// TypeUsage is one share type's live-row count and summed recorded size.
type TypeUsage struct {
	Count int64
	Bytes int64
}

// Storage is the store's read-only aggregate surface, satisfied by
// *store.Store. Two methods, because the two gauges fail independently: a
// storage read that cannot answer says nothing about whether expiry is
// working, and one collapsed call would let it silence the past-TTL gauge too.
type Storage interface {
	// StorageUsageByType groups live artifact rows by share type. Types with
	// no live rows may be absent; the collector reports them as zero from the
	// registry's known set (SPEC-0014 REQ-2).
	StorageUsageByType(ctx context.Context) (map[artifact.ShareType]TypeUsage, error)
	// CountArtifactsPastTTL counts live rows whose expires_at is in the past.
	CountArtifactsPastTTL(ctx context.Context) (int, error)
}

// collectionErrors is cairn_metrics_collection_errors_total, emitted by the
// storage collector rather than registered as an ordinary CounterVec for one
// reason: a collection failure is discovered DURING a scrape, and a gather reads
// registered metrics concurrently with the collectors that feed them, so an
// increment made inside Collect can be snapshotted before it lands and surface
// a scrape later. That is one scrape of "everything is fine" at the exact
// moment a collector went blind, which is the thing REQ-6 forbids. Emitting the
// family from the collector that detects the failure makes the ordering program
// order instead. The {collector} set stays closed (StorageCollectors), so the
// family is still emitted complete on every healthy scrape.
type collectionErrors struct {
	mu    sync.Mutex
	desc  *prometheus.Desc
	count map[string]uint64
}

func newCollectionErrors() *collectionErrors {
	e := &collectionErrors{
		desc: prometheus.NewDesc(
			"cairn_metrics_collection_errors_total",
			"Scrape-time collector failures, by collector. A failed collector omits its values rather than reporting zero (SPEC-0014 REQ-6).",
			[]string{"collector"}, nil),
		count: make(map[string]uint64, len(StorageCollectors)),
	}
	for _, c := range StorageCollectors {
		e.count[c] = 0
	}
	return e
}

func (e *collectionErrors) inc(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count[name]++
}

func (e *collectionErrors) metrics() []prometheus.Metric {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.count))
	for name := range e.count {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]prometheus.Metric, 0, len(names))
	for _, name := range names {
		out = append(out, prometheus.MustNewConstMetric(e.desc, prometheus.CounterValue,
			float64(e.count[name]), name))
	}
	return out
}

// storageCollector computes the storage and expiry gauges at scrape time.
type storageCollector struct {
	store Storage
	// known is the closed share_type label set: the keys of the registry this
	// instance serves, reported in full on every scrape including the types
	// with no live rows (SPEC-0014 REQ-2).
	known []artifact.ShareType
	reg   *Registry
}

var _ prometheus.Collector = (*storageCollector)(nil)

// RegisterStorage installs the scrape-time storage collector over store.
// known is the share-type registry's key set. cairn serve calls it once, after
// the store and the share-type registry exist; calling it twice registers a
// second identical collector, which MustRegister refuses, so it stays a boot
// call.
func (r *Registry) RegisterStorage(store Storage, known []artifact.ShareType) {
	if r == nil || store == nil {
		return
	}
	r.reg.MustRegister(&storageCollector{
		store: store,
		known: slices.Clone(known),
		reg:   r,
	})
}

func (c *storageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.reg.artifactsTotal
	ch <- c.reg.storageBytes
	ch <- c.reg.pastTTL
	ch <- c.reg.collErrors.desc
}

func (c *storageCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	byType, err := c.store.StorageUsageByType(ctx)
	if err != nil {
		// The counts and the byte totals cannot be computed, so they are
		// omitted: a zero would read as an empty store (SPEC-0014 REQ-6).
		c.reg.ObserveCollectionError(CollectorStorageCounts)
	} else {
		for _, m := range c.usage(byType) {
			ch <- m
		}
	}
	count, err := c.store.CountArtifactsPastTTL(ctx)
	if err != nil {
		// The load-bearing gauge: omitted, never zero, and the error counter
		// is what tells the reader the absence was a failure rather than an
		// empty instance.
		c.reg.ObserveCollectionError(CollectorPastTTL)
	} else {
		ch <- prometheus.MustNewConstMetric(c.reg.pastTTL, prometheus.GaugeValue, float64(count))
	}
	// Last, so every increment this scrape made is in the values emitted for it.
	for _, m := range c.reg.collErrors.metrics() {
		ch <- m
	}
}

// usage renders the per-type gauges: every row the query returned, plus a zero
// for each known type it did not.
func (c *storageCollector) usage(byType map[artifact.ShareType]TypeUsage) []prometheus.Metric {
	out := make([]prometheus.Metric, 0, 2*(len(byType)+len(c.known)))
	seen := make(map[artifact.ShareType]bool, len(byType)+len(c.known))
	for _, shareType := range slices.Concat(c.known, sortedKeys(byType)) {
		if seen[shareType] {
			continue
		}
		seen[shareType] = true
		usage := byType[shareType]
		out = append(out,
			prometheus.MustNewConstMetric(c.reg.artifactsTotal, prometheus.GaugeValue,
				float64(usage.Count), string(shareType)),
			prometheus.MustNewConstMetric(c.reg.storageBytes, prometheus.GaugeValue,
				float64(usage.Bytes), string(shareType)),
		)
	}
	return out
}

func sortedKeys(byType map[artifact.ShareType]TypeUsage) []artifact.ShareType {
	keys := make([]artifact.ShareType, 0, len(byType))
	for k := range byType {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
