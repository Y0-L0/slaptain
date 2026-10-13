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

package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Reasons of the ReplicationConverged condition (ADR-008 amendment 2026-09-14).
const (
	reasonCSNsMatch            = "CSNsMatch"
	reasonCSNsDiverged         = "CSNsDiverged"
	reasonCSNQueriesIncomplete = "CSNQueriesIncomplete"
)

// csnReading is one contextCSN query result: what was asked (which pod or
// remote address, which database suffix) and what came back — the CSN vector,
// or the error that prevented reading it. A failed read is a reading too: it
// carries "we could not see this", which is evidence in its own right and must
// never silently vanish from a verdict.
type csnReading struct {
	// Pod is the pod (local) or remote address the query went to.
	Pod string
	// Suffix is the database suffix the contextCSN belongs to. CSN vectors are
	// only comparable within one suffix.
	Suffix string
	// CSNs is the raw contextCSN vector; empty when the database has none.
	CSNs []string
	// Err is non-empty when the query failed. CSNs is then meaningless.
	Err string
	// Answered records whether the SERVER responded at all, which is a
	// different question from whether the query succeeded: a bind rejection or
	// a search refusal is an answer, a dial timeout is not. Same rule ADR-030's
	// readiness probe uses, and for the same reason — "is it up" must not be
	// confused with "may this identity read that".
	//
	// True whenever Err is empty.
	Answered bool
}

// localVerdict is the outcome of comparing local per-pod contextCSN readings.
type localVerdict struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// NewestBySuffix carries the newest CSN timestamp observed per database
	// suffix, the baseline cross-site comparison uses.
	NewestBySuffix map[string]time.Time
	// ReadoutBySuffix carries the MEASUREMENT behind the verdict, per database
	// suffix: what was attempted, what was readable, and how far apart the
	// readable pods are. One entry per database the pass attempted; a database
	// absent from the map was never asked about (ADR-033 decision 5 — presence
	// in the input is what means "attempted").
	//
	// The verdict above is a judgement and collapses to a condition status;
	// this is the quantity that judgement reduces away. ADR-033 decision 3
	// names it as Surface 2's: "per-database CSN convergence and the measured
	// spread, labelled by database — the quantity ReplicationConverged reduces
	// to a boolean".
	ReadoutBySuffix map[string]dbCSNReadout
	// Silent suppresses the condition write entirely (too little data to say
	// anything at all).
	Silent bool
}

// dbCSNReadout is one database's local contextCSN measurement from one pass.
//
// Three numbers, and the distinctions between them are the whole point:
//
//	PodsExpected — the pass asked this many pods about this database.
//	PodsRead     — this many answered with a contextCSN that parsed.
//	Spread       — newest minus oldest over those, or nil when there is no
//	               spread to be had.
//
// `PodsRead < PodsExpected` is the alertable condition, which is why these are
// two absolute counts rather than a ratio: a ratio divides by a count that can
// be zero, and `PodsRead == 0` is itself a real and important reading — the
// 2026-10-06 shape where a site could read none of its own databases, a fact
// that lived only as English inside a condition message.
//
// Computed on EVERY pass, converged or not. A converged database's spread of 0
// is the measurement Surface 2 most wants to carry, and the diverged branch's
// message is not a place a measurement can live.
type dbCSNReadout struct {
	// PodsExpected is how many (pod, database) pairs this pass holds for the
	// database — one per read-write pod it queried, FAILURES INCLUDED.
	//
	// Counted from the readings rather than from the replica count, so both
	// numbers come from the same evidence and `PodsRead < PodsExpected` has
	// exactly one meaning: a pod was asked and could not be read. Taking it
	// from spec.replicas instead would make the comparison fire for two
	// unrelated causes — an unreadable pod, and a pass that never asked — with
	// nothing in the series to tell them apart.
	PodsExpected int
	// PodsRead is how many of those yielded a contextCSN that parsed. A pod
	// that answered with no contextCSN at all did NOT yield a reading: there is
	// no timestamp in an empty vector, so counting it would put a pod into the
	// spread's denominator while contributing nothing to its value.
	PodsRead int
	// Spread is the newest minus the oldest contextCSN timestamp over the pods
	// that answered, and is nil when PodsRead < 2 — fewer than two readings is
	// not a spread, and a 0 there would be indistinguishable from the healthiest
	// reading on the surface (ADR-033 decision 5's converse: 0 with two or more
	// readings is a MEASUREMENT and must survive).
	//
	// A pointer rather than a duration plus a bool, so "known but unset" and
	// "unknown but set" are unexpressible.
	//
	// Since 2026-10-06 obsmetrics.DatabaseCSN.Spread is a pointer too, and the
	// render gates on IT ALONE. So this nil is not a second statement of the
	// rule that happens to agree with the renderer's — it is the only
	// statement, carried unchanged all the way to the scrape. What was two
	// authorities agreeing by convention (a Duration here, a PairsRead >= 2
	// gate there) rendered a measured 0 for a database nobody measured, which
	// is the one failure ADR-033 decision 5 calls itself least able to detect.
	//
	// The consequence lands on the function below: `Spread != nil` exactly when
	// PodsRead >= 2 is now an invariant NOTHING downstream re-checks, and
	// TestEvaluateLocalConvergenceReadoutBySuffix asserts it on every case for
	// that reason.
	Spread *time.Duration
}

