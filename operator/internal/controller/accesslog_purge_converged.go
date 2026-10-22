/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// AccesslogPurgeConverged: did every pod we could read take the purge schedule
// this database and its cluster between them declare? (ADR-037.)
//
// Why this condition sits on the SlapdDatabase and not on the SlapdCluster:
// olcAccessLogPurge is an attribute of ONE database's accesslog overlay, written
// per (pod, database). The two things that can go wrong are both per-database
// facts — this database's own spec.replication.accesslogPurge carrying an
// interval the cluster's schedule now owns, and this database's overlay being
// refused by a pod whose slapd cannot express a phase. The cluster-wide half of
// the decision — whether the rotation is configured at all, and on what grid —
// is reported separately as RotationConfigured on the SlapdCluster, which is
// where spec.rotation lives.
//
// Named positively, like every other condition this operator publishes: True is
// the good case, and "rotation is off" is a good case.
const accesslogPurgeConvergedCondition = "AccesslogPurgeConverged"

// The reasons AccesslogPurgeConverged publishes beyond the shared
// reasonConverged (names.go) and reasonNoReachablePod (data_present.go).
const (
	// reasonPurgePhaseUnsupported: a pod's slapd refused the three-field value
	// outright ("extra cruft after <age> <interval>"). Its accesslog overlay is
	// a stock build without the accesslog-purge-phase patch. Decided handling
	// (ADR-037, PURGE-ROTATION-PLAN Phase 3): attempt, then report. No version
	// probing, no marker convention — the failure is loud, bounded, and names
	// exactly what to fix.
	reasonPurgePhaseUnsupported = "PurgePhaseUnsupported"
	// reasonRetentionNotUsable: spec.replication.accesslogPurge.retention has
	// no representation on the server — non-positive, or finer than the whole
	// seconds olcAccessLogPurge counts in. The API server's rule checks the
	// duration GRAMMAR and nothing else, so this half is the operator's.
	//
	// It is REPORTED, and the operator's default retention is written anyway.
	// That is the one thing this file may not trade: a database must always
	// come out of a pass with a purge value, whatever the user got wrong. The
	// alternative is the defect ADR-037's ninth amendment records — a withheld
	// value on a cluster that never had one, and an unpurged journal that grows
	// to its map ceiling and then stops data writes.
	reasonRetentionNotUsable = "RetentionNotUsable"
	// reasonPurgeDisabled: spec.replication.accesslogPurge.enabled is false.
	// No purging on this journal at all; the user has taken responsibility for
	// its growth. A good state, reported as True.
	reasonPurgeDisabled = "PurgeDisabled"
	// reasonRotationNotInForce: spec.rotation is DECLARED and the cluster's
	// rotation gate refused it, so nothing is staggered and every pod is on the
	// fallback interval. True, because that is a bounded journal on every pod —
	// which is all this condition is about.
	//
	// It exists because the alternative is reporting it as reasonConverged, and
	// the only honest message for reasonConverged says the rotation is OFF. It
	// is not off: the user set the field, and the thing to go and read is
	// RotationConfigured on the SlapdCluster. One reason per state, so that
	// "nothing is staggered because you asked for nothing" and "nothing is
	// staggered because what you asked for was refused" cannot be read as the
	// same sentence.
	reasonRotationNotInForce = "RotationNotInForce"
)

// defaultPurgeRetention is how long an entry stays in a change journal when the
// database declares nothing: 7 days. The window is how long a consumer may stay
// away and still resume from a delta instead of a full refresh, so it is sized
// to survive a weekend-plus outage.
//
// It is the retention half of the old two-token default "7+00:00 1+00:00",
// unchanged. Decomposing the field must not move a value every cluster is
// already running.
const defaultPurgeRetention = 7 * 24 * time.Hour

// purgeDefect is a per-database fault: something this database declares has no
// representation on the server. It is REPORTED, never acted on by withholding —
// the purge value is written from the operator's default instead, because an
// unpurged journal is an outage and a wrong retention is a tuning error.
type purgeDefect struct {
	Reason  string
	Message string
}

// purgePodPlan is the olcAccessLogPurge decision for one (pod, database).
//
// Value is what to write; "" means the attribute must be absent, which is the
// explicit `enabled: false` opt-out and nothing else.
//
// Fallback is the two-field value to fall back to when the server refuses
// Value's third field. It is empty whenever Value carries no phase, so the
// fallback path cannot fire where there is nothing to fall back from. A fresh
// cluster on an unpatched image has no olcAccessLogPurge at all, and writing
// nothing there would restore the unpurged-journal failure the default exists
// to prevent — so the fallback is written and the condition reports what was
// lost, rather than the pod being left bare.
//
// There is no Withheld field any more, and its absence is the milestone: with
// the interval owned by the cluster there is no declared interval to refuse, so
// there is nothing to withhold, so a database cannot end up unpurged.
type purgePodPlan struct {
	Value    string
	Fallback string
}

