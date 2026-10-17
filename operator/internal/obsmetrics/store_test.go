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
	"reflect"
	"sync"
	"testing"
	"time"
)

// Every test here is named TestStore*, so the store's red can be isolated from
// the render's with `-run '^TestStore'` while RenderCluster is still a panicking
// stub — a panic aborts the whole test binary, and a seam whose red is swallowed
// by another seam's panic is a red nobody can count.
//
// The fixtures are obsmetrics_test.go's: healthyCluster and controlCluster
// share no value with each other, and neither does healthyDatabase with
// controlDatabase (the ridcoord_test.go:28-31 control convention). That is
// exactly what a "replaces wholesale" assertion needs — after replacing one with
// the other there must be no value of the first left anywhere.

// keyA and keyB are two different objects in the same namespace: the shape that
// matters, since a store keyed on something derived from the value rather than
// on the Key argument would collapse them.
var (
	keyA = Key{Namespace: testNamespace, Name: testCluster}
	keyB = Key{Namespace: testNamespace, Name: testControlCluster}
)

// clusterStore is a store with one observation already in it, plus the key it
// went in under. Tests that must not pass vacuously against a no-op stub all
// start here and assert the recording LANDED before they assert anything about
// its removal or replacement.
func clusterStore(t *testing.T) *Store[ClusterObservation] {
	t.Helper()
	s := &Store[ClusterObservation]{}
	s.Record(keyA, healthyCluster())
	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("Snapshot() holds %d observations after one Record, want 1 — "+
			"nothing below can mean anything until a Record lands", got)
	}
	return s
}

// TestStoreRecordThenSnapshotReturnsTheObservationUnchanged is the base case:
// what went in is what comes out, field for field. A store that re-derived
// anything, or dropped a field it did not understand, fails here before any of
// the absence rules are reached.
func TestStoreRecordThenSnapshotReturnsTheObservationUnchanged(t *testing.T) {
	s := &Store[ClusterObservation]{}
	want := healthyCluster()
	s.Record(keyA, want)

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %d observations, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("Snapshot()[0] = %+v, want %+v", got[0], want)
	}
}

// TestStoreRecordReplacesWholesale pins that one key holds one observation,
// swapped entire. Merging field by field would let a reading THIS pass could not
// take survive as the previous pass's value, carried under this pass's
// freshness anchor — ADR-033 decision 5's "never the last known value",
// laundered into a series that claims it was just measured.
//
// The two fixtures share no value, so any surviving trace of the first is
// visible: a different namespace, cluster, mesh, timestamp, database set and
// peer set.
func TestStoreRecordReplacesWholesale(t *testing.T) {
	s := clusterStore(t)
	want := controlCluster()
	s.Record(keyA, want)

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %d observations after re-Recording one key, want 1 — "+
			"a replacement must not become a second entry", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("Snapshot()[0] = %+v, want exactly the second observation %+v", got[0], want)
	}
	// Named explicitly rather than left to DeepEqual: a merge that kept the
	// first pass's databases would still produce a struct, and this says which
	// property the equality above was protecting.
	for _, db := range got[0].Databases {
		if db.Database == testDB || db.Database == testDB2 {
			t.Errorf("database %q from the REPLACED observation survived the replacement", db.Database)
		}
	}
}

// TestStoreEvictRemovesTheObservation pins the only way an entry leaves. The
// presence assertion in clusterStore is what gives this teeth: "record, evict,
// see nothing" passes against a store that never recorded anything at all.
func TestStoreEvictRemovesTheObservation(t *testing.T) {
	s := clusterStore(t)
	s.Evict(keyA)

	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot() = %+v after evicting the only key, want nothing at all", got)
	}
}

// TestStoreEvictedKeyContributesNothingNotAZeroObservation is decision 5 at the
// store layer. An evicted cluster must leave NO sample, and a zero-valued
// ClusterObservation left behind would render as an identity with empty labels
// and a 1970 timestamp — the "absent evidence exported as 0" this whole surface
// exists to avoid, arriving before the render ever gets a say.
func TestStoreEvictedKeyContributesNothingNotAZeroObservation(t *testing.T) {
	s := clusterStore(t)
	s.Evict(keyA)

	for _, obs := range s.Snapshot() {
		t.Errorf("Snapshot() yielded %+v after eviction; an absent key must contribute "+
			"no element, not a zero-valued observation", obs)
	}
}

