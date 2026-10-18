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
)

// Literals the purge-rotation tables repeat. The values are the ones slapd's
// log_age_parse accepts, spelled out here rather than built from the formatter
// they check.
const (
	testZeroLogAge = "00:00:00"
	// testNoRotationGrid names the cases where there is no grid to be on.
	testNoRotationGrid = "a zero slot length means there is no grid"
)

// eligibleSet builds writeTarget's predicate from an explicit list of ordinals.
// Everything the grid does not know — readiness, ADR-036's authorship gate,
// ADR-037 decision 4's promotion gate — reaches writeTarget only through this
// predicate, so the tables declare it as a set rather than as cluster state.
func eligibleSet(ordinals ...int32) func(int32) bool {
	set := make(map[int32]bool, len(ordinals))
	for _, o := range ordinals {
		set[o] = true
	}
	return func(o int32) bool { return set[o] }
}

// eligibleAll is the predicate for a fully healthy fleet.
func eligibleAll() func(int32) bool {
	return func(int32) bool { return true }
}

// eligibleNone is the predicate for a fleet with nothing to write to.
func eligibleNone() func(int32) bool {
	return func(int32) bool { return false }
}

// slapd's log_age_parse is strict in a way that fails silently from the
// operator's side: it accepts "[ddd+]hh:mm[:ss]" with TWO-DIGIT fields, so
// "00:05" parses where "0:05" is refused, and a value carrying days must carry
// minutes too. A refused olcAccessLogPurge is rejected at config load, and that
// pod then never purges at all — the exact failure ADR-037 exists to prevent,
// arriving by a route the ADR does not discuss (PURGE-ROTATION-PLAN hazard 4,
// "unit-test the formatter against known values before anything else uses it").
//
// So the formatter emits "hh:mm:ss" or "ddd+hh:mm:ss" unconditionally. The day
// count is not zero-padded, which is how the value slaptain already ships —
// defaultAccesslogPurge, "7+00:00 1+00:00" — is spelled; the three time fields
// always are.
func TestFormatLogAge(t *testing.T) {
	cases := []struct {
		name    string
		in      time.Duration
		want    string
		because string
	}{
		{
			name:    "zero is still three two-digit fields",
			in:      0,
			want:    testZeroLogAge,
			because: "pod 0's phase is zero and must be written, not omitted",
		},
		{
			name:    "five minutes pads the hours field to two digits",
			in:      5 * time.Minute,
			want:    "00:05:00",
			because: "0:05:00 is refused by log_age_parse and the pod then never purges",
		},
		{
			name:    "a single second still carries hours and minutes",
			in:      time.Second,
			want:    "00:00:01",
			because: "a bare seconds field is not a time log_age_parse recognises",
		},
		{
			name:    "the last second before a day stays in the hours field",
			in:      23*time.Hour + 59*time.Minute + 59*time.Second,
			want:    "23:59:59",
			because: "hh carries 0-23 only; 24 there would be read as an hour count slapd rejects",
		},
		{
			name:    "exactly one day rolls into the days prefix",
			in:      24 * time.Hour,
			want:    "1+00:00:00",
			because: "anything from a day up belongs in ddd+, not in an overflowing hh",
		},
		{
			name:    "the days count is not zero-padded",
			in:      7 * 24 * time.Hour,
			want:    "7+00:00:00",
			because: "the shipped default spells a week as 7+00:00, and slapd parses the day count as a number",
		},
		{
			name:    "days carry the remaining hours, minutes and seconds",
			in:      7*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
			want:    "7+03:04:05",
			because: "every field after the days prefix is still two digits",
		},
		{
			name:    "a three-digit day count is emitted whole",
			in:      100 * 24 * time.Hour,
			want:    "100+00:00:00",
			because: "ddd is a day count, not a fixed-width field to truncate",
		},
		{
			name:    "twelve hours, the phase length a production rotation expects",
			in:      12 * time.Hour,
			want:    "12:00:00",
			because: "ADR-037 expects long phases; a 6- or 12-hour slot must not acquire a days prefix",
		},
		{
			name:    "sub-second precision truncates toward zero",
			in:      1500 * time.Millisecond,
			want:    "00:00:01",
			because: "the runqueue grid is whole seconds (tv_sec); there is nothing finer to express",
		},
		{
			name:    "a negative duration clamps to zero",
			in:      -5 * time.Minute,
			want:    testZeroLogAge,
			because: "slapd refuses a leading '-', and a refused value loses the pod's whole purge configuration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatLogAge(tc.in); got != tc.want {
				t.Fatalf("formatLogAge(%s) = %q, want %q — %s", tc.in, got, tc.want, tc.because)
			}
		})
	}
}

