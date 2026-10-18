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

// ADR-024 R6 and ADR-037's ninth amendment: olcAccessLogPurge is one slapd
// directive with THREE owners, and this file holds the composition and the
// condition that reports it.
//
//	<retention>   the database's   — how far behind a consumer may fall
//	<interval>    the cluster's    — how often the cost is paid, by the POD
//	<phase>       derived          — which pod pays it when; nobody declares it
//
// The milestone that wrote this file removed a withhold, and the absence is the
// point: with no declared interval on a database there is nothing to refuse, so
// there is no way for a database to come out of a pass unpurged.

const (
	// wantDefaultRetentionRendered pins defaultPurgeRetention's rendering,
	// spelled out rather than derived from the formatter it checks.
	wantDefaultRetentionRendered = "7+00:00:00"
	// wantDefaultIntervalRendered pins defaultPurgeInterval's.
	wantDefaultIntervalRendered = "1+00:00:00"
)

func activePlan(slot time.Duration, replicas int32) rotationPlan {
	return rotationPlan{
		Active:     true,
		SlotLength: slot,
		Interval:   time.Duration(replicas) * slot,
		Replicas:   replicas,
	}
}

// flatPlan is form A: one interval, every pod purging together, no phase.
func flatPlan(interval time.Duration) rotationPlan {
	return rotationPlan{Interval: interval, Replicas: 3}
}

// The defaults are the whole point of R5's half of this: an unpurged journal
// grows to its map ceiling and then stops taking writes — and because the
// accesslog overlay sits in the DATA write path, data writes stop with it.
//
// Decomposing the field must not MOVE either default. The old two-token default
// was "7+00:00 1+00:00", and the two typed constants must still render it.
func TestThePurgeDefaultsDidNotMoveWhenTheFieldWasDecomposed(t *testing.T) {
	if got := formatLogAge(defaultPurgeRetention); got != wantDefaultRetentionRendered {
		t.Errorf("defaultPurgeRetention renders as %q, want %q — the old default kept 7 days",
			got, wantDefaultRetentionRendered)
	}
	if got := formatLogAge(defaultPurgeInterval); got != wantDefaultIntervalRendered {
		t.Errorf("defaultPurgeInterval renders as %q, want %q — the old default swept daily",
			got, wantDefaultIntervalRendered)
	}
	if defaultPurgeRetention <= 0 || defaultPurgeInterval <= 0 {
		t.Fatal("a non-positive default would mean no purge at all, which is a slow-motion outage")
	}
}

func TestResolveRetention(t *testing.T) {
	cases := []struct {
		name       string
		sd         *slapdDatabaseFixture
		want       time.Duration
		wantDefect bool
		mustSay    string
	}{
		{
			name: "a nil database takes the operator's default",
			sd:   nil,
			want: defaultPurgeRetention,
		},
		{
			name: "an unset block takes the operator's default",
			sd:   &slapdDatabaseFixture{},
			want: defaultPurgeRetention,
		},
		{
			name: "a block with no retention takes the operator's default",
			sd:   &slapdDatabaseFixture{enabled: new(true)},
			want: defaultPurgeRetention,
		},
		{
			name: "enabled false means never purge, and is NOT a defect",
			sd:   &slapdDatabaseFixture{enabled: new(false)},
			want: 0,
		},
		{
			// The inversion `enabled` exists to avoid: if it had been named
			// for the retention, `enabled: false` would read as "purge
			// everything". It is named for the ACTION, so false means never.
			name: "enabled false wins over a declared retention",
			sd:   &slapdDatabaseFixture{enabled: new(false), retention: 48 * time.Hour},
			want: 0,
		},
		{
			name: "a declared retention is used as declared",
			sd:   &slapdDatabaseFixture{retention: 48 * time.Hour},
			want: 48 * time.Hour,
		},
		{
			name:       "a zero retention is reported, and the DEFAULT is written anyway",
			sd:         &slapdDatabaseFixture{retention: 0, retentionSet: true},
			want:       defaultPurgeRetention,
			wantDefect: true,
			mustSay:    "enabled: false",
		},
		{
			name:       "a negative retention is reported, and the default is written anyway",
			sd:         &slapdDatabaseFixture{retention: -time.Hour, retentionSet: true},
			want:       defaultPurgeRetention,
			wantDefect: true,
		},
		{
			// olcAccessLogPurge's fields are [ddd+]hh:mm[:ss]. "500ms" passes
			// the API server's grammar rule and has no representation on the
			// server, which is the half of the check the CEL rule cannot make.
			name:       "a sub-second retention is reported, and the default is written anyway",
			sd:         &slapdDatabaseFixture{retention: 1500 * time.Millisecond, retentionSet: true},
			want:       defaultPurgeRetention,
			wantDefect: true,
			mustSay:    "whole number of seconds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, defect := resolveRetention(tc.sd.build())
			if got != tc.want {
				t.Fatalf("retention = %v, want %v", got, tc.want)
			}
			if (defect != nil) != tc.wantDefect {
				t.Fatalf("defect = %v, want defect = %v", defect, tc.wantDefect)
			}
			if defect == nil {
				return
			}
			if defect.Reason != reasonRetentionNotUsable {
				t.Fatalf("Reason = %q, want %q", defect.Reason, reasonRetentionNotUsable)
			}
			if !strings.Contains(defect.Message, "accesslogPurge.retention") {
				t.Fatalf("message %q does not name the field at fault", defect.Message)
			}
			if tc.mustSay != "" && !strings.Contains(defect.Message, tc.mustSay) {
				t.Fatalf("message %q does not mention %q", defect.Message, tc.mustSay)
			}
		})
	}
}