// TestStoreEvictOfAnAbsentKeyLeavesTheRestAlone pins that eviction is scoped to
// its key and is not an error path. A reconcile that sees a deleted object it
// never recorded is routine, and it must not take a sibling's observation with
// it.
func TestStoreEvictOfAnAbsentKeyLeavesTheRestAlone(t *testing.T) {
	s := clusterStore(t)
	s.Evict(keyB)
	s.Evict(Key{Namespace: "never-recorded", Name: "nothing"})

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %d observations, want the 1 that was never evicted", len(got))
	}
	if !reflect.DeepEqual(got[0], healthyCluster()) {
		t.Errorf("Snapshot()[0] = %+v, want the untouched observation", got[0])
	}
}

// TestStoreKeepsDistinctKeysApart pins that the Key ARGUMENT is the identity. A
// store that keyed on something inside the value — Identity.Cluster is the
// tempting one, since every observation carries it — would work for clusters and
// silently collapse a cluster's databases onto one entry, each database's pass
// overwriting its siblings'.
func TestStoreKeepsDistinctKeysApart(t *testing.T) {
	s := &Store[DatabaseObservation]{}
	first := healthyDatabase()  // cluster "slapd", database "default"
	second := healthyDatabase() // same cluster, a second database
	second.Database = testDB2

	s.Record(Key{Namespace: first.Namespace, Name: first.Database}, first)
	s.Record(Key{Namespace: second.Namespace, Name: second.Database}, second)

	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot() = %d observations for two databases of one cluster, want 2 "+
			"(got %+v)", len(got), got)
	}
}

// TestStoreKeyFieldsDoNotCollideThroughConcatenation is the structural half of
// the same rule. These two keys join to the identical string under any single
// separator, and a store that built its map key that way would serve one
// object's observation for the other — a wrong reading, which is worse than a
// missing one, because nothing about it looks absent.
func TestStoreKeyFieldsDoNotCollideThroughConcatenation(t *testing.T) {
	s := &Store[ClusterObservation]{}
	s.Record(Key{Namespace: "a", Name: "b/c"}, healthyCluster())
	s.Record(Key{Namespace: "a/b", Name: "c"}, controlCluster())

	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot() = %d observations for two keys that differ only in where the "+
			"separator falls, want 2 (got %+v)", len(got), got)
	}
}

// TestStoreSnapshotIsOrderedByKey pins the deterministic walk. A map iterated
// directly reorders on every scrape, which makes the exported sample set
// undiffable and any golden-file review of Surface 2 impossible — the same
// reason RenderCluster promises an order.
func TestStoreSnapshotIsOrderedByKey(t *testing.T) {
	s := &Store[ClusterObservation]{}
	const nsA = "ns-a"
	// Recorded in an order that is neither the wanted one nor its reverse, so a
	// store returning insertion order fails too.
	s.Record(Key{Namespace: "ns-b", Name: testNameFirst}, healthyCluster())
	s.Record(Key{Namespace: nsA, Name: testNameLast}, controlCluster())
	s.Record(Key{Namespace: nsA, Name: testNameFirst}, healthyCluster())

	want := []Key{
		{Namespace: nsA, Name: testNameFirst},
		{Namespace: nsA, Name: testNameLast},
		{Namespace: "ns-b", Name: testNameFirst},
	}

	// The observations do not carry their key, so order is checked through the
	// values the fixtures differ in: healthy, control, healthy by key order.
	got := s.Snapshot()
	if len(got) != len(want) {
		t.Fatalf("Snapshot() = %d observations, want %d", len(got), len(want))
	}
	wantMesh := []string{testMesh, testControlMesh, testMesh}
	for i, mesh := range wantMesh {
		if got[i].Mesh != mesh {
			t.Errorf("Snapshot()[%d] is the observation for mesh %q, want the one for %q "+
				"(key order: %v)", i, got[i].Mesh, mesh, want)
		}
	}
}

// TestStoreSnapshotSliceIsIndependentOfTheStore pins that a caller cannot
// corrupt the store through what Snapshot handed it. The collector will sort,
// append to and iterate this slice on every scrape; if that aliased the store's
// own storage, a scrape would mutate the operator's observations.
//
// It pins the SLICE, not the observations in it: those share their backing
// arrays by documented contract (see Snapshot), because the only writer swaps
// whole observations and never mutates one in place.
func TestStoreSnapshotSliceIsIndependentOfTheStore(t *testing.T) {
	s := clusterStore(t)

	first := s.Snapshot()
	first[0] = ClusterObservation{}

	second := s.Snapshot()
	if len(second) != 1 {
		t.Fatalf("Snapshot() = %d observations after a caller mutated an earlier snapshot, want 1",
			len(second))
	}
	if !reflect.DeepEqual(second[0], healthyCluster()) {
		t.Errorf("Snapshot()[0] = %+v after a caller overwrote an earlier snapshot's element, "+
			"want the stored observation unchanged", second[0])
	}
}