// ADR-037's second amendment defines the grid: with n read-write pods and a
// slot length t, the period is T = n*t and pod-k purges at phase k*t. The phase
// is the per-pod half of olcAccessLogPurge, and it is the value the local
// accesslog-purge-phase patch made expressible at all — before it, every pod
// carried the same configured value and the providers purged together, which
// the ADR's third amendment measured as the worst case every deployment runs.
//
// t is fixed and T = n*t varies, never the reverse (PURGE-ROTATION-PLAN hazard
// 2). That is what makes a scale event cheap: existing phases do not move, only
// the interval does, and an interval change does not move a phase.
//
// One case below is defensive rather than live, and the choice of defined
// behaviour is the point. An ordinal at or beyond n cannot reach this function
// from the real call site — the per-pod loop runs over [0, replicas), so a pod
// above spec.replicas is never visited and keeps its existing configuration
// until it terminates. If that call site ever changes, the value must fail
// LOUDLY: ordinal*t unwrapped is a phase >= the interval, which slapd refuses
// with "<olcAccessLogPurge> phase must be less than interval", so the modify
// fails, the pod is named in the pass's failed-pod reporting, and its previous
// working value stands. Wrapping modulo n would instead put two pods in one
// slot — the synchronised purge this whole feature exists to prevent, arriving
// silently with every health signal green, which is the failure the plan's
// hazards warn about twice.
func TestPurgePhase(t *testing.T) {
	cases := []struct {
		name    string
		ordinal int32
		n       int32
		slot    time.Duration
		want    time.Duration
		because string
	}{
		{
			name:    "pod 0 purges at the start of the period",
			ordinal: 0, n: 4, slot: time.Hour,
			want:    0,
			because: "the grid is anchored at the epoch and pod 0 sits on the anchor",
		},
		{
			name:    "pod k purges k slots in",
			ordinal: 3, n: 4, slot: time.Hour,
			want:    3 * time.Hour,
			because: "the phase is ordinal*t, which is what spreads the fleet across the period",
		},
		{
			name:    "the step is the slot length, not the period",
			ordinal: 1, n: 8, slot: 15 * time.Minute,
			want:    15 * time.Minute,
			because: "deriving the step from T would re-phase everyone on every scale event",
		},
		{
			name:    "the last ordinal sits one slot short of the period",
			ordinal: 7, n: 8, slot: 15 * time.Minute,
			want:    105 * time.Minute,
			because: "slapd refuses a phase that is not strictly less than the interval",
		},
		{
			name:    "a single replica has only phase zero",
			ordinal: 0, n: 1, slot: 6 * time.Hour,
			want:    0,
			because: "with one pod there is nothing to stagger against",
		},
		{
			name:    "an ordinal beyond the declared replica count is left outside the grid",
			ordinal: 5, n: 4, slot: time.Hour,
			want:    5 * time.Hour,
			because: "the value is then >= the interval and slapd refuses it loudly, where wrapping would silently put two pods in one slot",
		},
		{
			name:    "a negative ordinal reads as pod 0",
			ordinal: -1, n: 4, slot: time.Hour,
			want:    0,
			because: "no StatefulSet produces one, and a negative phase is not a value slapd parses",
		},
		{
			name:    testNoRotationGrid,
			ordinal: 2, n: 4, slot: 0,
			want:    0,
			because: "rotation off means every pod keeps the unphased two-field value",
		},
		{
			name:    "a non-positive replica count has no grid",
			ordinal: 2, n: 0, slot: time.Hour,
			want:    0,
			because: "n is spec.replicas and a period of zero has no slots to sit in",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := purgePhase(tc.ordinal, tc.n, tc.slot)
			if got != tc.want {
				t.Fatalf("purgePhase(%d, %d, %s) = %s, want %s — %s",
					tc.ordinal, tc.n, tc.slot, got, tc.want, tc.because)
			}
		})
	}
}

// The two properties the grid stands on, over the whole range of fleet sizes
// slaptain supports (ADR-028 caps a site at 15 pods).
//
// Distinctness is the feature: two pods sharing a phase purge together, and the
// third amendment measured that configuration as indistinguishable from today.
// Staying strictly below the period is what slapd itself enforces — the patch
// refuses "<olcAccessLogPurge> phase must be less than interval" — and a pod
// whose value is refused never purges at all, which is a worse failure than the
// one the feature removes.
func TestPurgePhaseIsDistinctAndInsideThePeriod(t *testing.T) {
	const slot = 90 * time.Minute

	for n := int32(1); n <= 15; n++ {
		seen := make(map[time.Duration]int32, n)
		period := time.Duration(n) * slot
		for k := int32(0); k < n; k++ {
			phase := purgePhase(k, n, slot)
			if phase < 0 || phase >= period {
				t.Fatalf("purgePhase(%d, %d, %s) = %s, want it inside [0, %s) — "+
					"slapd refuses a phase that is not strictly less than the interval, and that pod then never purges",
					k, n, slot, phase, period)
			}
			if other, dup := seen[phase]; dup {
				t.Fatalf("purgePhase(%d, %d, %s) = %s, already taken by ordinal %d — "+
					"two pods on one phase purge together, which measures exactly like no rotation at all",
					k, n, slot, phase, other)
			}
			seen[phase] = k
		}
	}
}

// Hazard 2 of PURGE-ROTATION-PLAN, made executable: scaling up must leave every
// existing pod's phase alone and only move the interval. The tempting
// alternative — hold T fixed and recompute t = T/n — re-phases the entire fleet
// on every scale event, rewriting cn=config on every pod to achieve nothing.
func TestPurgePhaseSurvivesAScaleUp(t *testing.T) {
	const slot = 90 * time.Minute

	for n := int32(1); n <= 14; n++ {
		for k := int32(0); k < n; k++ {
			before := purgePhase(k, n, slot)
			after := purgePhase(k, n+1, slot)
			if before != after {
				t.Fatalf("purgePhase(%d, %d, %s) = %s but purgePhase(%d, %d, %s) = %s — "+
					"a scale-up moved an existing pod's phase, which rewrites the whole fleet's cn=config for nothing",
					k, n, slot, before, k, n+1, slot, after)
			}
		}
		// The pod the scale-up adds takes the slot that was the end of the
		// period, which is what keeps distinctness free.
		if got, want := purgePhase(n, n+1, slot), time.Duration(n)*slot; got != want {
			t.Fatalf("purgePhase(%d, %d, %s) = %s, want %s — the new pod takes n*t, the slot the period just grew into",
				n, n+1, slot, got, want)
		}
	}
}