// databaseCSNReadouts computes the per-database readout above from one pass's
// readings: one entry per suffix present in the input, each carrying how many
// pods were asked, how many parsed, and the spread over those that did (nil
// below two).
//
// Separate from the verdict on purpose. The verdict answers "should the
// condition say False", and it short-circuits — the existing spread
// computation lives inside the diverged branch and never runs on a healthy
// database. A measurement has to run on every pass whatever the judgement
// turns out to be.
func databaseCSNReadouts(readings []csnReading) map[string]dbCSNReadout {
	// bounds tracks the newest and oldest parseable CSN timestamp per suffix,
	// over the readings that yielded one. Kept beside the counts rather than in
	// them, because a spread is only derivable once the whole pass is walked.
	type bounds struct {
		newest time.Time
		oldest time.Time
	}

	out := make(map[string]dbCSNReadout, len(readings))
	seen := make(map[string]bounds, len(readings))

	for _, r := range readings {
		// Presence of the reading is what means "attempted", failures included:
		// both counts come from the same evidence, so PodsRead < PodsExpected
		// has exactly one meaning.
		readout := out[r.Suffix]
		readout.PodsExpected++
		out[r.Suffix] = readout

		if r.Err != "" {
			continue
		}
		// Readable means PARSEABLE. newestCSN reports no error for an empty
		// vector — it returns the zero time — and a pod that answered with no
		// contextCSN has no timestamp to contribute, so counting it would put a
		// pod into the spread's denominator while adding nothing to its value.
		t, err := newestCSN(r.CSNs)
		if err != nil || t.IsZero() {
			continue
		}

		readout.PodsRead++
		out[r.Suffix] = readout

		b, had := seen[r.Suffix]
		if !had {
			b.newest, b.oldest = t, t
		}
		if t.After(b.newest) {
			b.newest = t
		}
		if t.Before(b.oldest) {
			b.oldest = t
		}
		seen[r.Suffix] = b
	}

	for suffix, readout := range out {
		if readout.PodsRead < 2 {
			continue
		}
		b := seen[suffix]
		spread := b.newest.Sub(b.oldest)
		readout.Spread = &spread
		out[suffix] = readout
	}
	return out
}

