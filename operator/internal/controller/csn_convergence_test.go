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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// Two databases whose CSN vectors differ by construction. Independent write
// histories mean disjoint serverID activity and different timestamps; the only
// comparable unit is one suffix across pods.
const (
	sfxA = "dc=example,dc=org"
	sfxB = "dc=second,dc=example,dc=net"
)

// Vectors modelled on the live lab reading of 2026-09-14 (scm-s1): db1 carries
// five serverIDs, db2 three, and their newest writes are minutes apart.
var (
	csnsA = []string{
		"20260913211834.691270Z#000000#002#000000",
		"20260913212045.356801Z#000000#065#000000",
		"20260913212312.073437Z#000000#001#000000",
	}
	csnsB = []string{
		"20260913212135.592130Z#000000#003#000000",
		"20260913211000.706980Z#000000#066#000000",
	}
	// csnsAOlder is db1's vector as seen on a pod that is genuinely behind:
	// same serverIDs, one CSN ~6 minutes older.
	csnsAOlder = []string{
		"20260913211834.691270Z#000000#002#000000",
		"20260913212045.356801Z#000000#065#000000",
		"20260913211712.073437Z#000000#001#000000",
	}
)

func TestEvaluateLocalConvergence(t *testing.T) {
	tests := []struct {
		name       string
		readings   []csnReading
		replicas   int32
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsgHas []string
		wantSilent bool
	}{
		{
			// THE INCIDENT SHAPE. Two healthy databases, each identical across
			// every pod, whose last writes are minutes apart. Flattening every
			// (pod × database) vector into one comparison reads this as
			// "diverged" with a "lag" that is the age gap between two different
			// databases' last writes — measured live on 2026-09-14 as
			// "local CSN divergence: 0.0s lag across 3 pods" on a fully
			// converged three-pod mesh.
			name: "two healthy databases with different vectors are converged",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-0", Suffix: sfxB, CSNs: csnsB},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-1", Suffix: sfxB, CSNs: csnsB},
				{Pod: "slapd-2", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-2", Suffix: sfxB, CSNs: csnsB},
			},
			replicas:   3,
			wantStatus: metav1.ConditionTrue,
			wantReason: "CSNsMatch",
			wantMsgHas: []string{"2 databases"},
		},
		{
			// Positive control: the single-database case the check was born
			// with must still read True, so a "refuse everything" implementation
			// cannot pass this suite vacuously.
			name: "single database converged across pods",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: csnsA},
			},
			replicas:   2,
			wantStatus: metav1.ConditionTrue,
			wantReason: "CSNsMatch",
		},
		{
			// Real divergence must still be caught — and must name WHICH
			// database diverged, since that is the actionable half.
			name: "one database diverged across pods names that database",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-0", Suffix: sfxB, CSNs: csnsB},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: csnsAOlder},
				{Pod: "slapd-1", Suffix: sfxB, CSNs: csnsB},
			},
			replicas:   2,
			wantStatus: metav1.ConditionFalse,
			wantReason: "CSNsDiverged",
			wantMsgHas: []string{sfxA},
		},
		{
			// A pod reporting no contextCSN at all while its peers report one
			// is divergence, not agreement (the never-synced / hidden-glue
			// shape, ADR-025).
			name: "a pod with no contextCSN diverges from pods that have one",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: nil},
			},
			replicas:   2,
			wantStatus: metav1.ConditionFalse,
			wantReason: "CSNsDiverged",
			wantMsgHas: []string{sfxA},
		},
		{
			// Strict failure direction: an unreadable (pod × database) pair is
			// not evidence of agreement. It caps the verdict at Unknown and
			// names what could not be read — an absence that could mean "could
			// not read it" must never count toward the good verdict.
			name: "an unreadable pod/database caps the verdict at Unknown",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-2", Suffix: sfxA, Err: "dial slapd-2: connection refused"},
			},
			replicas:   3,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "CSNQueriesIncomplete",
			wantMsgHas: []string{"slapd-2", sfxA},
		},
		{
			// Observed divergence outranks incompleteness: a problem we can see
			// is reported as a problem, not downgraded to "unknown".
			name: "observed divergence wins over an unreadable pod",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
				{Pod: "slapd-1", Suffix: sfxA, CSNs: csnsAOlder},
				{Pod: "slapd-2", Suffix: sfxA, Err: "dial slapd-2: connection refused"},
			},
			replicas:   3,
			wantStatus: metav1.ConditionFalse,
			wantReason: "CSNsDiverged",
			wantMsgHas: []string{sfxA},
		},
		{
			// Every query failed: the condition must say so rather than leave a
			// stale verdict standing (the old code returned early and left the
			// previous condition untouched).
			name: "all queries failing is Unknown, not silence",
			readings: []csnReading{
				{Pod: "slapd-0", Suffix: sfxA, Err: "dial: i/o timeout"},
				{Pod: "slapd-1", Suffix: sfxA, Err: "dial: i/o timeout"},
			},
			replicas:   2,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "CSNQueriesIncomplete",
		},
		{
			// Nothing was asked at all — no databases, no pods. There is
			// genuinely nothing to report.
			name:       "no readings at all writes no condition",
			readings:   nil,
			replicas:   3,
			wantSilent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateLocalConvergence(tt.readings, tt.replicas)
			if got.Silent != tt.wantSilent {
				t.Fatalf("Silent = %v, want %v (message %q)", got.Silent, tt.wantSilent, got.Message)
			}
			if tt.wantSilent {
				return
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (reason %q, message %q)",
					got.Status, tt.wantStatus, got.Reason, got.Message)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q (message %q)", got.Reason, tt.wantReason, got.Message)
			}
			for _, want := range tt.wantMsgHas {
				if !strings.Contains(got.Message, want) {
					t.Errorf("Message = %q, want it to mention %q", got.Message, want)
				}
			}
		})
	}
}

