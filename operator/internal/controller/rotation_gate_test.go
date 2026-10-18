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

// ADR-037's fourth amendment hands the operator four checks the API server
// cannot make, "each reported as a condition rather than refused at admission":
//
//  1. slotLength <= 0 — admission accepts "0s", and t = 0 collapses every
//     pod's phase k·t to zero, silently restoring the synchronised purge this
//     whole feature exists to remove.
//  2. a slotLength whose derived interval or phase is not a whole number of
//     seconds — olcAccessLogPurge is [ddd+]hh:mm[:ss], and "500ms" passes
//     admission and cannot be written.
//  3. writerOffset > n-1 — the upper bound depends on spec.replicas, which a
//     CRD marker cannot see. Clamp and say so.
//  4. slotLength absent while enabled is true.
//
// evaluateRotation is the pure seam that makes all four assertable without a
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

// fieldSlotLength is the spec path the gate's messages must name, so that a
// reader of the condition knows what to edit without reading the operator.
const fieldSlotLength = "spec.rotation.slotLength"

func TestEvaluateRotation(t *testing.T) {
	cases := []struct {
		name         string
		sc           *ldapv1alpha1.SlapdCluster
		wantActive   bool
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantInterval time.Duration
		wantSlot     time.Duration
		wantOffset   int32
	}{
		{
			name:       "unset: nothing rotates, and that is not a fault",
			sc:         scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{}),
			wantActive: false,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonRotationDisabled,
		},
		{
			name: "explicitly disabled, slot length and all",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(false), SlotLength: dur(6 * time.Hour),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonRotationDisabled,
		},
		{
			name: "enabled with a slot length: T = replicas * t, writer at n-1",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(6 * time.Hour),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 18 * time.Hour,
			wantSlot:     6 * time.Hour,
			wantOffset:   2,
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
			wantOffset:   1,
		},
		{
			name: "duty 4: enabled with no slot length is inert, and says so",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonSlotLengthMissing,
		},
		{
			name: "duty 1: a zero slot length collapses every phase onto zero",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(0),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonSlotLengthNotPositive,
		},
		{
			name: "duty 1: a negative slot length is the same fault",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(-time.Hour),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonSlotLengthNotPositive,
		},
		{
			name: "duty 2: 500ms has no representation in olcAccessLogPurge",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(500 * time.Millisecond),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonSlotLengthNotWholeSeconds,
		},
		{
			name: "duty 2: a sub-second remainder on an otherwise sane value",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour + 500*time.Millisecond),
			}),
			wantActive: false,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonSlotLengthNotWholeSeconds,
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
			wantOffset:   1,
		},
		{
			name: "duty 3: a writerOffset above n-1 is clamped, and the clamp is reported",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour), WriterOffset: new(int32(9)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonWriterOffsetClamped,
			wantInterval: 3 * time.Hour,
			wantSlot:     time.Hour,
			wantOffset:   2,
		},
		{
			name: "duty 3: a writerOffset below 1 would put the writer on the purging pod",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour), WriterOffset: new(int32(0)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonWriterOffsetClamped,
			wantInterval: 3 * time.Hour,
			wantSlot:     time.Hour,
			wantOffset:   1,
		},
		{
			name: "an in-range writerOffset is taken verbatim and reported as active",
			sc: scWithRotation(4, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour), WriterOffset: new(int32(2)),
			}),
			wantActive:   true,
			wantStatus:   metav1.ConditionTrue,
			wantReason:   reasonRotationActive,
			wantInterval: 4 * time.Hour,
			wantSlot:     time.Hour,
			wantOffset:   2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateRotation(tc.sc)
			if got.Active != tc.wantActive {
				t.Fatalf("Active = %v, want %v", got.Active, tc.wantActive)
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
			if !tc.wantActive {
				return
			}
			if got.Interval != tc.wantInterval {
				t.Fatalf("Interval = %v, want %v", got.Interval, tc.wantInterval)
			}
			if got.SlotLength != tc.wantSlot {
				t.Fatalf("SlotLength = %v, want %v", got.SlotLength, tc.wantSlot)
			}
			if got.WriterOffset != tc.wantOffset {
				t.Fatalf("WriterOffset = %d, want %d", got.WriterOffset, tc.wantOffset)
			}
		})
	}
}

// A nil cluster must never panic the reconciler — every other pure resolver in
// this package is nil-safe for the same reason.
func TestEvaluateRotation_NilClusterIsInert(t *testing.T) {
	got := evaluateRotation(nil)
	if got.Active {
		t.Fatal("a nil SlapdCluster must not produce an active rotation")
	}
	if got.Reason != reasonRotationDisabled {
		t.Fatalf("Reason = %q, want %q", got.Reason, reasonRotationDisabled)
	}
}

// The faults must be legible without reading the operator's source: each
// message names the field at fault and what the consequence is.
func TestEvaluateRotation_MessagesNameTheField(t *testing.T) {
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
			name: "clamped writer offset",
			sc: scWithRotation(3, ldapv1alpha1.SlapdRotationConfig{
				Enabled: new(true), SlotLength: dur(time.Hour), WriterOffset: new(int32(9)),
			}),
			mustSay: []string{"spec.rotation.writerOffset", "9", "2"},
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
