/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ADR-037, second amendment, "Where the configuration lives": olcAccessLogPurge
// gains a second owner under rotation. <age> is per-database retention and
// stays on SlapdDatabase; <interval> and <phase> are a schedule over pods and
// come from the cluster. This file holds the split, the per-(pod, database)
// composition, and the condition that reports what could not be honoured.

const (
	// defaultPurgeAge is the age half of defaultAccesslogPurge, spelled out
	// here rather than derived, so the drift guard below has something
	// independent to compare against.
	defaultPurgeAge = "7+00:00"
	// twoFieldPurge is a user-supplied value carrying an interval — the one
	// rotation refuses rather than silently overriding.
	twoFieldPurge = "2+00:00 1+00:00"
	// ageOnlyPurge is the shape a rotating cluster wants from the CR.
	ageOnlyPurge = "2+00:00"
	// noneMixedCase is the opt-out spelled the way a user might.
	noneMixedCase = "None"
)

func activePlan(slot time.Duration, replicas int32) rotationPlan {
	return rotationPlan{
		Active:     true,
		SlotLength: slot,
		Interval:   time.Duration(replicas) * slot,
		Replicas:   replicas,
	}
}

func TestSplitPurgeAge(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantAge   string
		wantExtra bool
	}{
		{"the shipped default splits into its age", defaultAccesslogPurge, defaultPurgeAge, true},
		{"an age alone carries no interval", ageOnlyPurge, ageOnlyPurge, false},
		{"an age and an interval", twoFieldPurge, ageOnlyPurge, true},
		{"an already-phased value is an age plus extras", ageOnlyPurge + " 06:00:00 02:00:00", ageOnlyPurge, true},
		{"surrounding whitespace is not a field", "  " + ageOnlyPurge + "  ", ageOnlyPurge, false},
		{"slapd's internal respacing is not a field count", ageOnlyPurge + "\t1+00:00", ageOnlyPurge, true},
		{"empty in, empty out", "", "", false},
		{"whitespace-only in, empty out", testBlank, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			age, extra := splitPurgeAge(tc.in)
			if age != tc.wantAge {
				t.Fatalf("splitPurgeAge(%q) age = %q, want %q", tc.in, age, tc.wantAge)
			}
			if extra != tc.wantExtra {
				t.Fatalf("splitPurgeAge(%q) extra = %v, want %v", tc.in, extra, tc.wantExtra)
			}
		})
	}
}

// The drift guard the milestone that wrote composePurge could not write: the
// age this file extracts from defaultAccesslogPurge must be the same string the
// composition then hands to composePurge. If somebody retunes the default
// window and the splitter stops agreeing with it, a rotating cluster silently
// starts purging on a different retention than a non-rotating one.
func TestSplitPurgeAge_AgreesWithTheShippedDefault(t *testing.T) {
	age, extra := splitPurgeAge(defaultAccesslogPurge)
	if !extra {
		t.Fatalf("defaultAccesslogPurge = %q carries no interval; the split has nothing to do",
			defaultAccesslogPurge)
	}
	if age != defaultPurgeAge {
		t.Fatalf("age half of defaultAccesslogPurge = %q, want %q", age, defaultPurgeAge)
	}
	composed := composePurge(age, 3*time.Hour, time.Hour)
	if !strings.HasPrefix(composed, defaultPurgeAge+" ") {
		t.Fatalf("composePurge(%q, …) = %q, which does not start with the extracted age",
			age, composed)
	}
	if want := defaultPurgeAge + " 03:00:00 01:00:00"; composed != want {
		t.Fatalf("composePurge = %q, want %q", composed, want)
	}
}