// purgePhaseObservation is one pod's answer.
//
// assessed is ADR-025's rule in a field: a pod that could not be dialled, bound
// or read is not evidence and must never make the condition False. unsupported
// is only meaningful when assessed.
type purgePhaseObservation struct {
	pod         string
	assessed    bool
	unsupported bool
}

// resolveRetention is the database's half of olcAccessLogPurge: what to KEEP.
// Pure.
//
// Three answers, and the second return distinguishes the two that look alike
// from outside:
//
//   - purging off (`enabled: false`) — zero, no defect. The user asked for no
//     purge and takes the growth.
//   - a usable retention — the declared duration, no defect.
//   - a declared retention with no representation on the server — the
//     OPERATOR'S DEFAULT plus a defect to report. Never zero: falling back to
//     "no purge" because a duration was mistyped is how a tuning error becomes
//     an outage.
func resolveRetention(sd *ldapv1alpha1.SlapdDatabase) (time.Duration, *purgeDefect) {
	if sd == nil || sd.Spec.Replication.AccesslogPurge == nil {
		return defaultPurgeRetention, nil
	}
	p := sd.Spec.Replication.AccesslogPurge
	if p.Enabled != nil && !*p.Enabled {
		return 0, nil
	}
	if p.Retention == nil {
		return defaultPurgeRetention, nil
	}

	d := p.Retention.Duration
	switch {
	case d <= 0:
		return defaultPurgeRetention, &purgeDefect{
			Reason: reasonRetentionNotUsable,
			Message: fmt.Sprintf("spec.replication.accesslogPurge.retention is %s. The API "+
				"server accepts it — its rule checks the duration GRAMMAR and nothing else — "+
				"but a retention of zero or less would purge the journal out from under every "+
				"consumer. The operator's default of %s is in force instead; set a positive "+
				"retention, or set enabled: false if you really want no purging at all",
				d, formatLogAge(defaultPurgeRetention)),
		}
	case d%time.Second != 0:
		return defaultPurgeRetention, &purgeDefect{
			Reason: reasonRetentionNotUsable,
			Message: fmt.Sprintf("spec.replication.accesslogPurge.retention is %s, which is not "+
				"a whole number of seconds. olcAccessLogPurge's fields are [ddd+]hh:mm[:ss], so "+
				"it has no representation on the server. The operator's default of %s is in "+
				"force instead", d, formatLogAge(defaultPurgeRetention)),
		}
	}
	return d, nil
}

// planPodPurge composes the olcAccessLogPurge value for one pod of one
// database. Pure; the one place the three owners of the attribute meet.
//
// The database brings the retention, the cluster brings the interval, and the
// grid brings the phase. THERE IS ALWAYS AN INTERVAL — declared in form A,
// derived as n·t in form B, the operator's default otherwise — so the only way
// out of here with no value at all is the explicit `enabled: false`.
//
// With rotation on, ALL DATABASES ON A POD SHARE THAT POD'S PHASE — aligned,
// not spread (ADR-037, second amendment, "Databases align, they do not
// spread"). The phase is a function of the ordinal alone; the database enters
// only through the retention. Spreading the databases over n·D slots would
// disturb every pod throughout the period and defeat the purpose, which is to
// concentrate a pod's disturbance into the window where it is not writing.
func planPodPurge(plan rotationPlan, sd *ldapv1alpha1.SlapdDatabase, ordinal int32) purgePodPlan {
	retention, _ := resolveRetention(sd)
	if retention <= 0 {
		// `enabled: false`. A schedule must not resurrect a purge the user
		// turned off.
		return purgePodPlan{}
	}
	if !plan.PhasesActive {
		return purgePodPlan{Value: composePurge(retention, plan.Interval, nil)}
	}

	phase := purgePhase(ordinal, plan.Replicas, plan.SlotLength)
	return purgePodPlan{
		Value:    composePurge(retention, plan.Interval, &phase),
		Fallback: composePurge(retention, plan.Interval, nil),
	}
}

// isPurgeArityRejection reports whether err is a stock slapd refusing the third
// value of olcAccessLogPurge, as opposed to any other failure.
//
// The phase is OUR patch, not upstream's (ADR-037, first and fifth amendments),
// so an image without it rejects the value at config-parse time with
// "<olcAccessLogPurge> extra cruft after <age> <interval>". That string is the
// detection: there is no version probe and no marker convention, because the
// server already says precisely what is wrong.
//
// Deliberately narrow. "<olcAccessLogPurge> phase must be less than interval"
// is the patch's OWN bound and means the operator composed a value it should
// never have composed — a bug in the grid, not a missing patch — so it is not
// matched here and propagates as the ordinary error it is.
func isPurgeArityRejection(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "extra cruft")
}

