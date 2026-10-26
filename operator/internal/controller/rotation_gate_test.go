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

// WHEN the purge runs is the cluster's, in one of two mutually exclusive forms
// (ADR-024 R6, ADR-037 ninth amendment):
//
//	form A  spec.accesslogPurge.interval  — one schedule, every pod together
//	form B  spec.rotation                 — staggered phases + the write endpoint
//
// The gate resolves both into one plan, and hands the operator the checks the
// API server cannot make, "each reported as a condition rather than refused at
// admission":
//
//  1. both forms declared at once — refused, never silently preferred.
//  2. slotLength <= 0 — admission accepts "0s", and t = 0 collapses every
//     pod's phase k·t to zero, silently restoring the synchronised purge this
//     whole feature exists to remove.
//  3. a slotLength that is not a whole number of seconds — olcAccessLogPurge is
//     [ddd+]hh:mm[:ss], and "500ms" passes admission and cannot be written.
//  4. slotsFromWriteToPurge ≡ 0 (mod n) — the rule needs spec.replicas, which a
//     CRD marker cannot see.
//  5. slotLength absent while enabled is true.
//  6. a form-A interval with no representation on the server.
//
// EVERY EXIT KEEPS A USABLE INTERVAL. That is the property this milestone
// exists for: whatever the user got wrong, the journals stay bounded, because
// an unpurged journal grows to its map ceiling and then stops data writes.
//
// evaluateRotation is the pure seam that makes all of it assertable without a
// cluster: it reads the CR, reads no clock and performs no I/O.

func scWithRotation(replicas int32, r ldapv1alpha1.SlapdRotationConfig) *ldapv1alpha1.SlapdCluster {
	return &ldapv1alpha1.SlapdCluster{
		Spec: ldapv1alpha1.SlapdClusterSpec{
			Replicas: replicas,
			Rotation: r,
		},
	}
}

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

// scWithInterval is form A: a declared interval and no rotation.
func scWithInterval(iv time.Duration) *ldapv1alpha1.SlapdCluster {
	sc := scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{})
	sc.Spec.AccesslogPurge = &ldapv1alpha1.SlapdAccesslogPurgeConfig{Interval: dur(iv)}
	return sc
}

// fieldSlotLength is the spec path the gate's messages must name, so that a
// reader of the condition knows what to edit without reading the operator.
const (
	fieldSlotLength    = "spec.rotation.slotLength"
	fieldFormAInterval = "spec.accesslogPurge.interval"
	fieldSlotsFromWTP  = "spec.rotation.slotsFromWriteToPurge"
)