func TestPlanPodPurge(t *testing.T) {
	cases := []struct {
		name         string
		plan         rotationPlan
		field        string
		ordinal      int32
		wantValue    string
		wantFallback string
		wantWithheld bool
	}{
		{
			name:      "rotation off: byte-identical to today, the operator default",
			plan:      rotationPlan{},
			field:     "",
			ordinal:   1,
			wantValue: defaultAccesslogPurge,
		},
		{
			name:      "rotation off: an explicit two-field value passes through verbatim",
			plan:      rotationPlan{},
			field:     twoFieldPurge,
			ordinal:   2,
			wantValue: twoFieldPurge,
		},
		{
			name:      "rotation off: none still opts out",
			plan:      rotationPlan{},
			field:     noneSentinel,
			ordinal:   0,
			wantValue: "",
		},
		{
			name:         "rotation on, unset field: the default age on the cluster's grid, pod 0",
			plan:         activePlan(6*time.Hour, 3),
			field:        "",
			ordinal:      0,
			wantValue:    defaultPurgeAge + " 18:00:00 00:00:00",
			wantFallback: defaultPurgeAge + " 18:00:00",
		},
		{
			name:         "rotation on, pod 1 sits one slot along",
			plan:         activePlan(6*time.Hour, 3),
			field:        "",
			ordinal:      1,
			wantValue:    defaultPurgeAge + " 18:00:00 06:00:00",
			wantFallback: defaultPurgeAge + " 18:00:00",
		},
		{
			name:         "rotation on, pod 2 sits two slots along",
			plan:         activePlan(6*time.Hour, 3),
			field:        "",
			ordinal:      2,
			wantValue:    defaultPurgeAge + " 18:00:00 12:00:00",
			wantFallback: defaultPurgeAge + " 18:00:00",
		},
		{
			name:         "rotation on, an age-only field is the shape the CR should carry",
			plan:         activePlan(time.Hour, 2),
			field:        ageOnlyPurge,
			ordinal:      1,
			wantValue:    ageOnlyPurge + " 02:00:00 01:00:00",
			wantFallback: ageOnlyPurge + " 02:00:00",
		},
		{
			name:      "rotation on, none still opts out: a schedule must not resurrect a purge",
			plan:      activePlan(time.Hour, 2),
			field:     noneSentinel,
			ordinal:   1,
			wantValue: "",
		},
		{
			name:         "rotation on, a declared interval is refused rather than overridden",
			plan:         activePlan(time.Hour, 2),
			field:        twoFieldPurge,
			ordinal:      1,
			wantWithheld: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planPodPurge(tc.plan, sdWithPurge(tc.field), tc.ordinal)
			if got.Withheld != tc.wantWithheld {
				t.Fatalf("Withheld = %v, want %v", got.Withheld, tc.wantWithheld)
			}
			if tc.wantWithheld {
				if got.Reason != reasonPurgeIntervalDeclared {
					t.Fatalf("Reason = %q, want %q", got.Reason, reasonPurgeIntervalDeclared)
				}
				if !strings.Contains(got.Message, "accesslogPurge") {
					t.Fatalf("message %q does not name the field at fault", got.Message)
				}
				return
			}
			if got.Value != tc.wantValue {
				t.Fatalf("Value = %q, want %q", got.Value, tc.wantValue)
			}
			if got.Fallback != tc.wantFallback {
				t.Fatalf("Fallback = %q, want %q", got.Fallback, tc.wantFallback)
			}
		})
	}
}

// The property the third amendment warns about, asserted directly: a driver
// that wrote one phase for every pod would look identical from the outside, so
// the distinctness is checked here rather than inferred from the arithmetic.
func TestPlanPodPurge_EveryPodGetsItsOwnPhase(t *testing.T) {
	const n = 4
	plan := activePlan(90*time.Minute, n)
	seen := map[string]int32{}
	for ord := range int32(n) {
		v := planPodPurge(plan, sdWithPurge(""), ord).Value
		fields := strings.Fields(v)
		if len(fields) != 3 {
			t.Fatalf("pod %d: %q has %d fields, want 3 (<age> <interval> <phase>)", ord, v, len(fields))
		}
		if fields[1] != "06:00:00" {
			t.Fatalf("pod %d: interval = %q, want 06:00:00 (replicas × slotLength)", ord, fields[1])
		}
		if prev, dup := seen[fields[2]]; dup {
			t.Fatalf("pods %d and %d both purge at phase %q — the stagger never happened",
				prev, ord, fields[2])
		}
		seen[fields[2]] = ord
	}
}

// Databases ALIGN, they do not spread (ADR-037, second amendment): a pod purges
// every one of its accesslogs at its own phase, concentrating its disturbance
// into the one window where it is not taking writes.
func TestPlanPodPurge_DatabasesAlignOnOnePodsPhase(t *testing.T) {
	plan := activePlan(time.Hour, 3)
	dbA := sdWithPurge("")
	dbA.Name = "alpha"
	dbB := sdWithPurge(ageOnlyPurge)
	dbB.Name = "beta"

	for ord := range int32(3) {
		a := strings.Fields(planPodPurge(plan, dbA, ord).Value)
		b := strings.Fields(planPodPurge(plan, dbB, ord).Value)
		if len(a) != 3 || len(b) != 3 {
			t.Fatalf("pod %d: want three-field values, got %v and %v", ord, a, b)
		}
		if a[2] != b[2] {
			t.Fatalf("pod %d: database phases differ (%q vs %q); databases must align, not spread",
				ord, a[2], b[2])
		}
	}
}