// THE DEFECT, AS A REGRESSION GUARD. Measured on a fresh three-site mesh: a
// database declaring an interval, on a cluster CREATED with rotation already
// on, ended up with NO olcAccessLogPurge at all on every pod at every site. The
// withhold assumed there was an existing value to keep.
//
// The decomposition dissolves it rather than patching it — there is no declared
// interval to refuse — and this is the assertion that says so, over every state
// a database and a cluster can be in together. The one exception is the
// explicit opt-out, which is a user decision rather than an accident.
func TestEveryPurgingDatabaseAlwaysGetsAPurgeValue(t *testing.T) {
	plans := map[string]rotationPlan{
		"rotation off, default interval": flatPlan(defaultPurgeInterval),
		"form A, declared interval":      flatPlan(6 * time.Hour),
		"form B, rotating":               activePlan(6*time.Hour, 3),
		"form B, two pods":               activePlan(90*time.Minute, 2),
	}
	dbs := map[string]*slapdDatabaseFixture{
		"nothing declared":        {},
		"retention declared":      {retention: 48 * time.Hour},
		"enabled true explicitly": {enabled: new(true)},
		"a retention it cannot have": {
			retention: 500 * time.Millisecond, retentionSet: true,
		},
		"a zero retention": {retention: 0, retentionSet: true},
	}

	for pn, plan := range plans {
		for dn, db := range dbs {
			t.Run(pn+"/"+dn, func(t *testing.T) {
				for ord := range max(plan.Replicas, 1) {
					got := planPodPurge(plan, db.build(), ord)
					if got.Value == "" {
						t.Fatalf("pod %d got NO purge value. An unpurged journal grows to its "+
							"map ceiling and then stops DATA writes — the outage ADR-024 R4 "+
							"exists to prevent, and the one this milestone removed", ord)
					}
					if n := len(strings.Fields(got.Value)); n < 2 {
						t.Fatalf("pod %d: %q has %d field(s); slapd needs at least "+
							"<retention> <interval>", ord, got.Value, n)
					}
				}
			})
		}
	}
}