// purgePhase takes no clock, and must not acquire one: the grid is the same
// whenever it is asked. The signature is the real guard — there is nothing to
// read a clock from — so this pins determinism and leaves a tripwire. If a
// time.Time ever appears in purgePhase's parameters, this test is no longer the
// right check and a real invariance spec belongs here instead.
func TestPurgePhaseIsInvariantUnderWhenYouAsk(t *testing.T) {
	const slot = 6 * time.Hour

	first := purgePhase(2, 4, slot)
	for i := range 1000 {
		if got := purgePhase(2, 4, slot); got != first {
			t.Fatalf("purgePhase(2, 4, %s) = %s on call %d but %s on the first call — "+
				"the grid must not depend on when it is asked; an absolute phase is the whole reason the patch exists",
				slot, got, i, first)
		}
	}
}

// rotationSlot reads the same absolute grid purgePhase describes, from the other
// end: which pod is purging right now. ADR-037 does not write the formula out —
// it assumes the correspondence, because the purge grid and the rotation grid
// are one grid. The anchor is the Unix epoch in seconds, matching the patched
// scheduler, which computes the next fire as ph + ((now-ph)/iv + 1)*iv from
// time(NULL). Because the epoch is midnight UTC, any interval dividing a day
// gives midnight-relative slots.
func TestRotationSlot(t *testing.T) {
	cases := []struct {
		name    string
		now     time.Time
		slot    time.Duration
		n       int32
		want    int32
		because string
	}{
		{
			name: "the epoch itself is slot 0",
			now:  time.Unix(0, 0).UTC(), slot: time.Hour, n: 4,
			want:    0,
			because: "the grid is anchored at the epoch, which is where pod 0's phase sits",
		},
		{
			name: "the first boundary advances exactly one slot",
			now:  time.Unix(3600, 0).UTC(), slot: time.Hour, n: 4,
			want:    1,
			because: "a boundary belongs to the slot it opens, not to the one it closes",
		},
		{
			name: "a moment inside a slot still names that slot",
			now:  time.Unix(3600+1799, 0).UTC(), slot: time.Hour, n: 4,
			want:    1,
			because: "the writer must not flicker between pods within one slot",
		},
		{
			name: "the last second of a slot has not advanced yet",
			now:  time.Unix(7199, 0).UTC(), slot: time.Hour, n: 4,
			want:    1,
			because: "an early advance would put the writer on the pod about to purge",
		},
		{
			name: "the period wraps back to slot 0",
			now:  time.Unix(4*3600, 0).UTC(), slot: time.Hour, n: 4,
			want:    0,
			because: "T = n*t is the period, and the grid repeats with no discontinuity at the wrap",
		},
		{
			name: "an interval dividing a day gives midnight-relative slots",
			now:  time.Date(2026, time.October, 18, 13, 37, 0, 0, time.UTC), slot: 6 * time.Hour, n: 4,
			want:    2,
			because: "the epoch is midnight UTC, so 13:37 falls in the third six-hour slot of the day",
		},
		{
			name: "a single replica is always slot 0",
			now:  time.Unix(123456789, 0).UTC(), slot: time.Hour, n: 1,
			want:    0,
			because: "one pod has one slot, and it is both the purger and the writer",
		},
		{
			name: "a time before the epoch stays on the same grid",
			now:  time.Unix(-1, 0).UTC(), slot: time.Hour, n: 4,
			want:    3,
			because: "the division is a floor, not Go's truncation; truncating would make the grid discontinuous at the epoch",
		},
		{
			name: testNoRotationGrid,
			now:  time.Unix(123456789, 0).UTC(), slot: 0, n: 4,
			want:    0,
			because: "rotation off leaves the writer where it is; there are no slots to count",
		},
		{
			name: "a slot length below one second has no grid",
			now:  time.Unix(123456789, 0).UTC(), slot: 500 * time.Millisecond, n: 4,
			want:    0,
			because: "the runqueue grid is whole seconds, so a sub-second slot is not a slot",
		},
		{
			name: "a non-positive replica count is slot 0",
			now:  time.Unix(123456789, 0).UTC(), slot: time.Hour, n: 0,
			want:    0,
			because: "n is spec.replicas; a period of zero has no slots to name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationSlot(tc.now, tc.slot, tc.n)
			if got != tc.want {
				t.Fatalf("rotationSlot(%s, %s, %d) = %d, want %d — %s",
					tc.now.UTC().Format(time.RFC3339), tc.slot, tc.n, got, tc.want, tc.because)
			}
		})
	}
}