// evaluateLocalConvergence judges whether the local RW pods agree on their
// contextCSN, PER DATABASE.
//
// A contextCSN vector is a property of one database on one pod, and two
// databases have different vectors by construction: independent write
// histories, disjoint serverID activity, last writes at unrelated times. So
// the only meaningful comparison is one suffix across pods; comparing db1's
// vector against db2's asks a question with no true answer, and on a
// multi-database cluster it answered "diverged" forever, reporting a "lag"
// that was really the age gap between two databases' last writes.
//
// The cluster verdict is the AND over databases, with three outcomes and a
// deliberate severity order:
//
//	False   — at least one database's pods disagree. Observed problems win:
//	          divergence is reported even when other readings were unreadable.
//	Unknown — no divergence was observed, but at least one (pod, database)
//	          pair could not be read. Positive evidence only: an absence that
//	          could mean "could not read it" never counts toward True.
//	True    — every database's readable pods agree and nothing was unreadable.
//
// What this can NOT see is unchanged and bounded by ADR-008's amendments: CSN
// equality on an idle database says everything replicable has replicated, not
// that the link works.
func evaluateLocalConvergence(readings []csnReading, replicas int32) localVerdict {
	v := localVerdict{
		NewestBySuffix:  map[string]time.Time{},
		ReadoutBySuffix: databaseCSNReadouts(readings),
	}
	if len(readings) == 0 {
		v.Silent = true
		return v
	}

	suffixes := sortedSuffixes(readings)
	setsBySuffix := map[string][][]string{}
	oldestBySuffix := map[string]time.Time{}
	var unreadable []string

	for _, r := range readings {
		if r.Err != "" {
			unreadable = append(unreadable, fmt.Sprintf("%s/%s", r.Pod, r.Suffix))
			continue
		}
		setsBySuffix[r.Suffix] = append(setsBySuffix[r.Suffix], r.CSNs)
		t, err := newestCSN(r.CSNs)
		if err != nil {
			continue
		}
		if t.After(v.NewestBySuffix[r.Suffix]) {
			v.NewestBySuffix[r.Suffix] = t
		}
		if o, seen := oldestBySuffix[r.Suffix]; !seen || t.Before(o) {
			oldestBySuffix[r.Suffix] = t
		}
	}

	var diverged []string
	for _, suffix := range suffixes {
		sets := setsBySuffix[suffix]
		if len(sets) < 2 || csnConverged(sets) {
			continue
		}
		lag := v.NewestBySuffix[suffix].Sub(oldestBySuffix[suffix])
		diverged = append(diverged, fmt.Sprintf("%s (%.1fs lag)", suffix, lag.Seconds()))
	}

	switch {
	case len(diverged) > 0:
		msg := fmt.Sprintf("CSN divergence on %d of %d databases across %d pods: %s",
			len(diverged), len(suffixes), replicas, strings.Join(diverged, "; "))
		if len(unreadable) > 0 {
			msg += fmt.Sprintf(" (%d further (pod, database) pairs unreadable: %s)",
				len(unreadable), summarizeList(unreadable, 4))
		}
		v.Status = metav1.ConditionFalse
		v.Reason = reasonCSNsDiverged
		v.Message = msg
	case len(unreadable) > 0:
		v.Status = metav1.ConditionUnknown
		v.Reason = reasonCSNQueriesIncomplete
		v.Message = fmt.Sprintf(
			"could not read contextCSN for %d of %d (pod, database) pairs: %s — "+
				"no divergence observed on what could be read, which is not evidence of convergence",
			len(unreadable), len(readings), summarizeList(unreadable, 4))
	default:
		v.Status = metav1.ConditionTrue
		v.Reason = reasonCSNsMatch
		v.Message = fmt.Sprintf("all %d pods report identical contextCSN for each of %d databases",
			replicas, len(suffixes))
	}
	return v
}

// peerVerdict is the outcome of comparing one external peer's contextCSN
// readings against our own.
type peerVerdict struct {
	State      ldapv1alpha1.ExternalPeerReplicationState
	LagSeconds string
	LastError  string
}

