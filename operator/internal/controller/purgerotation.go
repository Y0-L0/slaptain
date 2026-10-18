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
	"time"
)

// secondsPerDay is where formatLogAge stops filling the hours field and starts
// the "ddd+" prefix slapd's log_age_parse expects.
const secondsPerDay = 24 * 60 * 60

// The purge rotation grid (ADR-037, second amendment "the rotation,
// specified"). With n read-write pods and a slot length t, the period is
// T = n*t and pod-k purges at phase k*t; the write endpoint rotates every t,
// in lockstep, so the pod taking writes is never the pod purging.
//
// Everything in this file is arithmetic. No clock is read here — a caller that
// needs the current slot passes time.Now() in — and nothing observes the
// cluster. That is deliberate: the ADR's "Test discipline" section makes the
// rotation plan a pure function of the configuration and the clock precisely so
// the decision is unit-testable and the e2e stays a thin integration shell.
//
// Two invariants hold the whole grid together and are easy to break by
// "simplifying":
//
//   - n is spec.replicas, DECLARED and never observed. n fixes T, and T is the
//     interval written into every pod's cn=config, so deriving n from readiness
//     would rewrite the whole fleet's configuration on one transient restart —
//     the churn ADR-013 exists to prevent. Readiness enters only in writeTarget,
//     which persists nothing. This is not a bug to fix.
//   - t is fixed and T = n*t varies, not the reverse. Scaling then leaves every
//     existing pod's phase alone and gives the new pod n*t; only the interval
//     moves, and changing an interval does not move a phase. Holding T fixed and
//     recomputing t = T/n would re-phase the entire fleet on every scale event.
//
// The contract of each of the five functions below is its doc comment plus
// purgerotation_test.go, which was authored and observed red against them.

// formatLogAge renders d in the time format slapd's log_age_parse accepts:
// "hh:mm:ss", or "ddd+hh:mm:ss" once d reaches a day.
//
// The parser is strict and its rejections are silent at the operator's end —
// a malformed value is refused at config load and that pod then never purges.
// So the fields are emitted unconditionally and zero-padded: "00:05:00", never
// "0:05:00" and never a bare "05:00". The hours field carries 0-23 only;
// anything from a day up goes into the "ddd+" prefix, whose day count is NOT
// zero-padded (which is how the shipped default, "7+00:00 1+00:00", is spelled).
//
// Defined behaviour for inputs the grid should never produce:
//
//   - sub-second precision truncates toward zero; the runqueue grid is whole
//     seconds (the patch stores tv_sec), so there is nothing finer to express.
//   - a negative duration clamps to zero. slapd's parser rejects a leading '-'
//     outright, and emitting a value it rejects would lose the whole purge
//     configuration for that pod; "00:00:00" is the conservative defined answer,
//     and as an interval it is refused loudly by the server rather than silently.
func formatLogAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	secs := int64(d / time.Second)
	days, rest := secs/secondsPerDay, secs%secondsPerDay
	hh, mm, ss := rest/3600, rest%3600/60, rest%60

	if days > 0 {
		return fmt.Sprintf("%d+%02d:%02d:%02d", days, hh, mm, ss)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss)
}