// The grid is invariant under when you ask it: a whole period later is the same
// slot. This is the property that lets the reconciler RequeueAfter the time
// remaining to the next boundary and be exactly right, and it is the property a
// now-relative implementation ("now + interval", which is what the stock slapd
// scheduler does and what the local patch replaces) cannot have.
func TestRotationSlotAgreesAPeriodLater(t *testing.T) {
	const slot = 90 * time.Minute
	base := time.Unix(1_700_000_000, 0).UTC()

	for n := int32(1); n <= 15; n++ {
		period := time.Duration(n) * slot
		for step := range 8 {
			now := base.Add(time.Duration(step) * 37 * time.Minute)
			here := rotationSlot(now, slot, n)
			later := rotationSlot(now.Add(period), slot, n)
			if here != later {
				t.Fatalf("rotationSlot(%s, %s, %d) = %d but a full period later = %d — "+
					"the grid must repeat exactly, or the writer and the purge drift apart over time",
					now.Format(time.RFC3339), slot, n, here, later)
			}
		}
	}
}

// The correspondence ADR-037 assumes without writing down: the ordinal
// rotationSlot names is the ordinal whose purgePhase owns that slot. If the two
// ever disagree the feature inverts — the write Service would select the pod
// that is purging, which is the one outcome it exists to prevent, and nothing in
// the cluster would report it.
func TestRotationSlotNamesTheOrdinalWhosePhaseOwnsIt(t *testing.T) {
	const slot = 90 * time.Minute
	// A period boundary for every n in range: 15! seconds is not needed, a
	// multiple of 90 minutes by lcm(1..15) is. Anchoring on the epoch itself
	// keeps the arithmetic readable, and the grid is epoch-anchored anyway.
	base := time.Unix(0, 0).UTC()

	for n := int32(2); n <= 15; n++ {
		for k := int32(0); k < n; k++ {
			// A moment comfortably inside the slot that begins k slots into
			// the period.
			now := base.Add(time.Duration(k)*slot + slot/3)

			got := rotationSlot(now, slot, n)
			if got != k {
				t.Fatalf("rotationSlot(%s, %s, %d) = %d, want %d — "+
					"the slot that begins at k*t into the period belongs to pod k",
					now.Format(time.RFC3339), slot, n, got, k)
			}
			if phase, want := purgePhase(got, n, slot), time.Duration(k)*slot; phase != want {
				t.Fatalf("purgePhase(%d, %d, %s) = %s, want %s — "+
					"rotationSlot and purgePhase must read one grid; disagreeing puts the writer on the purging pod",
					got, n, slot, phase, want)
			}
		}
	}
}

// olcAccessLogPurge has two owners (ADR-037, second amendment, "Where the
// configuration lives"): the age is per-database retention and stays on
// SlapdDatabase, the interval and the phase are a schedule over pods and come
// from the cluster. composePurge is where they meet, and the only place they
// do — which is what removes the consistency problem without cross-resource
// validation, since databases cannot disagree about a schedule they no longer
// own.
//
// The age is used verbatim. It arrives already resolved from
// desiredAccesslogPurge, and reformatting it would rewrite what the user wrote.
func TestComposePurge(t *testing.T) {
	phase := func(d time.Duration) *time.Duration { return &d }
	cases := []struct {
		name      string
		retention time.Duration
		interval  time.Duration
		phase     *time.Duration
		want      string
		because   string
	}{
		{
			name:      "the three fields are space-separated in slapd's order",
			retention: 7 * 24 * time.Hour, interval: 4 * time.Hour, phase: phase(time.Hour),
			want:    "7+00:00:00 04:00:00 01:00:00",
			because: "the patch reads logpurge as <age> <interval> [<phase>] and nothing reorders it",
		},
		{
			name:      "every field goes through the one tested formatter",
			retention: 14 * 24 * time.Hour, interval: 4 * time.Hour, phase: phase(2 * time.Hour),
			want: "14+00:00:00 04:00:00 02:00:00",
			because: "log_age_parse is positional and refuses 0:05 where it accepts 00:05; " +
				"a typed duration and one formatter remove the class (ADR-024 R6)",
		},
		{
			name:      "pod 0's zero phase is emitted, not omitted",
			retention: 7 * 24 * time.Hour, interval: 4 * time.Hour, phase: phase(0),
			want:    "7+00:00:00 04:00:00 " + testZeroLogAge,
			because: "a two-field value leaves the task unaligned, and pod 0 would drift off the grid the others hold",
		},
		{
			// The reason phase is a POINTER. Form A has no phase at all; form
			// B's pod 0 has a phase of exactly zero. A plain duration would
			// make them one value, and the three-field form is the one a stock
			// slapd refuses outright.
			name:      "a nil phase is form A: two fields, every pod purging together",
			retention: 7 * 24 * time.Hour, interval: 4 * time.Hour, phase: nil,
			want:    "7+00:00:00 04:00:00",
			because: "absence and zero are different states, and only one of them a stock slapd can parse",
		},
		{
			name:      "a period of days carries the days prefix in both schedule fields",
			retention: 7 * 24 * time.Hour, interval: 48 * time.Hour, phase: phase(24 * time.Hour),
			want:    "7+00:00:00 2+00:00:00 1+00:00:00",
			because: "ADR-037 expects long phases, and both schedule fields go through the same strict parser",
		},
		{
			name:      "a non-positive retention composes to nothing at all",
			retention: 0, interval: 4 * time.Hour, phase: phase(time.Hour),
			want:    "",
			because: "a schedule must not resurrect a purge the user explicitly turned off",
		},
		{
			name:      "a non-positive interval means there is no schedule to write",
			retention: 7 * 24 * time.Hour, interval: 0, phase: nil,
			want:    "",
			because: "a retention alone is a value slapd refuses, and inventing an interval here would hide the misconfiguration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composePurge(tc.retention, tc.interval, tc.phase)
			if got != tc.want {
				t.Fatalf("composePurge(%s, %s, %v) = %q, want %q — %s",
					tc.retention, tc.interval, tc.phase, got, tc.want, tc.because)
			}
		})
	}
}