func TestPlanPodPurge(t *testing.T) {
	const (
		dayRetention = "2+00:00:00"
		weekRetained = wantDefaultRetentionRendered
	)
	cases := []struct {
		name         string
		plan         rotationPlan
		sd           *slapdDatabaseFixture
		ordinal      int32
		wantValue    string
		wantFallback string
	}{
		{
			name:      "form A default: the shape every cluster has always run",
			plan:      flatPlan(defaultPurgeInterval),
			sd:        &slapdDatabaseFixture{},
			ordinal:   1,
			wantValue: weekRetained + " " + wantDefaultIntervalRendered,
		},
		{
			name:      "form A, a declared interval and a declared retention",
			plan:      flatPlan(6 * time.Hour),
			sd:        &slapdDatabaseFixture{retention: 48 * time.Hour},
			ordinal:   2,
			wantValue: dayRetention + " 06:00:00",
		},
		{
			name:      "form A carries NO phase: every pod purges together",
			plan:      flatPlan(6 * time.Hour),
			sd:        &slapdDatabaseFixture{},
			ordinal:   2,
			wantValue: weekRetained + " 06:00:00",
		},
		{
			name:      "purging off: no attribute at all, whatever the schedule says",
			plan:      flatPlan(6 * time.Hour),
			sd:        &slapdDatabaseFixture{enabled: new(false)},
			ordinal:   0,
			wantValue: "",
		},
		{
			name:         "form B, pod 0 sits on the anchor",
			plan:         activePlan(6*time.Hour, 3),
			sd:           &slapdDatabaseFixture{},
			ordinal:      0,
			wantValue:    weekRetained + " 18:00:00 00:00:00",
			wantFallback: weekRetained + " 18:00:00",
		},
		{
			name:         "form B, pod 1 sits one slot along",
			plan:         activePlan(6*time.Hour, 3),
			sd:           &slapdDatabaseFixture{},
			ordinal:      1,
			wantValue:    weekRetained + " 18:00:00 06:00:00",
			wantFallback: weekRetained + " 18:00:00",
		},
		{
			name:         "form B, pod 2 sits two slots along",
			plan:         activePlan(6*time.Hour, 3),
			sd:           &slapdDatabaseFixture{},
			ordinal:      2,
			wantValue:    weekRetained + " 18:00:00 12:00:00",
			wantFallback: weekRetained + " 18:00:00",
		},
		{
			name:         "form B with a declared retention",
			plan:         activePlan(time.Hour, 2),
			sd:           &slapdDatabaseFixture{retention: 48 * time.Hour},
			ordinal:      1,
			wantValue:    dayRetention + " 02:00:00 01:00:00",
			wantFallback: dayRetention + " 02:00:00",
		},
		{
			name:      "form B, purging off: a schedule must not resurrect a purge",
			plan:      activePlan(time.Hour, 2),
			sd:        &slapdDatabaseFixture{enabled: new(false)},
			ordinal:   1,
			wantValue: "",
		},
		{
			// The defect's negation, in one row. This database used to be
			// WITHHELD; now it is written, from the operator's default
			// retention, with the fault reported on the condition.
			name:         "form B with an unusable retention: written anyway, from the default",
			plan:         activePlan(time.Hour, 2),
			sd:           &slapdDatabaseFixture{retention: 0, retentionSet: true},
			ordinal:      1,
			wantValue:    weekRetained + " 02:00:00 01:00:00",
			wantFallback: weekRetained + " 02:00:00",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planPodPurge(tc.plan, tc.sd.build(), tc.ordinal)
			if got.Value != tc.wantValue {
				t.Fatalf("Value = %q, want %q", got.Value, tc.wantValue)
			}
			if got.Fallback != tc.wantFallback {
				t.Fatalf("Fallback = %q, want %q", got.Fallback, tc.wantFallback)
			}
		})
	}
}