// purgePhase returns the offset into the period at which pod `ordinal` purges:
// ordinal*t, where t is the slot length. It is the one half of the grid that
// must never move — a phase is absolute, anchored to the Unix epoch by the
// accesslog-purge-phase patch, so a returning pod rejoins its own slot at the
// next boundary without re-phasing anyone.
//
// n (spec.replicas) does not enter the value for an in-range ordinal. It is a
// parameter because it bounds the grid: the period is n*t, and slapd refuses a
// phase that is not strictly less than the interval ("<olcAccessLogPurge> phase
// must be less than interval"). For every ordinal in [0, n) the returned phase
// is therefore distinct and below n*t by construction.
//
// purgePhase reads no clock, and must not acquire one. The grid is the same
// whenever it is asked.
//
// Defined behaviour for inputs the grid should never produce:
//
//   - an ordinal at or beyond n returns ordinal*t UNWRAPPED, which is a phase
//     >= the interval and so a value slapd refuses outright. That is deliberate.
//     The case cannot arise from the real call site — the per-pod loop runs over
//     [0, replicas), so a pod above spec.replicas is never visited and simply
//     keeps its existing configuration until it terminates — which makes this a
//     defensive branch, and the only question is which defined behaviour is
//     least dangerous should the call site ever change. Wrapping modulo n would
//     put two pods in one slot: the synchronised purge this whole feature exists
//     to prevent, arriving silently, with every health signal green. Unwrapped,
//     the modify fails with "<olcAccessLogPurge> phase must be less than
//     interval", the error propagates into the pass's failed-pod reporting, and
//     the pod keeps its previous working value. Loud, bounded, self-announcing.
//   - a negative ordinal reads as pod 0.
//   - n <= 0 or t <= 0 means there is no grid: the phase is zero.
func purgePhase(ordinal, n int32, t time.Duration) time.Duration {
	if n <= 0 || t <= 0 || ordinal <= 0 {
		return 0
	}
	// Deliberately unwrapped: an ordinal at or beyond n yields a phase slapd
	// refuses, which is the loud failure the doc comment above argues for.
	return time.Duration(ordinal) * t
}

// rotationSlot returns the ordinal whose purge is current at `now`:
// floor(unixSeconds(now)/t) mod n.
//
// This is the same absolute grid purgePhase describes, read from the other end.
// The correspondence is the whole mechanism and is load-bearing: at any instant
// inside the slot that begins at k*t within the period, rotationSlot returns k,
// and purgePhase(k, n, t) is k*t. The ADR does not write the formula out — it
// assumes the correspondence, because the purge grid and the rotation grid are
// one grid.
//
// The anchor is the Unix epoch in seconds, matching the patched scheduler, which
// computes the next fire as ph + ((now - ph)/iv + 1)*iv with ph = phase % iv and
// now = time(NULL). Because the epoch is midnight UTC, any interval dividing a
// day yields midnight-relative slots.
//
// Defined behaviour for inputs the grid should never produce:
//
//   - the division is a FLOOR, not Go's truncation, so the grid stays continuous
//     across the epoch; a `now` before 1970 is absurd, but a truncating
//     implementation would make the grid discontinuous exactly there, and the
//     floor is the honest statement of "an absolute grid".
//   - the result is always in [0, n), never negative.
//   - n <= 0, or a slot length below one second, means there is no grid: slot 0.
func rotationSlot(now time.Time, t time.Duration, n int32) int32 {
	step := int64(t / time.Second)
	if n <= 0 || step <= 0 {
		return 0
	}

	// Floor division, not Go's truncation, so the grid stays continuous
	// across the epoch.
	sec := now.Unix()
	slots := sec / step
	if sec < 0 && sec%step != 0 {
		slots--
	}

	return int32((slots%int64(n) + int64(n)) % int64(n))
}

// composePurge builds the olcAccessLogPurge value slapd sees:
// "<retention> <interval>", with "<phase>" appended when there is one.
//
// This is the one place the directive's three owners meet (ADR-024 R6).
// Retention is the database's, the interval is the pod's and therefore the
// cluster's, and the phase is derived per pod and declared by nobody. Each
// arrives here as a time.Duration and leaves as one tested rendering of
// slapd's [ddd+]hh:mm[:ss] — which is the third thing decomposition buys:
// log_age_parse is positional and refuses "0:05" where it accepts "00:05", so
// handing a user that format means handing them a plausible value that is
// silently rejected.
//
// phase is a POINTER because absence is a real state and zero is a real value.
// Form A — one interval, every pod purging together — has no phase at all;
// form B's pod 0 has a phase of exactly zero and must still carry it. Taking a
// plain duration would make those two indistinguishable, and the three-field
// value is the one a stock slapd refuses outright, so the difference decides
// whether an unpatched image can run this database at all.
//
// The caller is responsible for the invariant slapd enforces, phase < interval.
// With phase = purgePhase(k, n, t) and interval = n*t it holds by construction
// for every ordinal in [0, n).
//
// Defined behaviour for inputs the planner should never produce:
//
//   - a non-positive retention or interval yields "", meaning no
//     olcAccessLogPurge at all. Emitting a retention alone would be a
//     one-field value slapd refuses, and inventing an interval here would hide
//     the misconfiguration from the condition that exists to report it. The
//     planner resolves both before calling, so reaching this is a bug upstream
//     rather than a user error to absorb.
func composePurge(retention, interval time.Duration, phase *time.Duration) string {
	if retention <= 0 || interval <= 0 {
		return ""
	}
	v := formatLogAge(retention) + " " + formatLogAge(interval)
	if phase != nil {
		v += " " + formatLogAge(*phase)
	}
	return v
}