// The invariant slapd enforces, held by construction. The patched parser refuses
// a value whose phase is not strictly less than its interval:
//
//	<olcAccessLogPurge> phase must be less than interval
//
// and a refused value means that pod never purges. With phase = purgePhase(k, n,
// t) and interval = n*t it cannot happen for any ordinal in range — which is
// precisely why the composition seam is allowed to trust its caller instead of
// validating. This test is what makes that trust auditable.
func TestComposePurgeKeepsThePhaseBelowTheInterval(t *testing.T) {
	const slot = 90 * time.Minute
	const retention = 7 * 24 * time.Hour

	for n := int32(1); n <= 15; n++ {
		interval := time.Duration(n) * slot
		for k := int32(0); k < n; k++ {
			phase := purgePhase(k, n, slot)
			if phase >= interval {
				t.Fatalf("purgePhase(%d, %d, %s) = %s, interval %s — slapd refuses a phase that is not "+
					"strictly less than the interval, and that pod then never purges at all",
					k, n, slot, phase, interval)
			}

			value := composePurge(retention, interval, &phase)
			fields := strings.Fields(value)
			if len(fields) != 3 {
				t.Fatalf("composePurge(%s, %s, %s) = %q, want three space-separated fields — "+
					"an unpatched slapd reports extra cruft and a short value leaves the task unaligned",
					retention, interval, phase, value)
			}
			if want := formatLogAge(retention); fields[0] != want {
				t.Fatalf("composePurge wrote retention %q, want %q", fields[0], want)
			}
			if want := formatLogAge(interval); fields[1] != want {
				t.Fatalf("composePurge wrote interval %q, want %q — the schedule fields must go through the strict formatter",
					fields[1], want)
			}
			if want := formatLogAge(phase); fields[2] != want {
				t.Fatalf("composePurge wrote phase %q, want %q — the schedule fields must go through the strict formatter",
					fields[2], want)
			}
		}
	}
}

