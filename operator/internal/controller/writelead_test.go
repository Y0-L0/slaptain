/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The lead time (ADR-037, spec.rotation.leadTime).
//
// What it is for, measured on a two-provider cluster: with rotation on and the
// writes following the endpoint, throughput fell 2.8 % against the same
// rotation with the writes pinned, and 99.9 % of that loss landed in the first
// 30 s after a slot boundary — 69 % of it inside the client's 20 s connection
// lifetime. Staggering the purges was free. Moving the writes cost, and it cost
// only at the handover: the endpoint moved AT the boundary, so the pod that
// stopped taking writes began purging at the same instant and every connection
// that had not yet aged out delivered into an active purge.
//
// The lead moves the endpoint d BEFORE the boundary, so with d above the
// client's connection lifetime no connection still points at the old writer
// when it starts purging.
//
// Two properties carry the whole feature and both are pinned here:
//
//  1. with no lead, EVERY answer is the one the code gave before this field
//     existed — byte-identically, including the condition text. The field is
//     opt-in and an operator upgrade must change nothing;
//  2. with a lead d, the writer at T−d is the writer the unleaded code would
//     have chosen at T. That is the lookahead, and a no-op lookahead is
//     indistinguishable from the feature working unless something asserts it.

// leadGrid is the slot length every case here is built on. Whole seconds,
// because the grid slapd reads counts in them.
const leadGrid = 100 * time.Second

// leadBoundary is an instant that is exactly a slot boundary of leadGrid and
// also exactly a period boundary for n in {2, 3, 4, 5}: the Unix epoch plus a
// whole number of 60 × leadGrid.
func leadBoundary() time.Time {
	return time.Unix(6000*7, 0).UTC()
}

// TestWriterSlotWithoutALeadIsTodaysAnswer is the compatibility guarantee for
// the lookahead. Every instant across two whole periods, sampled finely enough
// to cross each boundary from both sides, must give exactly rotationSlot.
func TestWriterSlotWithoutALeadIsTodaysAnswer(t *testing.T) {
	base := leadBoundary()
	for _, n := range []int32{1, 2, 3, 5} {
		for off := -3 * time.Second; off < 2*time.Duration(n)*leadGrid; off += 7 * time.Second {
			now := base.Add(off)
			got := writerSlot(now, 0, leadGrid, n)
			want := rotationSlot(now, leadGrid, n)
			if got != want {
				t.Fatalf("writerSlot(%s, lead=0, t=%s, n=%d) = %d, want rotationSlot's %d: a "+
					"zero lead must be the behaviour that shipped before leadTime existed",
					now.UTC().Format(time.RFC3339), leadGrid, n, got, want)
			}
		}
	}
}

// TestWriterSlotLooksAhead is the feature. At T−d the endpoint must already be
// steering by the slot that becomes current at T.
func TestWriterSlotLooksAhead(t *testing.T) {
	base := leadBoundary()
	for _, n := range []int32{2, 3, 4, 5} {
		for k := range 2 * n {
			boundary := base.Add(time.Duration(k) * leadGrid)
			for _, lead := range []time.Duration{1 * time.Second, 20 * time.Second, 99 * time.Second} {
				now := boundary.Add(-lead)
				got := writerSlot(now, lead, leadGrid, n)
				want := rotationSlot(boundary, leadGrid, n)
				if got != want {
					t.Fatalf("n=%d lead=%s: writerSlot at %s (the boundary %s minus the lead) = "+
						"%d, want %d — the slot that becomes current AT the boundary. A lookahead "+
						"that does not look ahead leaves the endpoint moving at the boundary, "+
						"which is the handover this field exists to move off",
						n, lead, now.UTC().Format(time.RFC3339),
						boundary.UTC().Format(time.RFC3339), got, want)
				}
			}
		}
	}
}

// TestWriterSlotLeadIsNeverBehind pins the direction: a lead may only ever move
// the answer FORWARD on the grid, never back. An implementation that subtracted
// the lead would pass TestWriterSlotWithoutALeadIsTodaysAnswer and read as a
// working rotation while steering writes onto the pod that just purged.
func TestWriterSlotLeadIsNeverBehind(t *testing.T) {
	base := leadBoundary()
	const n = int32(4)
	const lead = 30 * time.Second
	for off := time.Duration(0); off < 4*time.Duration(n)*leadGrid; off += 11 * time.Second {
		now := base.Add(off)
		got := writerSlot(now, lead, leadGrid, n)
		here := rotationSlot(now, leadGrid, n)
		ahead := rotationSlot(now.Add(lead), leadGrid, n)
		if got != ahead {
			t.Fatalf("writerSlot at %s with lead %s = %d; the slot at now+lead is %d and the slot "+
				"at now is %d — the lead must read the grid FORWARD",
				now.UTC().Format(time.RFC3339), lead, got, ahead, here)
		}
	}
}