// writeTarget picks the pod the write Service should select during slot `slot`.
//
// ADR-037, ninth amendment, "the sign convention": v is
// spec.rotation.slotsFromWriteToPurge, the signed displacement in slots from a
// pod's WRITE slot to its own PURGE slot, taken mod n. Pod p purges in slot p
// (that is what purgePhase says), so pod p writes in slot p−v, and at slot s
// the writer is therefore the pod that purges v slots from now: (s+v) mod n.
//
// The walk is ASCENDING — v, v+1, … n−1 — reading as "the pod that purges v
// slots from now; if it is not eligible, one that purges a little later".
// Offset 0 is the pod purging THIS slot, and it falls off the end of the range
// rather than needing a rule to exclude it. Under the superseded descending
// parameterisation "never offset 0" was a doc comment plus a property test;
// here it is the loop bound.
//
// eligible carries everything the grid does not know: Ready, not read-only
// under ADR-036's authorship gate, not working off purge backlog, lag or a
// recent absence (decision 4's promotion gate).
//
// Two decided edges, neither of them a fallthrough:
//
//   - degenerate fallback. If no offset in [v, n−1] is eligible, the purging
//     pod itself is taken when it is eligible. ADR-037: with n = 2 and one pod
//     down the only candidate is the purging pod — "take it and accept the
//     stall: writes continuing badly beats writes not continuing".
//   - nothing eligible at all returns (0, false). The false means "no writer
//     could be chosen", and the caller must then leave the Service selector
//     where it is: the write Service must not be left selecting nothing.
//
// Defined behaviour for inputs the gate should never produce:
//
//   - v is read modulo n, so −1 and n−1 are the same answer. That is the point
//     of the sign convention and is what makes a declared value survive a
//     scale event.
//   - v ≡ 0 (mod n) reads as 1. Zero and every multiple of n put the write on
//     the pod that is purging; the rotation gate refuses such a value with a
//     condition (reasonWriteSlotCollidesWithPurge) and withholds the whole
//     feature, so this is the defensive end of a path that should not be
//     walked — and 1 is the one answer that cannot silently invert the
//     rotation. n = 1 is the exception the rule cannot cover: with one pod
//     every v is a multiple of n, the single pod is both purger and writer,
//     and it is reached through the degenerate fallback.
//   - n <= 0 has no writer. A slot outside [0, n) is read modulo n.
func writeTarget(slot, n, v int32, eligible func(int32) bool) (int32, bool) {
	if n <= 0 {
		return 0, false
	}
	slot = (slot%n + n) % n

	// Modulo first — the whole point of a signed displacement is that −1 and
	// n−1 are one answer. Only then the defensive floor: v ≡ 0 is the purging
	// pod, which is the one selection this feature exists to forbid.
	v = (v%n + n) % n
	if v == 0 {
		v = 1
	}

	// The ascending preference walk. Offset 0 is not in the range.
	for j := v; j <= n-1; j++ {
		if ord := (slot + j) % n; eligible(ord) {
			return ord, true
		}
	}

	// The degenerate fallback: nothing else is left, so the purging pod takes
	// the writes and the stall with them.
	if eligible(slot) {
		return slot, true
	}
	return 0, false
}