// ADR-037, ninth amendment, "the sign convention": v is the signed displacement
// in slots from a pod's WRITE slot to its own PURGE slot, taken mod n. Pod p
// purges in slot p, so pod p writes in slot p−v, and at slot s the writer is
// the pod that purges v slots from now: (s+v) mod n.
//
// The walk DEGRADES IN THE DIRECTION OF THE SIGN (tenth amendment): candidates
// are (v + k·sign(v)) mod n for increasing k, skipping ≡ 0, until all n−1
// non-zero offsets have been offered. The ninth amendment's ascending
// `v … n−1` is withdrawn — it had ONE candidate at v = −1, which handed the
// writes to the purging pod with pods to spare — and with it the claim that
// offset 0 needs no rule to exclude it. TestWriteTargetWalkOrder pins the
// order; the property tests below pin that offset 0 is never offered.
//
// v is a tunable with a measured range, not a fixed choice: it is bounded by
// the purge duration at one end and the drain time at the other, and the drain
// time has no measured value at all yet.
//
// Everything eligibility knows — Ready, ADR-036's authorship gate, decision 4's
// promotion gate — arrives through the predicate. The grid itself contributes
// exactly one rule, and it is the feature: offset 0, the pod purging this slot,
// is never offered by the walk.
func TestWriteTarget(t *testing.T) {
	cases := []struct {
		name     string
		slot     int32
		n        int32
		v        int32
		eligible func(int32) bool
		want     int32
		wantOK   bool
		because  string
	}{
		{
			name: "v of 1 is the default: the writer purges in the NEXT slot",
			slot: 0, n: 4, v: 1, eligible: eligibleAll(),
			want: 1, wantOK: true,
			because: "+1 is \"purge the slot after writing\", the configuration ADR-037 prefers and measures first",
		},
		{
			name: "the walk wraps around the period",
			slot: 3, n: 4, v: 1, eligible: eligibleAll(),
			want: 0, wantOK: true,
			because: "offsets are modular; the period has no edge for the walk to fall off",
		},
		{
			name: "v of -1 is n-1: the writer purged in the PREVIOUS slot",
			slot: 0, n: 4, v: -1, eligible: eligibleAll(),
			want: 3, wantOK: true,
			because: "the sign is taken mod n, which is what makes a declared value survive a scale event",
		},
		{
			name: "n-1 spelled out is the same answer as -1",
			slot: 0, n: 4, v: 3, eligible: eligibleAll(),
			want: 3, wantOK: true,
			because: "-1 and n-1 are one answer, which is why the operator reports the slot it RESOLVED to",
		},
		{
			name: "an ineligible preferred writer falls back to one that purges a little later",
			slot: 0, n: 4, v: 1, eligible: eligibleSet(0, 2, 3),
			want: 2, wantOK: true,
			because: "the order is a preference, not a requirement; the next-best pod still is not purging",
		},
		{
			name: "the walk skips every ineligible pod before giving up on the order",
			slot: 0, n: 4, v: 1, eligible: eligibleSet(0, 3),
			want: 3, wantOK: true,
			because: "offset n-1 is the last rung of the ascending walk and is still preferable to the purging pod",
		},
		{
			name: "v of 2 starts the walk two slots out",
			slot: 0, n: 4, v: 2, eligible: eligibleAll(),
			want: 2, wantOK: true,
			because: "2 is the deferred cooldown experiment: a slot between writing and purging",
		},
		{
			name: "with nothing else eligible the purging pod takes the writes anyway",
			slot: 0, n: 2, v: 1, eligible: eligibleSet(0),
			want: 0, wantOK: true,
			because: "ADR-037 decides this case explicitly: writes continuing badly beats writes not continuing",
		},
		{
			name: "nothing eligible at all leaves no writer to choose",
			slot: 2, n: 4, v: 1, eligible: eligibleNone(),
			want: 0, wantOK: false,
			because: "the caller must then leave the selector where it is; the write Service must not select nothing",
		},
		{
			name: "a v above the pods that exist is read modulo n, not clamped",
			slot: 0, n: 4, v: 9, eligible: eligibleAll(),
			want: 1, wantOK: true,
			because: "9 mod 4 is 1; modulo is the whole arithmetic of a signed displacement",
		},
		{
			name: "v of zero reads as one rather than selecting the purging pod",
			slot: 0, n: 4, v: 0, eligible: eligibleAll(),
			want: 1, wantOK: true,
			because: "v = 0 IS the purging pod, which is the one selection the feature exists to forbid; the gate refuses it with a condition and this is the defensive floor",
		},
		{
			name: "a multiple of n reads as one for the same reason",
			slot: 1, n: 4, v: 8, eligible: eligibleAll(),
			want: 2, wantOK: true,
			because: "8 mod 4 is 0, which is why the rule is `v mod n != 0` and not a minimum",
		},
		{
			name: "a single replica writes to itself",
			slot: 0, n: 1, v: 1, eligible: eligibleSet(0),
			want: 0, wantOK: true,
			because: "n = 1 has no rotation: the only pod is both purger and writer",
		},
		{
			name: "a single replica that is not eligible has no writer",
			slot: 0, n: 1, v: 1, eligible: eligibleNone(),
			want: 0, wantOK: false,
			because: "a down single pod is reported as no writer, not as a writer that happens to be down",
		},
		{
			name: "a non-positive replica count has no writer",
			slot: 0, n: 0, v: 1, eligible: eligibleAll(),
			want: 0, wantOK: false,
			because: "n is spec.replicas; with no pods there is nothing to select and nothing to purge",
		},
		{
			name: "a slot outside the period is read modulo n",
			slot: 5, n: 4, v: 1, eligible: eligibleAll(),
			want: 2, wantOK: true,
			because: "slot 5 of a four-slot period is slot 1, and the writer must not depend on how the caller counted",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := writeTarget(tc.slot, tc.n, tc.v, tc.eligible)
			if ok != tc.wantOK {
				t.Fatalf("writeTarget(%d, %d, %d, …) ok = %v, want %v — %s",
					tc.slot, tc.n, tc.v, ok, tc.wantOK, tc.because)
			}
			if got != tc.want {
				t.Fatalf("writeTarget(%d, %d, %d, …) = %d, want %d — %s",
					tc.slot, tc.n, tc.v, got, tc.want, tc.because)
			}
		})
	}
}

// The one rule the grid contributes, over every fleet size, every slot and
// every legal v: the writer is never the pod purging, unless it is the only
// candidate there is.
//
// This is the whole feature in one assertion. ADR-037's third amendment measured
// that the only variable a client can see is whether the write target purges —
// staggering the peers changed nothing at all — so a walk that can land on
// offset 0 while another pod is available does not merely degrade the feature,
// it removes it, and every health signal stays green while it does.
//
// This is the WEAK half of the property, kept for the healthy fleet it sweeps.
// The clause that does the work since ADR-037's tenth amendment — never the
// purging pod while ANY other pod is eligible — lives in
// TestWriteTargetPrefersAnyEligiblePodOverThePurgingOne, because the ninth
// amendment's walk satisfied this form and still had the hole.
//
// The legal range of v is "every int32 whose residue mod n is non-zero", so the
// sweep covers both signs rather than only the positive half the old
// parameterisation admitted.
func TestWriteTargetNeverPicksThePurgingPodUnlessItIsAlone(t *testing.T) {
	for n := int32(2); n <= 15; n++ {
		for slot := int32(0); slot < n; slot++ {
			for v := -2*n + 1; v <= 2*n; v++ {
				if v%n == 0 {
					// Refused by the gate with a condition; writeTarget's
					// defensive floor reads it as 1, which is covered above.
					continue
				}
				got, ok := writeTarget(slot, n, v, eligibleAll())
				if !ok {
					t.Fatalf("writeTarget(%d, %d, %d, all eligible) found no writer — "+
						"a healthy fleet always has one", slot, n, v)
				}
				if got == slot {
					t.Fatalf("writeTarget(%d, %d, %d, all eligible) = %d, the pod purging this slot — "+
						"the write target purging its own writes is the entire quantity the feature removes",
						slot, n, v, got)
				}
			}

			// The decided degenerate case: only the purging pod is left.
			got, ok := writeTarget(slot, n, 1, eligibleSet(slot))
			if !ok || got != slot {
				t.Fatalf("writeTarget(%d, %d, 1, only the purging pod eligible) = %d, %v; want %d, true — "+
					"ADR-037 takes the stall here: writes continuing badly beats writes not continuing",
					slot, n, got, ok, slot)
			}
		}
	}
}