// TestNextWriterMoveWithoutALeadIsTodaysAnswer is the requeue half of the
// compatibility guarantee.
func TestNextWriterMoveWithoutALeadIsTodaysAnswer(t *testing.T) {
	base := leadBoundary()
	for off := -5 * time.Second; off < 3*leadGrid; off += 3 * time.Second {
		now := base.Add(off)
		if got, want := nextWriterMove(now, 0, leadGrid), nextSlotBoundary(now, leadGrid); got != want {
			t.Fatalf("nextWriterMove(%s, lead=0, t=%s) = %s, want nextSlotBoundary's %s",
				now.UTC().Format(time.RFC3339), leadGrid, got, want)
		}
	}
}

// TestNextWriterMoveFiresAtBoundaryMinusLead: the requeue must land on the
// instant the WRITER changes, which is one lead before the purge boundary.
func TestNextWriterMoveFiresAtBoundaryMinusLead(t *testing.T) {
	base := leadBoundary()
	for _, lead := range []time.Duration{0, 1 * time.Second, 20 * time.Second, 97 * time.Second} {
		for off := time.Duration(0); off < 3*leadGrid; off += time.Second {
			now := base.Add(off)
			move := now.Add(nextWriterMove(now, lead, leadGrid))
			// The instant the writer changes is boundary − lead, so adding the
			// lead back must land exactly on a grid boundary.
			boundary := move.Add(lead)
			if rem := boundary.Sub(base) % leadGrid; rem != 0 {
				t.Fatalf("lead=%s now=%s: the next writer move is %s, which is %s off the grid — "+
					"the endpoint must move exactly one lead before a slot boundary",
					lead, now.UTC().Format(time.RFC3339), move.UTC().Format(time.RFC3339), rem)
			}
			if !move.After(now) {
				t.Fatalf("lead=%s now=%s: the next writer move is %s, which is not in the future",
					lead, now.UTC().Format(time.RFC3339), move.UTC().Format(time.RFC3339))
			}
		}
	}
}

// TestNextWriterMoveIsAlwaysPositive. A non-positive requeue is a hot loop
// against the API server, and the lead is a new way to produce one.
func TestNextWriterMoveIsAlwaysPositive(t *testing.T) {
	base := leadBoundary()
	leads := []time.Duration{-time.Hour, 0, time.Nanosecond, leadGrid - time.Nanosecond,
		leadGrid, 3 * leadGrid, 1000 * time.Hour}
	slots := []time.Duration{-time.Second, 0, time.Millisecond, time.Second, leadGrid, 6 * time.Hour}
	for _, lead := range leads {
		for _, slot := range slots {
			for _, off := range []time.Duration{0, time.Nanosecond, 37 * time.Second, -time.Nanosecond} {
				now := base.Add(off)
				if got := nextWriterMove(now, lead, slot); got <= 0 {
					t.Fatalf("nextWriterMove(now=%s, lead=%s, t=%s) = %s: a non-positive requeue "+
						"is a hot loop", now.UTC().Format(time.RFC3339), lead, slot, got)
				}
			}
		}
	}
}

// leadCluster is a rotating cluster with n replicas, slot length leadGrid and
// the given lead (nil for an unset field).
func leadCluster(n int32, lead *time.Duration) *ldapv1alpha1.SlapdCluster {
	t := leadGrid
	sc := &ldapv1alpha1.SlapdCluster{}
	sc.Name = testClusterName
	sc.Namespace = testNamespace
	sc.Spec.Replicas = n
	on := true
	sc.Spec.Rotation.Enabled = &on
	sc.Spec.Rotation.SlotLength = &metav1.Duration{Duration: t}
	if lead != nil {
		sc.Spec.Rotation.LeadTime = &metav1.Duration{Duration: *lead}
	}
	return sc
}

// TestEvaluateRotationUnsetLeadIsByteIdenticalToZero: an absent field and an
// explicit "0s" must produce the same plan AND the same condition text as each
// other, and the message must not mention a lead at all. That is the
// compatibility guarantee at the gate.
func TestEvaluateRotationUnsetLeadIsByteIdenticalToZero(t *testing.T) {
	zero := time.Duration(0)
	unset := evaluateRotation(leadCluster(3, nil))
	explicit := evaluateRotation(leadCluster(3, &zero))

	if unset != explicit {
		t.Fatalf("an unset leadTime planned %+v and an explicit 0s planned %+v; they must be the "+
			"same plan", unset, explicit)
	}
	if unset.LeadTime != 0 {
		t.Fatalf("an unset leadTime resolved to %s, want 0 — unset must be the behaviour that "+
			"shipped before the field existed", unset.LeadTime)
	}
	if strings.Contains(strings.ToLower(unset.Message), "lead") {
		t.Fatalf("the RotationConfigured message mentions a lead with none in force:\n%s",
			unset.Message)
	}
}

