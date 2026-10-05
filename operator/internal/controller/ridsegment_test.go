/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// ADR-031 decision 3: the segment is DECLARED, and the operator's whole job is
// to refuse a bad one. These tests pin the refusal, not an allocation — there
// is no ledger, no release protocol and no stored state to read back, because
// decision 7 keeps everything about a RID out of status.
//
// Decision 6 is the stake: a refusal withholds the pod's entire stanza set.
// olcSyncRepl is written with one atomic Replace, so "converge toward nothing"
// is a deletion of live replication. A wrong verdict here is not a cosmetic
// condition; it is either a silent RID collision or a torn-down mesh.

const (
	testRIDCluster = "slapd"
	testRIDNS      = "slaptain-testing"
)

func i32(v int32) *int32 { return &v }

func boolp(v bool) *bool { return &v }

// ridDB builds a SlapdDatabase declaring `segment` (nil for "not declared")
// against cluster `clusterRef`, with replication enabled.
func ridDB(name, clusterRef string, segment *int32) ldapv1alpha1.SlapdDatabase {
	return ldapv1alpha1.SlapdDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testRIDNS},
		Spec: ldapv1alpha1.SlapdDatabaseSpec{
			ClusterRef: clusterRef,
			Replication: ldapv1alpha1.DatabaseReplicationConfig{
				Enabled:    boolp(true),
				RIDSegment: segment,
			},
		},
	}
}

// ridDBNotReplicated is the same, with replication explicitly off. ADR-031
// decision 3's reuse argument rests on a deleted database's stanzas being GONE
// from the pods — a flag flipped to false removes nothing, so such a database
// still owns its segment.
func ridDBNotReplicated(name, clusterRef string, segment *int32) ldapv1alpha1.SlapdDatabase {
	db := ridDB(name, clusterRef, segment)
	db.Spec.Replication.Enabled = boolp(false)
	return db
}

// narrowLayout admits exactly one segment: M = 800, C = floor(999/800) = 1.
// It exists so a test can hand the judge an input that is individually
// plausible and only becomes wrong once the layout is applied.
func narrowLayout() ridcoord.Layout {
	return ridcoord.Layout{PodsPerSite: 400, MaxSites: 400}
}

// wideLayout is the loosest the API bounds admit: M = 2, C = 499.
func wideLayout() ridcoord.Layout {
	return ridcoord.Layout{PodsPerSite: 1, MaxSites: 1}
}