// The correspondence the whole feature rests on, asserted end to end: the pod
// the write endpoint names at slot s is the pod whose PURGE PHASE is v slots
// ahead of the one currently firing. The grid is written by purgePhase and read
// by rotationSlot, and writeTarget has to agree with both or the endpoint moves
// to a pod that is about to purge.
func TestWriteTargetAgreesWithThePhaseGrid(t *testing.T) {
	const t0 = 15 * time.Minute
	for n := int32(2); n <= 8; n++ {
		period := time.Duration(n) * t0
		for _, v := range []int32{1, 2, -1} {
			if v%n == 0 {
				// Refused by the gate with a condition; writeTarget's
				// defensive floor reads it as 1, which is a different grid.
				continue
			}
			for step := range int64(n) {
				now := time.Unix(step*int64(t0/time.Second), 0)
				slot := rotationSlot(now, t0, n)
				writer, ok := writeTarget(slot, n, v, eligibleAll())
				if !ok {
					t.Fatalf("n=%d v=%d: no writer on a healthy fleet", n, v)
				}
				// The writer's own purge phase must be v slots after the phase
				// of the slot firing now, modulo the period.
				want := (purgePhase(slot, n, t0) + time.Duration(((v%n)+n)%n)*t0) % period
				if got := purgePhase(writer, n, t0); got != want {
					t.Fatalf("n=%d v=%d slot=%d: writer %d purges at %s, want %s — "+
						"writeTarget and purgePhase describe one grid and must agree on it",
						n, v, slot, writer, got, want)
				}
			}
		}
	}
}

// eligibleRecording wraps a predicate and records, in order, every ordinal
// writeTarget asked about. The walk's ORDER is a decision in its own right
// (ADR-037, tenth amendment: "preferred first, every offset exactly once"), and
// a walk that reaches the right pod by luck is indistinguishable from a correct
// one if only the answer is asserted.
func eligibleRecording(inner func(int32) bool, seen *[]int32) func(int32) bool {
	return func(o int32) bool {
		*seen = append(*seen, o)
		return inner(o)
	}
}

// The repro ADR-037's tenth amendment was written for: the ninth amendment's
// ascending walk `v, v+1, … n−1` has exactly ONE candidate whenever v sits at
// the end of the range. At v = -1 — "write just before purging", one of the two
// configurations this ADR exists to compare — a single ineligible pod therefore
// drops the writes onto the PURGING pod while n−2 eligible pods stand
// unexamined.
//
// The degenerate fallback exists for n = 2 with one pod down, where there
// genuinely is no other candidate. Reaching it with candidates available is the
// defect.
func TestWriteTargetDoesNotFallToThePurgingPodWhileAnotherIsEligible(t *testing.T) {
	cases := []struct {
		name    string
		n, v    int32
		slot    int32
		down    int32
		want    int32
		because string
	}{
		{
			name: "v of -1 with the preferred pod down walks AWAY from the purge",
			n:    4, v: -1, slot: 0, down: 3,
			want:    2,
			because: "offset 2 is still \"just before the purge\", one slot further out; offset 0 is the purging pod and three pods were never asked",
		},
		{
			name: "n-1 spelled out has the same hole and the same fix",
			n:    4, v: 3, slot: 0, down: 3,
			want:    1,
			because: "the bare form degrades ASCENDING — same selection as -1, opposite degradation, which is why ADR-037 recommends the signed spelling",
		},
		{
			name: "a five-pod fleet at v of -1 keeps walking down, not onto the purge",
			n:    5, v: -1, slot: 2, down: 1,
			want:    0,
			because: "ordinal 1 is offset 4; the next rung down is offset 3, ordinal 0 — the purging pod 2 is not a candidate while that exists",
		},
		{
			name: "v of +2 at the top of a three-pod fleet wraps rather than giving up",
			n:    3, v: 2, slot: 0, down: 2,
			want:    1,
			because: "stepping alone exhausts the range at |v| >= 2 (2, then nothing); the wrap offers offset 1 before the purging pod",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eligible := func(o int32) bool { return o != tc.down }
			got, ok := writeTarget(tc.slot, tc.n, tc.v, eligible)
			if !ok {
				t.Fatalf("writeTarget(%d, %d, %d, all but %d) found no writer — %s",
					tc.slot, tc.n, tc.v, tc.down, tc.because)
			}
			if got == tc.slot {
				t.Fatalf("writeTarget(%d, %d, %d, all but %d) = %d, the PURGING pod, while eligible pods went unexamined — %s",
					tc.slot, tc.n, tc.v, tc.down, got, tc.because)
			}
			if got != tc.want {
				t.Fatalf("writeTarget(%d, %d, %d, all but %d) = %d, want %d — %s",
					tc.slot, tc.n, tc.v, tc.down, got, tc.want, tc.because)
			}
		})
	}
}