func TestEvaluateRotation(t *testing.T) {
	bothForms := scWithInterval(6 * time.Hour)
	bothForms.Spec.Rotation = ldapv1alpha1.SlapdRotationConfig{
		Enabled: new(true), SlotLength: dur(time.Hour),
	}

	cases := []struct {
		name         string
		sc           *ldapv1alpha1.SlapdCluster
		wantActive   bool
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantInterval time.Duration
		wantSlot     time.Duration
		wantV        int32
	}{
		{
			name:         "unset: nothing rotates, everyone purges on the default interval",
			sc:           scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{}),
			wantActive:   false,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationDisabled,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "explicitly disabled, slot length and all",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(false), SlotLength: dur(6 * time.Hour),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationDisabled,
			wantInterval: defaultPurgeInterval,
		},
		{
			name:         "form A: a declared interval, every pod purging together",
			sc:           scWithInterval(6 * time.Hour),
			wantActive:   false,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationDisabled,
			wantInterval: 6 * time.Hour,
		},
		{
			name: "form A beside an explicitly DISABLED rotation is not a conflict",
			sc: func() *ldapv1alpha1.SlapdCluster {
				sc := scWithInterval(6 * time.Hour)
				sc.Spec.Rotation = ldapv1alpha1.SlapdRotationConfig{
					Enabled: new(false), SlotLength: dur(time.Hour),
				}
				return sc
			}(),
			wantActive:   false,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationDisabled,
			wantInterval: 6 * time.Hour,
		},
		{
			name:         "BOTH FORMS: refused, and the default interval keeps the journals bounded",
			sc:           bothForms,
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonScheduleDeclaredTwice,
			wantInterval: defaultPurgeInterval,
		},
		{
			name:         "form A, a non-positive interval is reported and the default stands",
			sc:           scWithInterval(0),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonPurgeIntervalNotUsable,
			wantInterval: defaultPurgeInterval,
		},
		{
			name:         "form A, a sub-second interval has no representation on the server",
			sc:           scWithInterval(1500 * time.Millisecond),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonPurgeIntervalNotUsable,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "form B: T = replicas × t, and the writer purges one slot later",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(6 * time.Hour),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 18 * time.Hour,
			wantSlot:     6 * time.Hour,
			wantV:        1,
		},
		{
			name: "replicas unset defaults to one pod, which is a one-slot grid",
			sc: scWithRotation(0, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: time.Hour,
			wantSlot:     time.Hour,
			wantV:        1,
		},
		{
			name: "duty 5: enabled with no slot length is inert, and says so",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonSlotLengthMissing,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "duty 2: a zero slot length collapses every phase onto zero",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(0),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonSlotLengthNotPositive,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "duty 2: a negative slot length is the same fault",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(-time.Hour),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonSlotLengthNotPositive,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "duty 3: 500ms has no representation in olcAccessLogPurge",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(500 * time.Millisecond),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonSlotLengthNotWholeSeconds,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "duty 3: a sub-second remainder on an otherwise sane value",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour + 500*time.Millisecond),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonSlotLengthNotWholeSeconds,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "one whole second is the finest grid the server can express",
			sc: scWithRotation(2, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Second),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 2 * time.Second,
			wantSlot:     time.Second,
			wantV:        1,
		},
		{
			name: "duty 4: v of zero puts the write on the pod that is purging",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(0)),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonWriteSlotCollidesWithPurge,
			wantInterval: defaultPurgeInterval,
		},
		{
			// The reason the rule is modular and not a minimum: 3 is a
			// perfectly ordinary-looking number that resolves to the purging
			// pod on a three-pod cluster, and a `minimum: 1` marker would
			// admit it without a word.
			name: "duty 4: a multiple of n is the same collision, wearing a legal-looking number",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(3)),
			}),
			wantActive:   false,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   reasonWriteSlotCollidesWithPurge,
			wantInterval: defaultPurgeInterval,
		},
		{
			name: "a negative v is legal and resolves mod n: -1 is n-1",
			sc: scWithRotation(4, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(-1)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 4 * time.Hour,
			wantSlot:     time.Hour,
			wantV:        3,
		},
		{
			name: "a v above n is legal and resolves mod n",
			sc: scWithRotation(4, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(6)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 4 * time.Hour,
			wantSlot:     time.Hour,
			wantV:        2,
		},
		{
			name: "an in-range v is taken verbatim and reported as active",
			sc: scWithRotation(4, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(2)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 4 * time.Hour,
			wantSlot:     time.Hour,
			wantV:        2,
		},
		{
			// n = 1 is the one case the modular rule cannot cover: every v is
			// a multiple of 1, and the single pod is unavoidably both writer
			// and purger. Refusing a one-replica cluster for that would be a
			// rule punishing a shape it does not apply to.
			name: "a single replica is not refused for an unsatisfiable rule",
			sc: scWithRotation(1, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(1)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: time.Hour,
			wantSlot:     time.Hour,
			wantV:        1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateRotation(tc.sc)
			// Every case in this table is the coupled default (no mode set),
			// so the two halves move together; asserting both keeps the table
			// honest if one of them ever stops.
			if got.PhasesActive != tc.wantActive || got.WriterActive != tc.wantActive {
				t.Fatalf("phases/writer = %v/%v, want both %v",
					got.PhasesActive, got.WriterActive, tc.wantActive)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %v, want %v", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Message == "" {
				t.Fatal("Message is empty; a condition that says False without saying why is useless")
			}
			if got.Interval != tc.wantInterval {
				t.Fatalf("Interval = %v, want %v", got.Interval, tc.wantInterval)
			}
			if !tc.wantActive {
				return
			}
			if got.SlotLength != tc.wantSlot {
				t.Fatalf("SlotLength = %v, want %v", got.SlotLength, tc.wantSlot)
			}
			if got.SlotsFromWriteToPurge != tc.wantV {
				t.Fatalf("SlotsFromWriteToPurge = %d, want %d", got.SlotsFromWriteToPurge, tc.wantV)
			}
		})
	}
}

// THE PROPERTY THE MILESTONE EXISTS FOR, over every way a cluster can be
// misconfigured: the plan always carries a usable interval, so a database can
// never come out of a pass with no purge value. The old shape could leave a
// database unpurged on every pod at every site; this is the assertion that says
// it cannot happen again, whatever is in the spec.
func TestEvaluateRotation_AlwaysYieldsAUsableInterval(t *testing.T) {
	both := scWithInterval(6 * time.Hour)
	both.Spec.Rotation = ldapv1alpha1.SlapdRotationConfig{
		Enabled: new(true), SlotLength: dur(time.Hour),
	}
	clusters := map[string]*ldapv1alpha1.SlapdCluster{
		"nil":                nil,
		"nothing declared":   scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{}),
		"form A":             scWithInterval(6 * time.Hour),
		"form A, zero":       scWithInterval(0),
		"form A, sub-second": scWithInterval(time.Millisecond),
		"both forms":         both,
		"form B": scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true), SlotLength: dur(6 * time.Hour),
		}),
		"form B, no slot length": scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true),
		}),
		"form B, zero slot length": scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true), SlotLength: dur(0),
		}),
		"form B, sub-second slot length": scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true), SlotLength: dur(time.Millisecond),
		}),
		"form B, colliding v": scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true), SlotLength: dur(time.Hour),
			SlotsFromWriteToPurge: new(int32(0)),
		}),
		"zero replicas": scWithRotation(0, ldapv1alpha1.SlapdRotationConfig{
			Enabled: new(true), SlotLength: dur(time.Hour),
		}),
	}
	for name, sc := range clusters {
		t.Run(name, func(t *testing.T) {
			plan := evaluateRotation(sc)
			if plan.Interval <= 0 {
				t.Fatalf("plan.Interval = %v. A cluster with no interval writes no "+
					"olcAccessLogPurge, the journal grows to its map ceiling, and then DATA "+
					"writes stop — the outage this milestone removed", plan.Interval)
			}
			if plan.Interval%time.Second != 0 {
				t.Fatalf("plan.Interval = %v has no representation in [ddd+]hh:mm[:ss]",
					plan.Interval)
			}
			if plan.Replicas <= 0 {
				t.Fatalf("plan.Replicas = %d; the grid must stay defined", plan.Replicas)
			}
		})
	}
}