// aggregateAccesslogPurgeConverged computes the verdict from this pass's
// observations. Pure.
//
// Three rules, the same three every condition in this operator follows:
//
//   - a pod that was not read is noted, never indicted (ADR-025). A fleet with
//     one pod mid-restart must not flip the condition to False.
//   - a database with nothing to say says so, rather than going Unknown for
//     want of evidence it does not need. Purging switched off is that case.
//   - the whole verdict is recomputed from this pass (ADR-001), so a fleet
//     upgraded onto a patched image clears it with no special case.
//
// What is NOT here any more: the withhold. There is no declared interval on a
// database to refuse, so there is nothing to withhold, which is how the defect
// was dissolved rather than patched (ADR-037 ninth amendment).
func aggregateAccesslogPurgeConverged(
	plan rotationPlan, defect *purgeDefect, purging bool, results []purgePhaseObservation,
) (metav1.ConditionStatus, string, string) {
	var assessed, unread, unsupported []string
	for _, r := range results {
		if !r.assessed {
			unread = append(unread, r.pod)
			continue
		}
		assessed = append(assessed, r.pod)
		if r.unsupported {
			unsupported = append(unsupported, r.pod)
		}
	}

	unreadNote := ""
	if len(unread) > 0 {
		unreadNote = fmt.Sprintf(" (not assessed, and therefore not counted against the "+
			"verdict: %s)", strings.Join(unread, ", "))
	}

	switch {
	case !purging:
		return metav1.ConditionTrue, reasonPurgeDisabled,
			"spec.replication.accesslogPurge.enabled is false, so this database's change " +
				"journal is never purged and olcAccessLogPurge is absent. The journal grows " +
				"unbounded; you have taken responsibility for it"

	// The defect is decided from the SPEC, not from any pod, so the unread-pod
	// note is omitted: it is true but irrelevant, and reading "not assessed:
	// slapd-0, slapd-1, slapd-2" under a spec fault suggests the pods were
	// unreachable when in fact the operator wrote the default to all of them.
	case defect != nil:
		return metav1.ConditionFalse, defect.Reason, defect.Message

	case len(unsupported) > 0:
		return metav1.ConditionFalse, reasonPurgePhaseUnsupported,
			fmt.Sprintf("the purge PHASE was refused by slapd on %s. That slapd has no "+
				"accesslog-purge-phase patch, so the third value of olcAccessLogPurge is "+
				"rejected as cruft; the operator left the two-field <retention> <interval> value "+
				"in place, which keeps the journal bounded but makes those pods purge in step "+
				"with each other instead of on the rotation grid. Run a slapd image carrying the "+
				"patch (2.7.1-0+slaptain2 or later — `slapd -VV`), or turn "+
				"spec.rotation.enabled off on the cluster%s",
				strings.Join(unsupported, ", "), unreadNote)

	case len(assessed) == 0:
		return metav1.ConditionUnknown, reasonNoReachablePod,
			fmt.Sprintf("could not read or write the accesslog overlay on any pod, so the purge "+
				"schedule could not be verified%s", unreadNote)

	// Rotation DECLARED and refused. The effect is the one above — a flat
	// interval on every pod — but the cause is the opposite, and saying
	// "spec.rotation is off" about a cluster that has spec.rotation set sends
	// the reader to the one field that is already what they wanted. The fault
	// itself is RotationConfigured's to state; this condition names it and
	// stops, so one fault is reported once.
	case plan.Status == metav1.ConditionFalse:
		return metav1.ConditionTrue, reasonRotationNotInForce,
			fmt.Sprintf("every one of the %d pod(s) read purges this journal every %s, all of "+
				"them together. The cluster DECLARES spec.rotation, and it is not in force: "+
				"%s on the SlapdCluster reports %s. Until that is resolved nothing is staggered "+
				"and the write endpoint does not move, so this journal is bounded but the purge "+
				"still runs on the pod taking writes (ADR-037)%s",
				len(assessed), formatLogAge(plan.Interval),
				rotationConfiguredCondition, plan.Reason, unreadNote)

	case !plan.PhasesActive:
		return metav1.ConditionTrue, reasonConverged,
			fmt.Sprintf("every one of the %d pod(s) read purges this journal every %s, all of "+
				"them together — the cluster's spec.rotation is off, so nothing is staggered and "+
				"the write endpoint does not move (ADR-037)%s",
				len(assessed), formatLogAge(plan.Interval), unreadNote)

	default:
		return metav1.ConditionTrue, reasonConverged,
			fmt.Sprintf("every one of the %d pod(s) read carries its own purge phase on the "+
				"cluster's grid: interval %s, phases %s apart%s",
				len(assessed), formatLogAge(plan.Interval), formatLogAge(plan.SlotLength),
				unreadNote)
	}
}

// accesslogPurgeConvergedFor renders the published condition.
func accesslogPurgeConvergedFor(
	sd *ldapv1alpha1.SlapdDatabase,
	plan rotationPlan,
	results []purgePhaseObservation,
) metav1.Condition {
	retention, defect := resolveRetention(sd)
	status, reason, msg := aggregateAccesslogPurgeConverged(plan, defect, retention > 0, results)
	cond := metav1.Condition{
		Type:               accesslogPurgeConvergedCondition,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	}
	if sd != nil {
		cond.ObservedGeneration = sd.Generation
	}
	return cond
}