// TestStoreZeroValueIsUsableWithoutAConstructor pins the absence of a
// constructor: a controller declares a Store as a struct field and records into
// it, with no wiring step that can be forgotten in one of the two controllers
// and leave that surface silently exporting nothing.
func TestStoreZeroValueIsUsableWithoutAConstructor(t *testing.T) {
	var s Store[DatabaseObservation]
	s.Record(keyA, healthyDatabase())

	if got := len(s.Snapshot()); got != 1 {
		t.Fatalf("a zero-value Store held %d observations after Record, want 1", got)
	}
}

// TestStoreKeepsAnObservationHoweverStale pins the absence of a TTL, which is a
// decision and not an omission (see the Store doc comment): staleness is
// REPORTED through the observation's own timestamp and the
// *_observation_timestamp_seconds families, never enforced by dropping the
// entry.
//
// A store that expired entries would make a stalled reconcile look like a
// deleted cluster — the series would vanish, and `time() - <anchor> > T`
// would have nothing left to fire on. That is decision 5 turned inside out: not
// a 0 standing in for absent evidence, but a silence standing in for a stall.
//
// If you landed here intending to add expiry: the question is which alert stops
// working, and it belongs in the ADR.
func TestStoreKeepsAnObservationHoweverStale(t *testing.T) {
	s := &Store[ClusterObservation]{}
	ancient := healthyCluster()
	ancient.Observed = time.Unix(0, 0)
	s.Record(keyA, ancient)

	// Any expiry would have to run on one of these; the store has no clock of
	// its own and no background goroutine, so this is the whole opportunity.
	for range 50 {
		s.Snapshot()
		s.Record(keyB, controlCluster())
		s.Evict(keyB)
	}

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %d observations, want the 1 stale one still held", len(got))
	}
	if !got[0].Observed.Equal(ancient.Observed) {
		t.Errorf("Observed = %s, want the recorded time %s held verbatim — the store must not "+
			"refresh a timestamp it did not observe", got[0].Observed, ancient.Observed)
	}
}

// TestStoreExposesNoExpiryOrCapacityAPI is a tripwire for the same decision at
// the API level. The method set is the whole surface: record, evict, read. A
// SetTTL, Prune, Expire or SetCapacity appearing here is a behaviour change
// that the test above cannot see, because it would be opt-in at the call site
// and the default path would stay green.
//
// Green from birth by construction — it asserts a shape, not a behaviour.
func TestStoreExposesNoExpiryOrCapacityAPI(t *testing.T) {
	want := map[string]bool{"Record": true, "Evict": true, "Snapshot": true}

	typ := reflect.TypeFor[*Store[ClusterObservation]]()
	got := map[string]bool{}
	for method := range typ.Methods() {
		got[method.Name] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("Store is missing the %s method", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("Store has an extra exported method %q. If it expires, prunes or caps "+
				"anything, that is ADR-033's "+
				"\"staleness is reported, not enforced\" being reversed, and it needs a "+
				"decision before it needs a method", name)
		}
	}
}

// TestStoreIsSafeForConcurrentUse pins the lock. The controllers Record from
// reconcile goroutines (controller-runtime runs reconcilers concurrently by
// default) while the Prometheus collector Snapshots from the scrape handler's,
// so an unsynchronised map here is not a theoretical race — it is a map
// concurrently written, which Go terminates the operator process for.
//
// Run it with -race: without the detector this test only pins the final state.
// With it, a store that dropped its lock fails here.
func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	s := &Store[ClusterObservation]{}
	const workers = 8
	const iterations = 200

	// keySurvivor is recorded by every worker and never evicted, so the final
	// state is deterministic however the goroutines interleave.
	keySurvivor := Key{Namespace: testNamespace, Name: "survivor"}

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			churn := Key{Namespace: testNamespace, Name: "churn"}
			for range iterations {
				s.Record(keySurvivor, healthyCluster())
				s.Record(churn, controlCluster())
				s.Snapshot()
				s.Evict(churn)
			}
		})
	}
	wg.Wait()

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() = %d observations after concurrent churn, want exactly the 1 "+
			"never-evicted key (got %+v)", len(got), got)
	}
	if !reflect.DeepEqual(got[0], healthyCluster()) {
		t.Errorf("Snapshot()[0] = %+v, want the observation every worker recorded", got[0])
	}
}