// Capability handling, decided: attempt, then report. A stock slapd rejects the
// third value outright, and the rejection is a config-parse error carrying the
// attribute's name, not a transport failure.
func TestIsPurgeArityRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a rejection", nil, false},
		{
			name: "the stock parser's own words",
			err: ldap.NewError(ldap.LDAPResultOther,
				errors.New("<olcAccessLogPurge> extra cruft after <age> <interval>")),
			want: true,
		},
		{
			name: "wrapped, as the ensure path returns it",
			err: ldap.NewError(ldap.LDAPResultUnwillingToPerform,
				errors.New("olcAccessLogPurge: value #0 invalid per syntax: "+
					"<olcAccessLogPurge> extra cruft after <age> <interval>")),
			want: true,
		},
		{
			name: "case is not load-bearing",
			err:  errors.New("<olcAccessLogPurge> Extra Cruft after <age> <interval>"),
			want: true,
		},
		{
			name: "a phase at or above the interval is OUR bug, not a missing patch",
			err: ldap.NewError(ldap.LDAPResultOther,
				errors.New("<olcAccessLogPurge> phase must be less than interval")),
			want: false,
		},
		{"a dial failure is not a rejection", errors.New("dial tcp: connection refused"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPurgeArityRejection(tc.err); got != tc.want {
				t.Fatalf("isPurgeArityRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAggregateAccesslogPurgeConverged(t *testing.T) {
	withheld := &purgeWithhold{
		Reason:  reasonPurgeIntervalDeclared,
		Message: "spec.replication.accesslogPurge carries an interval",
	}
	cases := []struct {
		name       string
		active     bool
		withheld   *purgeWithhold
		obs        []purgePhaseObservation
		wantStatus metav1.ConditionStatus
		wantReason string
		mustSay    []string
	}{
		{
			name:       "rotation off: nothing to say, and True is the honest answer",
			active:     false,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonRotationDisabled,
		},
		{
			name:       "rotation on and every pod took its phase",
			active:     true,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}, {pod: testPod1, assessed: true}},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonConverged,
			mustSay:    []string{"2"},
		},
		{
			name:       "a declared interval withholds the whole database",
			active:     true,
			withheld:   withheld,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPurgeIntervalDeclared,
			mustSay:    []string{"accesslogPurge"},
		},
		{
			name:   "an unpatched image names the pod that refused the phase",
			active: true,
			obs: []purgePhaseObservation{
				{pod: testPod0, assessed: true},
				{pod: testPod1, assessed: true, unsupported: true},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPurgePhaseUnsupported,
			mustSay:    []string{testPod1},
		},
		{
			name:   "an unread pod is noted and never indicted (ADR-025)",
			active: true,
			obs: []purgePhaseObservation{
				{pod: testPod0, assessed: true},
				{pod: testPod1},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonConverged,
			mustSay:    []string{testPod1, "not assessed"},
		},
		{
			name:       "no pod could be read at all: Unknown, never False",
			active:     true,
			obs:        []purgePhaseObservation{{pod: testPod0}, {pod: testPod1}},
			wantStatus: metav1.ConditionUnknown,
			wantReason: reasonNoReachablePod,
		},
		{
			name:       "rotation on and no pods visited yet",
			active:     true,
			obs:        nil,
			wantStatus: metav1.ConditionUnknown,
			wantReason: reasonNoReachablePod,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, msg := aggregateAccesslogPurgeConverged(tc.active, tc.withheld, tc.obs)
			if status != tc.wantStatus {
				t.Fatalf("status = %v, want %v", status, tc.wantStatus)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if msg == "" {
				t.Fatal("message is empty")
			}
			for _, want := range tc.mustSay {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestAccesslogPurgeConvergedFor(t *testing.T) {
	sd := sdWithPurge("")
	sd.Generation = 4
	cond := accesslogPurgeConvergedFor(sd, true, nil,
		[]purgePhaseObservation{{pod: testPod0, assessed: true, unsupported: true}})

	if cond.Type != accesslogPurgeConvergedCondition {
		t.Fatalf("Type = %q, want %q", cond.Type, accesslogPurgeConvergedCondition)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False", cond.Status)
	}
	if cond.ObservedGeneration != 4 {
		t.Fatalf("ObservedGeneration = %d, want 4", cond.ObservedGeneration)
	}
}

// Rotation off must change nothing at all about the value written — the
// milestone's one hard compatibility requirement, asserted against the resolver
// the pre-rotation code used directly.
func TestPlanPodPurge_RotationOffIsByteIdenticalToTheOldResolution(t *testing.T) {
	for _, field := range []string{"", testBlank, noneSentinel, noneMixedCase, twoFieldPurge, ageOnlyPurge} {
		sd := sdWithPurge(field)
		for ord := range int32(4) {
			got := planPodPurge(rotationPlan{}, sd, ord)
			if got.Withheld {
				t.Fatalf("field %q, pod %d: withheld with rotation off", field, ord)
			}
			if want := desiredAccesslogPurge(sd); got.Value != want {
				t.Fatalf("field %q, pod %d: Value = %q, want %q", field, ord, got.Value, want)
			}
			if got.Fallback != "" {
				t.Fatalf("field %q, pod %d: Fallback = %q, want none with rotation off",
					field, ord, got.Fallback)
			}
		}
	}
}

// A nil database is the shape every other resolver in this package tolerates.
func TestPlanPodPurge_NilDatabase(t *testing.T) {
	if got := planPodPurge(rotationPlan{}, nil, 0); got.Value != defaultAccesslogPurge {
		t.Fatalf("Value = %q, want %q", got.Value, defaultAccesslogPurge)
	}
}