// The order itself, pinned. ADR-037's tenth amendment tabulates it: candidates
// are (v + k·sign(v)) mod n for increasing k, skipping ≡ 0, until all n−1
// non-zero offsets have been offered — preferred first, each exactly once.
//
// The sign is read BEFORE the value is normalised mod n, so -1 and n−1 select
// the same pod and degrade in opposite directions. That divergence is the
// amendment's point and not an accident to flatten.
func TestWriteTargetWalkOrder(t *testing.T) {
	cases := []struct {
		name    string
		n, v    int32
		want    []int32
		because string
	}{
		{
			name: "+1 degrades upward", n: 4, v: 1,
			want:    []int32{1, 2, 3},
			because: "the default: purge the slot after writing, then a little later",
		},
		{
			name: "-1 degrades downward", n: 4, v: -1,
			want:    []int32{3, 2, 1},
			because: "\"just before the purge\" degrades to \"further before the purge\", never across it",
		},
		{
			name: "+2 wraps after the top", n: 4, v: 2,
			want:    []int32{2, 3, 1},
			because: "stepping alone would stop at 3 and never offer 1, which is the same hole in a new place",
		},
		{
			name: "-2 wraps after the bottom", n: 4, v: -2,
			want:    []int32{2, 1, 3},
			because: "the descending walk skips offset 0 and resumes at the top",
		},
		{
			name: "n-1 is not -1: same first candidate, ascending degradation", n: 4, v: 3,
			want:    []int32{3, 1, 2},
			because: "the sign is read before the modulo, so the bare spelling degrades the other way",
		},
		{
			name: "every non-zero offset exactly once on a wider fleet", n: 6, v: -1,
			want:    []int32{5, 4, 3, 2, 1},
			because: "the walk terminates having offered all n-1 non-zero offsets",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []int32
			// slot 0 makes the ordinal the offset, so the recorded sequence
			// reads as the table does. Nothing is eligible, so the walk runs
			// to exhaustion and the degenerate fallback asks about the purging
			// pod last.
			got, ok := writeTarget(0, tc.n, tc.v, eligibleRecording(eligibleNone(), &seen))
			if ok {
				t.Fatalf("writeTarget(0, %d, %d, nothing eligible) = %d, true — want no writer",
					tc.n, tc.v, got)
			}
			wantSeen := append(append([]int32{}, tc.want...), 0)
			if len(seen) != len(wantSeen) {
				t.Fatalf("writeTarget(0, %d, %d) asked about %v, want %v (walk then the degenerate fallback) — %s",
					tc.n, tc.v, seen, wantSeen, tc.because)
			}
			for i := range wantSeen {
				if seen[i] != wantSeen[i] {
					t.Fatalf("writeTarget(0, %d, %d) asked about %v, want %v — %s",
						tc.n, tc.v, seen, wantSeen, tc.because)
				}
			}
		})
	}
}

// The strengthened property, over every fleet size, every slot and every legal
// v of both signs: the writer is never the pod purging WHILE ANY OTHER POD IS
// ELIGIBLE. The previous form asserted only "not the purging pod on a fully
// healthy fleet", which the ninth amendment's walk satisfied while still
// handing writes to the purging pod the moment one preferred pod went away.
//
// Three eligibility shapes per (n, slot, v) bound it from the tight end: a
// single other pod, that pod plus the purging one, and everything except one
// other pod.
func TestWriteTargetPrefersAnyEligiblePodOverThePurgingOne(t *testing.T) {
	for n := int32(2); n <= 15; n++ {
		for slot := int32(0); slot < n; slot++ {
			for v := -2*n + 1; v <= 2*n; v++ {
				if v%n == 0 {
					// Refused by the gate with a condition; writeTarget's
					// defensive floor reads it as 1, which is covered above.
					continue
				}
				for q := int32(0); q < n; q++ {
					if q == slot {
						continue
					}
					// Only one other pod is up: it must be chosen.
					if got, ok := writeTarget(slot, n, v, eligibleSet(q)); !ok || got != q {
						t.Fatalf("writeTarget(%d, %d, %d, only %d eligible) = %d, %v; want %d, true — "+
							"the walk must offer every non-zero offset before the purging pod",
							slot, n, v, q, got, ok, q)
					}
					// That pod and the purging pod are up: still that pod.
					if got, ok := writeTarget(slot, n, v, eligibleSet(q, slot)); !ok || got != q {
						t.Fatalf("writeTarget(%d, %d, %d, %d and the purging pod eligible) = %d, %v; want %d, true — "+
							"the degenerate fallback is for when NOTHING else is left",
							slot, n, v, q, got, ok, q)
					}
					// Everything but that pod is up: anything except the purge.
					all := make([]int32, 0, n-1)
					for o := int32(0); o < n; o++ {
						if o != q {
							all = append(all, o)
						}
					}
					got, ok := writeTarget(slot, n, v, eligibleSet(all...))
					if !ok {
						t.Fatalf("writeTarget(%d, %d, %d, all but %d) found no writer", slot, n, v, q)
					}
					if got == slot && n > 2 {
						t.Fatalf("writeTarget(%d, %d, %d, all but %d) = %d, the purging pod, with %d others eligible",
							slot, n, v, q, got, n-2)
					}
				}
			}
		}
	}
}