// evaluatePeerConvergence judges one external peer from the readings taken
// against it, PER DATABASE.
//
// A database is *verified* when BOTH sides yielded readable evidence for it: at
// least one of the peer's addresses answered for it, AND we hold a non-zero
// local baseline (localNewest) for that same suffix. Its lag is then our newest
// CSN for the suffix minus the peer's newest for it. Comparing across suffixes
// is meaningless and actively misleading: two healthy databases whose last
// writes are minutes apart would read as minutes of "lag".
//
// Both sides, because a comparison needs two operands, and the missing-local
// case fails open in the worst possible direction — a zero baseline makes every
// remote CSN look ahead, the remote-ahead clamp turns that into zero lag, and
// the peer reads Synced for a database we never read (ADR-008 amendment
// 2026-10-06). The one asymmetry is deliberate: a zero on the PEER's side
// against a readable baseline here is a measured deficit, so it stays Lagging.
//
// Severity order — observed problems win over missing evidence:
//
//	Unreachable       — nothing answered for any database.
//	Lagging           — a VERIFIED database is behind by more than threshold,
//	                    or the peer answered with no CSN at all for a database
//	                    we do hold a baseline for.
//	PartiallyVerified — every verified database is current, but at least one
//	                    database yielded no readable evidence on one side or
//	                    the other (named in LastError). Never Synced: a verdict
//	                    computed from whichever database answered is not the
//	                    peer's health.
//	Synced            — every database was read on BOTH sides and every one is
//	                    current.
//
// ADR-008's bound is unchanged: this is consumer-side and one-directional, and
// on an idle database equality is not proof the link works.
func evaluatePeerConvergence(
	localNewest map[string]time.Time,
	readings []csnReading,
	threshold time.Duration,
) peerVerdict {
	suffixes := sortedSuffixes(readings)
	remoteNewest := map[string]time.Time{}
	verified := map[string]bool{}

	for _, r := range readings {
		if r.Err != "" {
			continue
		}
		verified[r.Suffix] = true
		if t, err := newestCSN(r.CSNs); err == nil && t.After(remoteNewest[r.Suffix]) {
			remoteNewest[r.Suffix] = t
		}
	}

	if len(verified) == 0 {
		return peerVerdict{
			State:     ldapv1alpha1.ReplicationUnreachable,
			LastError: "all remote pod CSN queries failed",
		}
	}

	var unverified []string
	var maxLag time.Duration
	lagging := false
	for _, suffix := range suffixes {
		if !verified[suffix] {
			unverified = append(unverified, suffix)
			continue
		}
		// Our OWN baseline first, before anything is asked of the remote
		// value. localNewest is built only from local readings that parsed, so
		// a database whose every local read failed is simply absent from it —
		// and the zero time it then yields makes every remote CSN look "ahead",
		// which the clamp below turns into zero lag and a Synced verdict for a
		// database we never read. No baseline, no comparison basis: the
		// database is unverified, exactly as it would be if the PEER had been
		// the unreadable side.
		//
		// The order matters. Checked after the remote-zero branch, a database
		// empty on BOTH sides would land there, fall through as "nothing to
		// compare", and still be counted verified.
		//
		// Deliberately asymmetric, and only in this direction: a zero on the
		// PEER's side with a readable baseline here is a measured deficit, not
		// missing evidence, and stays Lagging.
		if localNewest[suffix].IsZero() {
			unverified = append(unverified, suffix)
			continue
		}
		remote := remoteNewest[suffix]
		if remote.IsZero() {
			// The peer answered but has no CSN for this database, while we have
			// one: it is behind us.
			lagging = true
			continue
		}
		lag := max(localNewest[suffix].Sub(remote),
			// remote is ahead — clocks, or it received a write first
			0)
		if lag > threshold {
			lagging = true
			if lag > maxLag {
				maxLag = lag
			}
		}
	}

	var lastError string
	if len(unverified) > 0 {
		lastError = fmt.Sprintf("no readable contextCSN for %d of %d databases: %s",
			len(unverified), len(suffixes), summarizeList(unverified, 4))
	}

	switch {
	case lagging:
		v := peerVerdict{State: ldapv1alpha1.ReplicationLagging, LastError: lastError}
		if maxLag > 0 {
			v.LagSeconds = fmt.Sprintf("%.1f", maxLag.Seconds())
		}
		return v
	case len(unverified) > 0:
		return peerVerdict{State: ldapv1alpha1.ReplicationPartiallyVerified, LastError: lastError}
	default:
		return peerVerdict{State: ldapv1alpha1.ReplicationSynced}
	}
}

// summarizeList renders at most max items, noting how many were elided, so a
// large cluster cannot produce an unbounded condition message.
func summarizeList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(items[:max], ", "), len(items)-max)
}

// sortedSuffixes returns the distinct suffixes of a reading set, sorted, so
// messages are deterministic.
func sortedSuffixes(readings []csnReading) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range readings {
		if _, dup := seen[r.Suffix]; dup {
			continue
		}
		seen[r.Suffix] = struct{}{}
		out = append(out, r.Suffix)
	}
	sort.Strings(out)
	return out
}