// TestEvaluateRotationCarriesTheLead: a usable lead reaches the plan and is
// named in the condition.
func TestEvaluateRotationCarriesTheLead(t *testing.T) {
	lead := 20 * time.Second
	plan := evaluateRotation(leadCluster(3, &lead))

	if plan.Status != metav1.ConditionTrue || plan.Reason != reasonRotationActive {
		t.Fatalf("a lead of %s inside the bound was refused: %s/%s — %s",
			lead, plan.Status, plan.Reason, plan.Message)
	}
	if plan.LeadTime != lead {
		t.Fatalf("plan.LeadTime = %s, want %s", plan.LeadTime, lead)
	}
	if !strings.Contains(plan.Message, lead.String()) {
		t.Fatalf("the RotationConfigured message does not report the lead in force (%s):\n%s",
			lead, plan.Message)
	}
}

// TestEvaluateRotationRefusesALeadAtOrBeyondTheBound. (n−1)·t is the instant
// the incoming writer started purging; a lead reaching back to it or past it
// hands writes to a pod that will purge during its own write slot.
func TestEvaluateRotationRefusesALeadAtOrBeyondTheBound(t *testing.T) {
	cases := []struct {
		name    string
		n       int32
		lead    time.Duration
		refused bool
	}{
		{"just inside the bound", 3, 2*leadGrid - time.Second, false},
		{"exactly the bound", 3, 2 * leadGrid, true},
		{"beyond the bound", 3, 3 * leadGrid, true},
		{"two pods, inside", 2, leadGrid - time.Second, false},
		{"two pods, at the bound", 2, leadGrid, true},
		{"one pod, zero lead is fine", 1, 0, false},
		{"one pod, any lead is nonsense", 1, time.Second, true},
		{"negative", 3, -time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lead := tc.lead
			plan := evaluateRotation(leadCluster(tc.n, &lead))
			refused := plan.Status == metav1.ConditionFalse
			if refused != tc.refused {
				t.Fatalf("n=%d lead=%s: refused=%v, want %v (%s/%s — %s)",
					tc.n, tc.lead, refused, tc.refused, plan.Status, plan.Reason, plan.Message)
			}
			if !tc.refused {
				return
			}
			if plan.Reason != reasonLeadTimeOutOfRange {
				t.Fatalf("n=%d lead=%s was refused as %s, want %s",
					tc.n, tc.lead, plan.Reason, reasonLeadTimeOutOfRange)
			}
			// Withheld, not refused: the rotation stops, the cluster keeps a
			// usable purge interval, and nothing about the apply failed.
			if plan.PhasesActive || plan.WriterActive {
				t.Fatalf("n=%d lead=%s: the rotation is still active after being refused "+
					"(phases=%v writer=%v)", tc.n, tc.lead, plan.PhasesActive, plan.WriterActive)
			}
			if plan.Interval <= 0 {
				t.Fatalf("n=%d lead=%s: the withheld plan left interval %s — a cluster whose "+
					"schedule could not be read must still purge", tc.n, tc.lead, plan.Interval)
			}
			if !strings.Contains(plan.Message, "leadTime") {
				t.Fatalf("the refusal does not name the field:\n%s", plan.Message)
			}
		})
	}
}

// TestWriteRotationConditionWithoutALeadIsTodaysText pins the exact string the
// condition carried before leadTime existed. A lead note leaking into the
// unleaded case is the compatibility break this asserts against.
func TestWriteRotationConditionWithoutALeadIsTodaysText(t *testing.T) {
	_, sc := writeFixture()
	next := time.Unix(1_800_000_000, 0).UTC()

	cond := writeRotationCondition(sc, 2, true, next, 0)
	want := "the write endpoint selects " + testClusterName + "-2; next rotation at " +
		next.Format(time.RFC3339)
	if cond.Message != want {
		t.Fatalf("with no lead the condition message must be byte-identical to the text that\n"+
			"shipped before leadTime existed.\n got: %s\nwant: %s", cond.Message, want)
	}
}

// TestWriteRotationConditionReportsTheLead: with a lead the condition names it,
// and the instant it reports is the ENDPOINT MOVE, not the purge boundary.
func TestWriteRotationConditionReportsTheLead(t *testing.T) {
	_, sc := writeFixture()
	next := time.Unix(1_800_000_000, 0).UTC()
	lead := 20 * time.Second

	cond := writeRotationCondition(sc, 2, true, next, lead)
	if !strings.Contains(cond.Message, lead.String()) {
		t.Fatalf("the condition does not report the lead in force (%s):\n%s", lead, cond.Message)
	}
	if !strings.Contains(cond.Message, "endpoint move") {
		t.Fatalf("the condition must name the instant as an endpoint MOVE, not a purge "+
			"boundary:\n%s", cond.Message)
	}
	if !strings.Contains(cond.Message, next.Format(time.RFC3339)) {
		t.Fatalf("the condition does not carry the next endpoint move instant:\n%s", cond.Message)
	}
}