// TestEvaluateLocalConvergenceNewestBySuffix pins the cross-site baseline: the
// newest CSN is tracked PER DATABASE, because a peer's db2 must be compared
// against our db2 and never against our db1.
func TestEvaluateLocalConvergenceNewestBySuffix(t *testing.T) {
	got := evaluateLocalConvergence([]csnReading{
		{Pod: "slapd-0", Suffix: sfxA, CSNs: csnsA},
		{Pod: "slapd-0", Suffix: sfxB, CSNs: csnsB},
	}, 1)

	a, ok := got.NewestBySuffix[sfxA]
	if !ok {
		t.Fatalf("no newest timestamp for %s", sfxA)
	}
	b, ok := got.NewestBySuffix[sfxB]
	if !ok {
		t.Fatalf("no newest timestamp for %s", sfxB)
	}
	if !a.After(b) {
		t.Errorf("expected %s newest (%s) after %s newest (%s) — the fixtures' whole point",
			sfxA, a, sfxB, b)
	}
	if len(got.NewestBySuffix) != 2 {
		t.Errorf("NewestBySuffix has %d entries, want one per database", len(got.NewestBySuffix))
	}
}

func TestEvaluatePeerConvergence(t *testing.T) {
	// Local baseline: db1 is newer than db2 by minutes — the gap that must
	// never be mistaken for replication lag. This is the DEFAULT for cases that
	// do not set their own: both databases known, both non-zero.
	//
	// A case sets `localNewest` itself only to vary the LOCAL side — which the
	// zero-baseline cases below must, because the defect they pin lives in what
	// happens when our own reading for a database is missing.
	defaultLocalNewest := map[string]time.Time{
		sfxA: mustCSNTime(t, csnsA),
		sfxB: mustCSNTime(t, csnsB),
	}

	tests := []struct {
		name string
		// localNewest overrides the shared baseline; nil means use it.
		localNewest map[string]time.Time
		readings    []csnReading
		wantState   ldapv1alpha1.ExternalPeerReplicationState
		wantLag     string
		wantErrHas  []string
		wantNoError bool
		wantNoLag   bool
		// because states the RULE, so a failure explains itself rather than
		// reporting that two strings differ (pods_responsive_test.go's idiom).
		because string
	}{
		{
			// A peer that is current on BOTH databases. The cross-database age
			// gap (db1 newer than db2 by minutes) must not register as lag,
			// because each database is compared against its own counterpart.
			name: "peer current on every database is Synced",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: csnsB},
			},
			wantState:   ldapv1alpha1.ReplicationSynced,
			wantNoError: true,
		},
		{
			// THE db2 INCIDENT SHAPE (2026-09-13): db1 binds and answers, db2's
			// bind fails err=49 on the same host. Computing the verdict from
			// whichever database answered reports Synced while that peer's
			// second database is dead — evidence that degrades to empty on a
			// read failure. Some databases verified, some not: PartiallyVerified.
			name: "a database with no readable evidence is not Synced",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.1", Suffix: sfxB, Err: "bind cn=replication: LDAP Result Code 49"},
			},
			wantState:  ldapv1alpha1.ReplicationPartiallyVerified,
			wantErrHas: []string{sfxB},
		},
		{
			// Observed problems outrank unverifiability: a measured lag on a
			// verified database is reported as Lagging even when another
			// database could not be read at all.
			name: "measured lag outranks an unverifiable database",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsAOlder},
				{Pod: "10.0.0.1", Suffix: sfxB, Err: "bind cn=replication: LDAP Result Code 49"},
			},
			wantState:  ldapv1alpha1.ReplicationLagging,
			wantLag:    "146.7",
			wantErrHas: []string{sfxB},
		},
		{
			// Lag is measured WITHIN a database. Here only db2 answered, and it
			// is exactly current. Comparing our db1's newest (minutes ahead, a
			// different database) against the peer's db2 manufactures a lag of
			// minutes out of two healthy databases — the cross-database mistake
			// in its peer-side form.
			name: "lag is measured per database, never across databases",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, Err: "bind cn=replication: LDAP Result Code 49"},
				{Pod: "10.0.0.2", Suffix: sfxB, CSNs: csnsB},
			},
			wantState:  ldapv1alpha1.ReplicationPartiallyVerified,
			wantErrHas: []string{sfxA},
		},
		{
			// Nothing answered anywhere: Unreachable, unchanged.
			name: "no readable evidence at all is Unreachable",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, Err: "dial: i/o timeout"},
				{Pod: "10.0.0.1", Suffix: sfxB, Err: "dial: i/o timeout"},
			},
			wantState:  ldapv1alpha1.ReplicationUnreachable,
			wantErrHas: []string{"queries failed"},
		},
		{
			// One address failing while another answers for the same database
			// is not a gap in evidence: the database IS verified.
			name: "one address failing does not make a verified database unverified",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, Err: "dial: i/o timeout"},
				{Pod: "10.0.0.2", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.2", Suffix: sfxB, CSNs: csnsB},
			},
			wantState:   ldapv1alpha1.ReplicationSynced,
			wantNoError: true,
		},
		{
			// The peer answered but its database is empty while ours has data:
			// it is behind, not merely unverified.
			name: "an empty remote database is Lagging",
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: nil},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: csnsB},
			},
			wantState: ldapv1alpha1.ReplicationLagging,
			because: "a remote-side zero with a known local value is a measured " +
				"deficit, not missing evidence — the one direction that stays Lagging",
		},
		{
			// THE ZERO-LOCAL-BASELINE DEFECT (measured 2026-10-06). `localNewest`
			// is built from readings that parsed, so a database whose every LOCAL
			// read failed has NO ENTRY at all. Subtracting the peer's fresh CSN
			// from that zero time yields a hugely negative lag, the remote-ahead
			// clamp turns it into zero, the database counts as verified (the
			// remote did answer) — and the peer reads Synced for a database we
			// never managed to read on our own side. Confirmed by execution
			// against this tree: verdict state="Synced" lag="" lastError="".
			//
			// ADR-008's rule is the same one in both directions: unreadable
			// evidence never counts toward the good verdict. A missing local
			// baseline is missing evidence, so the database is UNVERIFIED and
			// must be named — not silently scored as current.
			name:        "a database with no local baseline is not Synced",
			localNewest: map[string]time.Time{sfxA: mustCSNTime(t, csnsA)},
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: csnsB},
			},
			wantState:  ldapv1alpha1.ReplicationPartiallyVerified,
			wantErrHas: []string{sfxB},
			wantNoLag:  true,
			because: "without our own contextCSN for that database there is no " +
				"comparison basis, so neither Synced nor a lag figure can be earned",
		},
		{
			// Same rule, the other input shape: an entry that EXISTS and is the
			// zero time. Nothing in the current pipeline produces this — the map
			// is only ever written from a parsed CSN — but the rule is about the
			// VALUE, not about the map's shape, so a later refactor that
			// pre-seeds a key per database must not reopen the hole. IsZero()
			// covers both; a `_, ok :=` lookup would cover only the one above.
			name: "a zero local baseline is not Synced even when the key exists",
			localNewest: map[string]time.Time{
				sfxA: mustCSNTime(t, csnsA),
				sfxB: {},
			},
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: csnsB},
			},
			wantState:  ldapv1alpha1.ReplicationPartiallyVerified,
			wantErrHas: []string{sfxB},
			wantNoLag:  true,
			because: "absent and zero are the same absence of a baseline; the " +
				"check belongs on the value, not on the key",
		},
		{
			// Neither side has a baseline: no local reading for db2 AND the peer
			// answered for it with nothing. Today this falls through both zero
			// branches and lands on Synced — the worst of the three, because it
			// is the shape a never-read database takes on both sides at once.
			// It is also not Lagging: a deficit needs a known local value to be
			// measured against, which is exactly what is missing.
			name:        "a database empty on both sides is unverified, not Synced",
			localNewest: map[string]time.Time{sfxA: mustCSNTime(t, csnsA)},
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: csnsA},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: nil},
			},
			wantState:  ldapv1alpha1.ReplicationPartiallyVerified,
			wantErrHas: []string{sfxB},
			wantNoLag:  true,
			because: "two unknowns do not make an agreement; withholding the " +
				"verdict is safe, claiming health we cannot see is not",
		},
		{
			// THE ASYMMETRY GUARD. The rule above is one-sided ON PURPOSE, and
			// "zero on either side means unverified" is the natural wrong
			// generalisation of it:
			//
			//   zero LOCAL baseline, remote readable → unverified (the new rule)
			//   zero REMOTE value,   local known     → Lagging   (unchanged)
			//
			// The second is not missing evidence. We read our side, we read
			// theirs, and theirs is empty while ours has data: that is a peer
			// that has not received the database, which is the single most
			// actionable cross-site fault there is. Downgrading it to
			// PartiallyVerified loses it.
			//
			// This restates "an empty remote database is Lagging" above with the
			// asymmetry named, so an implementation that collapses the two
			// directions fails with a message that says what it broke.
			name:        "a readable local baseline against an empty peer stays Lagging",
			localNewest: defaultLocalNewest,
			readings: []csnReading{
				{Pod: "10.0.0.1", Suffix: sfxA, CSNs: nil},
				{Pod: "10.0.0.1", Suffix: sfxB, CSNs: csnsB},
			},
			wantState: ldapv1alpha1.ReplicationLagging,
			because: "the zero-baseline rule is about OUR side only; a zero on " +
				"the PEER's side with a known local value must remain Lagging",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localNewest := tt.localNewest
			if localNewest == nil {
				localNewest = defaultLocalNewest
			}
			got := evaluatePeerConvergence(localNewest, tt.readings, csnSyncThreshold)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (lag %q, err %q)%s",
					got.State, tt.wantState, got.LagSeconds, got.LastError,
					becauseTail(tt.because))
			}
			if tt.wantLag != "" && got.LagSeconds != tt.wantLag {
				t.Errorf("LagSeconds = %q, want %q", got.LagSeconds, tt.wantLag)
			}
			if tt.wantNoLag && got.LagSeconds != "" {
				t.Errorf("LagSeconds = %q, want no lag figure at all (state %q)%s",
					got.LagSeconds, got.State, becauseTail(tt.because))
			}
			if tt.wantNoError && got.LastError != "" {
				t.Errorf("LastError = %q, want empty", got.LastError)
			}
			for _, want := range tt.wantErrHas {
				if !strings.Contains(got.LastError, want) {
					t.Errorf("LastError = %q, want it to mention %q%s",
						got.LastError, want, becauseTail(tt.because))
				}
			}
		})
	}
}

// becauseTail renders a case's rule statement as a tail on a failure message,
// so the output says WHY the expectation is what it is.
func becauseTail(because string) string {
	if because == "" {
		return ""
	}
	return "\n  rule: " + because
}

func mustCSNTime(t *testing.T, csns []string) time.Time {
	t.Helper()
	ts, _, err := newestCSN(csns)
	if err != nil {
		t.Fatalf("newestCSN(%v): %v", csns, err)
	}
	return ts
}