func TestJudgeRIDSegment(t *testing.T) {
	def := ridcoord.DefaultLayout() // P=8, S=12 → M=20, C=49

	for _, tc := range []struct {
		name        string
		db          ldapv1alpha1.SlapdDatabase
		siblings    []ldapv1alpha1.SlapdDatabase
		layout      ridcoord.Layout
		wantOK      bool
		wantReason  string
		wantSegment int32
		wantInMsg   []string
		because     string
	}{
		{
			name:        "a distinct in-range segment converges, carrying the declared number",
			db:          ridDB("default", testRIDCluster, i32(3)),
			siblings:    []ldapv1alpha1.SlapdDatabase{ridDB("other", testRIDCluster, i32(0)), ridDB("third", testRIDCluster, i32(1))},
			layout:      def,
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 3,
			because:     "ADR-031 d3: the operator validates and then uses the number the user declared",
		},
		{
			name:        "segment zero is a legal coordinate, not an unset field",
			db:          ridDB("default", testRIDCluster, i32(0)),
			siblings:    []ldapv1alpha1.SlapdDatabase{ridDB("other", testRIDCluster, i32(1))},
			layout:      def,
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 0,
			because:     "ridBase 0 migrates to ridSegment 0 (d8); rid=0 is reserved, segment 0 is not",
		},
		{
			name:       "a collision refuses the database that asks first",
			db:         ridDB("a", testRIDCluster, i32(7)),
			siblings:   []ldapv1alpha1.SlapdDatabase{ridDB("b", testRIDCluster, i32(7))},
			layout:     def,
			wantReason: reasonRIDSegmentCollision,
			wantInMsg:  []string{"b"},
			because:    "d3: a refusal names the other database so a human can pick which one moves",
		},
		{
			name:       "a collision refuses the database that asks second, symmetrically",
			db:         ridDB("b", testRIDCluster, i32(7)),
			siblings:   []ldapv1alpha1.SlapdDatabase{ridDB("a", testRIDCluster, i32(7))},
			layout:     def,
			wantReason: reasonRIDSegmentCollision,
			wantInMsg:  []string{"a"},
			because:    "whoever reconciles first must not win: the loser's stanzas are already on the pods (d6)",
		},
		{
			name:       "a sibling with replication disabled still owns its segment",
			db:         ridDB("a", testRIDCluster, i32(2)),
			siblings:   []ldapv1alpha1.SlapdDatabase{ridDBNotReplicated("b", testRIDCluster, i32(2))},
			layout:     def,
			wantReason: reasonRIDSegmentCollision,
			wantInMsg:  []string{"b"},
			because:    "d3 ties reuse to stanzas being GONE; flipping a flag removes nothing from cn=config",
		},
		{
			name:        "a sibling of another cluster is not a collision",
			db:          ridDB("a", testRIDCluster, i32(5)),
			siblings:    []ldapv1alpha1.SlapdDatabase{ridDB("b", "other-cluster", i32(5))},
			layout:      def,
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 5,
			because:     "a RID is unique per SERVER; two clusters share no pod",
		},
		{
			name:        "the database itself appearing among the siblings is not a collision with itself",
			db:          ridDB("a", testRIDCluster, i32(5)),
			siblings:    []ldapv1alpha1.SlapdDatabase{ridDB("a", testRIDCluster, i32(5)), ridDB("b", testRIDCluster, i32(6))},
			layout:      def,
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 5,
			because:     "the caller hands over a plain List; assuming it was pre-filtered stalls every database forever",
		},
		{
			name:        "a sibling that declares no segment collides with nothing, not with segment zero",
			db:          ridDB("a", testRIDCluster, i32(0)),
			siblings:    []ldapv1alpha1.SlapdDatabase{ridDB("b", testRIDCluster, nil)},
			layout:      def,
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 0,
			because:     "nil is absence; reading it as the int32 zero value makes every undeclared database collide with segment 0",
		},
		{
			name:       "an undeclared segment is refused as missing, never defaulted to zero",
			db:         ridDB("a", testRIDCluster, nil),
			siblings:   nil,
			layout:     def,
			wantReason: reasonRIDSegmentMissing,
			wantInMsg:  []string{"ridSegment"},
			because:    "d3 + d8: reachable past CEL via an object predating the field or an apply with validation off",
		},
		{
			name:       "a segment AT the layout's ceiling is out of range",
			db:         ridDB("a", testRIDCluster, i32(49)),
			siblings:   nil,
			layout:     def,
			wantReason: reasonRIDSegmentOutOfRange,
			wantInMsg:  []string{"48"},
			because:    "d5: C = floor(999/M) = 49 segments, so 0..48; 49 would walk rid past 999",
		},
		{
			name:       "a segment above the ceiling is out of range",
			db:         ridDB("a", testRIDCluster, i32(100)),
			siblings:   nil,
			layout:     def,
			wantReason: reasonRIDSegmentOutOfRange,
			wantInMsg:  []string{"48"},
			because:    "the bound is the layout's C, not the CRD's Maximum=499",
		},
		{
			name:       "a negative segment is out of range",
			db:         ridDB("a", testRIDCluster, i32(-1)),
			siblings:   nil,
			layout:     def,
			wantReason: reasonRIDSegmentOutOfRange,
			wantInMsg:  []string{"-1"},
			because:    "a negative segment produces a rid below the reserved 0, which decodes as somebody else's",
		},
		{
			// The boundary lesson, paid for twice: an input that is
			// individually plausible and only becomes wrong once the layout is
			// applied. 60 is a perfectly good segment at P=S=1 and is off the
			// end at the defaults.
			name:        "a segment legal under a wide layout is accepted under that layout",
			db:          ridDB("a", testRIDCluster, i32(60)),
			siblings:    nil,
			layout:      wideLayout(),
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 60,
			because:     "C = floor(999/2) = 499 at P=S=1; the ceiling is the layout's, not a constant",
		},
		{
			name:       "that same segment is out of range under the default layout",
			db:         ridDB("a", testRIDCluster, i32(60)),
			siblings:   nil,
			layout:     def,
			wantReason: reasonRIDSegmentOutOfRange,
			wantInMsg:  []string{"48"},
			because:    "a mesh-wide layout change is exactly this: the same CR, a different verdict (d5)",
		},
		{
			name:        "the only segment a one-segment layout admits is accepted",
			db:          ridDB("a", testRIDCluster, i32(0)),
			siblings:    nil,
			layout:      narrowLayout(),
			wantOK:      true,
			wantReason:  reasonRIDSegmentOK,
			wantSegment: 0,
			because:     "M = 800 leaves C = 1; a layout with one usable segment is legal, not an error",
		},
		{
			name:       "a plausible segment is out of range once a narrow layout is applied",
			db:         ridDB("a", testRIDCluster, i32(30)),
			siblings:   nil,
			layout:     narrowLayout(),
			wantReason: reasonRIDSegmentOutOfRange,
			wantInMsg:  []string{"30"},
			because:    "30 is legal at the defaults and off the end at C = 1 — the check must apply the layout, not a constant",
		},
		{
			// Ordering, not just classification: the layout is judged FIRST.
			// This database's segment is also out of range AND colliding, so a
			// judge that checked those first would answer with the wrong
			// reason and send a human to move a number that is not the
			// problem.
			name:       "an unusable layout is judged before the segment, even when the segment is also wrong",
			db:         ridDB("a", testRIDCluster, i32(99)),
			siblings:   []ldapv1alpha1.SlapdDatabase{ridDB("b", testRIDCluster, i32(99))},
			layout:     ridcoord.Layout{},
			wantReason: reasonRIDLayoutInvalid,
			because:    "d5 + ridcoord: a zero Layout reaches the arithmetic; no segment is in range while no segment exists",
		},
		{
			name:       "a layout with one zero axis is unusable",
			db:         ridDB("a", testRIDCluster, i32(0)),
			siblings:   nil,
			layout:     ridcoord.Layout{PodsPerSite: 8},
			wantReason: reasonRIDLayoutInvalid,
			because:    "S = 0 leaves no remote block at all; ridcoord.Validate refuses rather than dividing",
		},
		{
			// Pins WHICH cluster scopes the sibling set: sd's own clusterRef,
			// the only cluster name the judgement is given. The databases that
			// can collide with sd are the ones that share a pod with it, and
			// clusterRef is what names those.
			name:       "a sibling naming the same clusterRef collides; clusterRef is the scope",
			db:         ridDB("a", "a-third-cluster", i32(4)),
			siblings:   []ldapv1alpha1.SlapdDatabase{ridDB("b", "a-third-cluster", i32(4)), ridDB("c", testRIDCluster, i32(4))},
			layout:     def,
			wantReason: reasonRIDSegmentCollision,
			wantInMsg:  []string{"b"},
			because:    "a RID collides on the pods the two databases share, which clusterRef names",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.db
			got := judgeRIDSegment(&db, tc.siblings, tc.layout)

			if got.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q, message %q) — %s", got.OK, tc.wantOK, got.Reason, got.Message, tc.because)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q — %s", got.Reason, tc.wantReason, tc.because)
			}
			if tc.wantOK && got.Segment != tc.wantSegment {
				t.Errorf("Segment = %d, want %d — a verdict that converges must carry the coordinate the stanza builder will use", got.Segment, tc.wantSegment)
			}
			if !tc.wantOK && got.Message == "" {
				t.Errorf("a refusal carries an empty Message — the condition is the only place a human learns what to change")
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(got.Message, want) {
					t.Errorf("Message %q does not contain %q — %s", got.Message, want, tc.because)
				}
			}
		})
	}
}

// TestJudgeRIDSegmentCeilingTracksTheLayout pins that the out-of-range message
// quotes the ceiling the layout in force produces, rather than a hardcoded 48.
// Separate from the table because the point is the DIFFERENCE between two
// verdicts on one segment, which a per-row assertion cannot express.
func TestJudgeRIDSegmentCeilingTracksTheLayout(t *testing.T) {
	for _, layout := range []ridcoord.Layout{ridcoord.DefaultLayout(), wideLayout(), {PodsPerSite: 4, MaxSites: 6}} {
		c := layout.MaxSegments()
		db := ridDB("a", testRIDCluster, i32(c)) // exactly one past the top
		v := judgeRIDSegment(&db, nil, layout)

		if v.OK || v.Reason != reasonRIDSegmentOutOfRange {
			t.Fatalf("layout %+v: segment %d (C = %d) gave OK=%v reason=%q, want a range refusal", layout, c, c, v.OK, v.Reason)
		}
		top := strconv.Itoa(int(c - 1))
		if !strings.Contains(v.Message, top) {
			t.Errorf("layout %+v: message %q does not name the top legal segment %s — a human cannot pick a number from it", layout, v.Message, top)
		}
	}
}