// A nil cluster must never panic the reconciler — every other pure resolver in
// this package is nil-safe for the same reason.
func TestEvaluateRotation_NilClusterIsInert(t *testing.T) {
	got := evaluateRotation(nil)
	if got.PhasesActive || got.WriterActive {
		t.Fatal("a nil SlapdCluster must not produce an active rotation")
	}
	if got.Reason != reasonRotationDisabled {
		t.Fatalf("Reason = %q, want %q", got.Reason, reasonRotationDisabled)
	}
}

// The faults must be legible without reading the operator's source: each
// message names the field at fault and what the consequence is.
func TestEvaluateRotation_MessagesNameTheField(t *testing.T) {
	both := scWithInterval(6 * time.Hour)
	both.Spec.Rotation = ldapv1alpha1.SlapdRotationConfig{
		Enabled: new(true), SlotLength: dur(time.Hour),
	}
	cases := []struct {
		name    string
		sc      *ldapv1alpha1.SlapdCluster
		mustSay []string
	}{
		{
			name: "zero slot length",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(0),
			}),
			mustSay: []string{fieldSlotLength},
		},
		{
			name: "sub-second slot length",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(500 * time.Millisecond),
			}),
			mustSay: []string{fieldSlotLength, "500ms"},
		},
		{
			name: "missing slot length",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true),
			}),
			mustSay: []string{fieldSlotLength},
		},
		{
			// Both field paths, so the reader knows there are two things and
			// which two — "mutually exclusive" is useless without the names.
			name:    "both forms names both fields",
			sc:      both,
			mustSay: []string{fieldFormAInterval, "spec.rotation.enabled", "mutually exclusive"},
		},
		{
			// ADR-037: the operator "should name the slot it RESOLVED to
			// rather than echo what was typed, since −1 and n−1 are the same
			// answer". Here the typed value is 3 and the resolved slot is 0.
			name: "a colliding v names the slot it resolved to",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour),
				SlotsFromWriteToPurge: new(int32(3)),
			}),
			mustSay: []string{fieldSlotsFromWTP, "slot 0", "purging"},
		},
		{
			name:    "an unusable form-A interval names the field",
			sc:      scWithInterval(0),
			mustSay: []string{fieldFormAInterval},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := evaluateRotation(tc.sc).Message
			for _, want := range tc.mustSay {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// The condition is positively named and carries the generation it was computed
// from, like every other condition this operator publishes.
func TestRotationConfiguredCondition(t *testing.T) {
	sc := scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
		Enabled: new(true), SlotLength: dur(0),
	})
	sc.Generation = 7

	cond := rotationConfiguredFor(sc, evaluateRotation(sc))
	if cond.Type != rotationConfiguredCondition {
		t.Fatalf("Type = %q, want %q", cond.Type, rotationConfiguredCondition)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False", cond.Status)
	}
	if cond.ObservedGeneration != 7 {
		t.Fatalf("ObservedGeneration = %d, want 7", cond.ObservedGeneration)
	}
}