// A fallback exists exactly when there is a phase to fall back FROM. Writing
// one where the value carries no phase would let the fallback path fire on a
// failure it does not explain; omitting one where the value does carry a phase
// would leave an unpatched pod with no olcAccessLogPurge at all — the
// unpurged-journal failure again, by another route.
func TestPlanPodPurge_FallbackExistsExactlyWhenThereIsAPhase(t *testing.T) {
	for _, plan := range []rotationPlan{
		flatPlan(defaultPurgeInterval), flatPlan(6 * time.Hour),
		activePlan(6*time.Hour, 3), activePlan(time.Minute, 2),
	} {
		for ord := range int32(3) {
			got := planPodPurge(plan, (&slapdDatabaseFixture{}).build(), ord)
			hasPhase := len(strings.Fields(got.Value)) == 3
			if hasPhase != (got.Fallback != "") {
				t.Fatalf("plan %+v pod %d: Value = %q (phase: %v) but Fallback = %q",
					plan, ord, got.Value, hasPhase, got.Fallback)
			}
			if hasPhase && got.Fallback != strings.Join(strings.Fields(got.Value)[:2], " ") {
				t.Fatalf("plan %+v pod %d: Fallback %q is not Value %q without its phase",
					plan, ord, got.Fallback, got.Value)
			}
		}
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
		v := planPodPurge(plan, (&slapdDatabaseFixture{}).build(), ord).Value
		fields := strings.Fields(v)
		if len(fields) != 3 {
			t.Fatalf("pod %d: %q has %d fields, want 3 (<retention> <interval> <phase>)",
				ord, v, len(fields))
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
	dbA := (&slapdDatabaseFixture{}).build()
	dbA.Name = "alpha"
	dbB := (&slapdDatabaseFixture{retention: 48 * time.Hour}).build()
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
			// Captured from slaptest on an unpatched 2.6.10 image: the
			// slapd.conf keyword path names <logpurge>, the cn=config path
			// names <olcAccessLogPurge>. Matching on the attribute name would
			// therefore miss half the cases for no gain — "extra cruft" is the
			// invariant, and it is what the matcher looks for.
			name: "the keyword spelling, as slaptest emits it",
			err:  errors.New("/t.conf: line 10: <logpurge> extra cruft after <age> <interval>."),
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
	defect := &purgeDefect{
		Reason:  reasonRetentionNotUsable,
		Message: "spec.replication.accesslogPurge.retention is 0s",
	}
	cases := []struct {
		name       string
		plan       rotationPlan
		defect     *purgeDefect
		purging    bool
		obs        []purgePhaseObservation
		wantStatus metav1.ConditionStatus
		wantReason string
		mustSay    []string
		mustNotSay []string
	}{
		{
			name:       "purging switched off: True, and the message says the journal grows",
			plan:       flatPlan(defaultPurgeInterval),
			purging:    false,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonPurgeDisabled,
			mustSay:    []string{"enabled is false", "unbounded"},
		},
		{
			name:       "rotation off: every pod purges together, and that is a good state",
			plan:       flatPlan(defaultPurgeInterval),
			purging:    true,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonConverged,
			mustSay:    []string{wantDefaultIntervalRendered, "together"},
		},
		{
			name:       "rotation on and every pod took its phase",
			plan:       activePlan(6*time.Hour, 2),
			purging:    true,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}, {pod: testPod1, assessed: true}},
			wantStatus: metav1.ConditionTrue,
			wantReason: reasonConverged,
			mustSay:    []string{"2", "06:00:00"},
		},
		{
			name:       "an unusable retention is reported against the field, not the pods",
			plan:       activePlan(6*time.Hour, 2),
			defect:     defect,
			purging:    true,
			obs:        []purgePhaseObservation{{pod: testPod0, assessed: true}},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonRetentionNotUsable,
			mustSay:    []string{"accesslogPurge.retention"},
		},
		{
			// The same lesson the withhold taught, carried forward: a defect
			// is decided from the SPEC, so appending "not assessed: slapd-0,
			// slapd-1" would make a field fault read as unreachable pods.
			name:       "a spec defect does not blame the pods",
			plan:       activePlan(6*time.Hour, 2),
			defect:     defect,
			purging:    true,
			obs:        []purgePhaseObservation{{pod: testPod0}, {pod: testPod1}},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonRetentionNotUsable,
			mustNotSay: []string{"not assessed", testPod0},
		},
		{
			name:    "an unpatched image names the pod that refused the phase",
			plan:    activePlan(6*time.Hour, 2),
			purging: true,
			obs: []purgePhaseObservation{
				{pod: testPod0, assessed: true},
				{pod: testPod1, assessed: true, unsupported: true},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonPurgePhaseUnsupported,
			mustSay:    []string{testPod1},
		},
		{
			name:    "an unread pod is noted and never indicted (ADR-025)",
			plan:    activePlan(6*time.Hour, 2),
			purging: true,
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
			plan:       activePlan(6*time.Hour, 2),
			purging:    true,
			obs:        []purgePhaseObservation{{pod: testPod0}, {pod: testPod1}},
			wantStatus: metav1.ConditionUnknown,
			wantReason: reasonNoReachablePod,
		},
		{
			name:       "rotation on and no pods visited yet",
			plan:       activePlan(6*time.Hour, 2),
			purging:    true,
			obs:        nil,
			wantStatus: metav1.ConditionUnknown,
			wantReason: reasonNoReachablePod,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, msg := aggregateAccesslogPurgeConverged(tc.plan, tc.defect, tc.purging, tc.obs)
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
			for _, unwanted := range tc.mustNotSay {
				if strings.Contains(msg, unwanted) {
					t.Fatalf("message %q mentions %q, which misdirects the reader", msg, unwanted)
				}
			}
		})
	}
}

func TestAccesslogPurgeConvergedFor(t *testing.T) {
	sd := (&slapdDatabaseFixture{}).build()
	sd.Generation = 4
	cond := accesslogPurgeConvergedFor(sd, activePlan(6*time.Hour, 1),
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

// accesslogPurgeConvergedFor resolves `purging` from the CR itself, so the
// opt-out must reach the condition without the caller passing it separately.
func TestAccesslogPurgeConvergedFor_ReadsTheOptOutFromTheCR(t *testing.T) {
	cond := accesslogPurgeConvergedFor(sdPurgeDisabled(), activePlan(6*time.Hour, 1),
		[]purgePhaseObservation{{pod: testPod0, assessed: true}})
	if cond.Status != metav1.ConditionTrue || cond.Reason != reasonPurgeDisabled {
		t.Fatalf("condition = %v/%q, want True/%s", cond.Status, cond.Reason, reasonPurgeDisabled)
	}
}

// A nil database is the shape every other resolver in this package tolerates,
// and it must still produce a purge value rather than nothing.
func TestPlanPodPurge_NilDatabase(t *testing.T) {
	got := planPodPurge(flatPlan(defaultPurgeInterval), nil, 0)
	want := wantDefaultRetentionRendered + " " + wantDefaultIntervalRendered
	if got.Value != want {
		t.Fatalf("Value = %q, want %q", got.Value, want)
	}
}
