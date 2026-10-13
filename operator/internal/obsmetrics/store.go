/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package obsmetrics

import (
	"sort"
	"sync"
)

// Key identifies the object one observation belongs to: the namespace and name
// of the SlapdCluster, or of the SlapdDatabase, whose reconcile pass produced
// it. Kubernetes guarantees that pair is unique per kind per cluster, so it is
// the whole identity and nothing derived needs to be stored alongside it.
//
// It is a STRUCT and deliberately not a joined string. "ns" + "/" + "a/b" and
// "ns/a" + "/" + "b" are the same string and two different objects; struct
// equality compares the fields, so the collision is unexpressible rather than
// merely unlikely. Go's comparability makes it a map key directly, at no cost.
//
// Which name goes in it is the caller's one decision and the one place this
// can go wrong: a DatabaseObservation is keyed by the SLAPDDATABASE's name,
// never by its cluster's, because one cluster has many databases and keying on
// the cluster would have each database's pass overwrite its siblings' — a
// Record that silently loses an observation, which is decision 5's failure
// mode arriving through the back door.
type Key struct {
	Namespace string
	Name      string
}

// Store holds the most recent observation per key, for one observation type.
//
// It exists because Surface 2 has two clocks that do not line up: the operator
// observes on its reconcile tick (ADR-033 Consequences — nothing here is
// fresher than the 60 s CSN-monitoring pass) and Prometheus asks on its scrape
// interval. Something has to hold the last pass's readings in between, and
// making it a store rather than a recompute-on-scrape is what keeps a scrape
// from issuing LDAP queries — decision 3's "no pod-local counter" rule would be
// defeated by an exporter that went and looked.
//
// Deliberately simple, and each absence is a decision:
//
//   - NO TTL, NO EXPIRY, NO SIZE CAP. Staleness is reported, never enforced:
//     every observation carries its own Observed time and the two
//     *_observation_timestamp_seconds families export it, so a consumer writes
//     `time() - <anchor> > T` and sees a stalled pass. A store that dropped
//     an entry on age would instead make a stalled pass look like a deleted
//     cluster — the series would vanish and the staleness rule would have
//     nothing to fire on, which is decision 5 ("a reading the operator could
//     not take is never the last known value") inverted into a silence. It is
//     also what lets every controller record at ONE point per pass instead of
//     refreshing to stay alive.
//   - No automatic cleanup. An observation leaves only when its object does,
//     through Evict at the reconcile that saw the deletion. Bounded by
//     clusters × databases in one operator's scope, which ADR-033 already
//     counts as affordable cardinality.
//   - No merge. Record replaces the whole observation (see Record).
//
// The zero value is ready to use; there is no constructor. All methods are safe
// for concurrent use, which is not optional here: the controllers Record from
// reconcile goroutines while the Prometheus collector Snapshots from the
// scrape handler's.
//
// This is the one piece of state in an otherwise pure package — see the package
// comment for why it lives here rather than in internal/controller.
type Store[T any] struct {
	// mu is a named field and not an embedded sync.RWMutex: embedding would put
	// Lock, Unlock, RLock and RUnlock into Store's own exported method set,
	// which is both a wider surface than record/evict/read and a way for a
	// caller to hold the store's lock across its own work.
	mu sync.RWMutex
	// obs is created on first Record, so the zero value is usable and there is
	// no constructor a controller can forget to call.
	obs map[Key]T
}

// Record stores obs under key, REPLACING any observation already there.
//
// Wholesale, never field-by-field: a pass publishes what it saw, and merging
// would let a reading that this pass could not take survive as the previous
// pass's value under this pass's timestamp — the exact "never the last known
// value" that ADR-033 decision 5 forbids, laundered through a freshness anchor
// that says it was just measured.
func (s *Store[T]) Record(key Key, obs T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.obs == nil {
		s.obs = make(map[Key]T)
	}
	s.obs[key] = obs
}

// Evict removes key's observation, if any. Evicting an absent key is a no-op.
//
// This is the only way an entry leaves the store, and it is called when the
// object itself goes away, so the series stops rather than freezing at its last
// value.
func (s *Store[T]) Evict(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.obs, key)
}

// Snapshot returns the observations currently held, ordered by key (namespace,
// then name). An empty store returns an empty result, never a zero-valued
// observation: an absent key contributes nothing at all.
//
// The slice is freshly allocated on every call, so a caller may sort, truncate
// or append to it freely. The observations in it are value copies, but their
// SLICE fields (Databases, Peers, Pods) share backing arrays with the store's
// copy, so a caller must treat a snapshot as READ-ONLY. Deep-copying is
// avoidable rather than expensive: the only writer is Record, which swaps a
// whole observation and mutates nothing it was handed, so nothing in the store
// ever changes under a snapshot's feet.
//
// Ordering is part of the contract for the same reason it is on RenderCluster:
// a deterministic walk makes the exported sample set diffable, and a map walked
// directly would reorder it on every scrape.
func (s *Store[T]) Snapshot() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]Key, 0, len(s.obs))
	for k := range s.obs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})

	out := make([]T, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.obs[k])
	}
	return out
}