// ---------------------------------------------------------------------------
// ADR-037 seventeenth amendment: the two halves are independently settable.
//
// Staggering the purges and moving the write endpoint are one feature today and
// two decisions in principle. The amendment separates them as tier-2 surfaces
// (ADR-039), each carrying its own evidence:
//
//	PurgeAndWrite  the measured one — 93,686/s at d=0 against 99,245/s at
//	               d=180s, 281x fewer drops.
//	PurgeOnly      inert for WRITER throughput (measured, n=2, 100k ops/s);
//	               UNMEASURED for fleet read capacity, which is the quantity it
//	               might actually buy, since reads run ~75 % on a purging
//	               provider and this staggers which pod is degraded.
//	WriteOnly      never measured as its own arm. Its rationale is ADR-036 /
//	               ITS#9580 dormancy: every provider's own sid keeps advancing.
//
// Two consequences the amendment predicted, and the reason this is not just an
// enum. Without purge phases `slotsFromWriteToPurge` has no purge slot to
// displace from, and `leadTime` has no boundary-purge to arrive ahead of — so
// in two of the three modes, two of the four fields lose their referent. ADR-024
// R4 says a field that cannot be honoured is reported, never ignored.
//
// And the form-A exclusion has to NARROW. It exists because rotation declares
// the purge schedule; WriteOnly declares none, so a fleet-wide interval is its
// natural companion rather than a conflict.
func TestEvaluateRotation_ModesSplitTheTwoHalves(t *testing.T) {
	on := true
	rot := func(mode string, tweak func(*ldapv1alpha1.SlapdRotationConfig)) ldapv1alpha1.SlapdRotationConfig {
		r := ldapv1alpha1.SlapdRotationConfig{Enabled: &on, SlotLength: dur(6 * time.Hour), Mode: mode}
		if tweak != nil {
			tweak(&r)
		}
		return r
	}

	t.Run("unset mode is the coupled form, byte-identical to the version without modes", func(t *testing.T) {
		p := evaluateRotation(scWithRotation(3, rot("", nil)))
		if !p.PhasesActive || !p.WriterActive {
			t.Fatalf("unset mode must drive BOTH halves: phases=%v writer=%v", p.PhasesActive, p.WriterActive)
		}
		if p.Reason != reasonRotationActive {
			t.Fatalf("reason = %q, want %q", p.Reason, reasonRotationActive)
		}
	})

	t.Run("PurgeOnly staggers the phases and leaves the endpoint alone", func(t *testing.T) {
		p := evaluateRotation(scWithRotation(3, rot(ldapv1alpha1.RotationModePurgeOnly, nil)))
		if !p.PhasesActive || p.WriterActive {
			t.Fatalf("PurgeOnly: phases=%v writer=%v, want true/false", p.PhasesActive, p.WriterActive)
		}
		if p.Interval != 18*time.Hour {
			t.Fatalf("PurgeOnly still derives the interval: got %s, want 18h", p.Interval)
		}
	})

	t.Run("WriteOnly moves the endpoint and declares no purge schedule", func(t *testing.T) {
		p := evaluateRotation(scWithRotation(3, rot(ldapv1alpha1.RotationModeWriteOnly, nil)))
		if p.PhasesActive || !p.WriterActive {
			t.Fatalf("WriteOnly: phases=%v writer=%v, want false/true", p.PhasesActive, p.WriterActive)
		}
		if p.Interval != defaultPurgeInterval {
			t.Fatalf("WriteOnly declares no schedule, so the interval is the default: got %s, want %s",
				p.Interval, defaultPurgeInterval)
		}
	})

	t.Run("WriteOnly ACCEPTS a form-A interval — it declares none of its own", func(t *testing.T) {
		sc := scWithRotation(3, rot(ldapv1alpha1.RotationModeWriteOnly, nil))
		sc.Spec.AccesslogPurge = &ldapv1alpha1.SlapdAccesslogPurgeConfig{Interval: dur(12 * time.Hour)}
		p := evaluateRotation(sc)
		if p.Reason == reasonScheduleDeclaredTwice {
			t.Fatal("WriteOnly declares no schedule; form A is its companion, not a conflict")
		}
		if p.Interval != 12*time.Hour {
			t.Fatalf("the declared form-A interval must be honoured: got %s, want 12h", p.Interval)
		}
		if !p.WriterActive {
			t.Fatal("the endpoint must still rotate")
		}
	})

	for _, mode := range []string{"", ldapv1alpha1.RotationModePurgeOnly} {
		t.Run("a schedule-declaring mode still refuses form A: "+mode, func(t *testing.T) {
			sc := scWithRotation(3, rot(mode, nil))
			sc.Spec.AccesslogPurge = &ldapv1alpha1.SlapdAccesslogPurgeConfig{Interval: dur(12 * time.Hour)}
			if got := evaluateRotation(sc); got.Reason != reasonScheduleDeclaredTwice {
				t.Fatalf("reason = %q, want %q", got.Reason, reasonScheduleDeclaredTwice)
			}
		})
	}

	// ADR-024 R4: reported, never ignored. The message must name the field and
	// the mode, because those are the two things the reader has to reconcile.
	noReferent := []struct {
		name, mode, field string
		tweak             func(*ldapv1alpha1.SlapdRotationConfig)
	}{
		{"leadTime under PurgeOnly", ldapv1alpha1.RotationModePurgeOnly, fieldRotationLeadTime,
			func(r *ldapv1alpha1.SlapdRotationConfig) { r.LeadTime = dur(5 * time.Minute) }},
		{"leadTime under WriteOnly", ldapv1alpha1.RotationModeWriteOnly, fieldRotationLeadTime,
			func(r *ldapv1alpha1.SlapdRotationConfig) { r.LeadTime = dur(5 * time.Minute) }},
		{"slotsFromWriteToPurge under PurgeOnly", ldapv1alpha1.RotationModePurgeOnly, fieldSlotsFromWTP,
			func(r *ldapv1alpha1.SlapdRotationConfig) { v := int32(1); r.SlotsFromWriteToPurge = &v }},
		{"slotsFromWriteToPurge under WriteOnly", ldapv1alpha1.RotationModeWriteOnly, fieldSlotsFromWTP,
			func(r *ldapv1alpha1.SlapdRotationConfig) { v := int32(1); r.SlotsFromWriteToPurge = &v }},
	}
	for _, tc := range noReferent {
		t.Run("reported, not ignored: "+tc.name, func(t *testing.T) {
			p := evaluateRotation(scWithRotation(3, rot(tc.mode, tc.tweak)))
			if p.Reason != reasonFieldHasNoReferentInMode {
				t.Fatalf("reason = %q, want %q", p.Reason, reasonFieldHasNoReferentInMode)
			}
			if !strings.Contains(p.Message, tc.field) || !strings.Contains(p.Message, tc.mode) {
				t.Fatalf("message must name the field and the mode; got %q", p.Message)
			}
			if p.Interval <= 0 {
				t.Fatal("every exit keeps a usable interval")
			}
		})
	}
}
