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
// reasonConverged (names.go), reasonNoReachablePod (data_present.go) and
// reasonRotationDisabled (rotation_gate.go).
const (
	// reasonPurgeIntervalDeclared: the database declares an interval in
	// spec.replication.accesslogPurge while the cluster's rotation owns the
	// schedule. ADR-024's rule is that a field which cannot be honoured is
	// rejected or converged, never ignored — so the purge value is WITHHELD for
	// this database (every pod keeps whatever it has) rather than silently
	// overridden with the cluster's interval.
	reasonPurgeIntervalDeclared = "PurgeIntervalDeclared"
	// reasonPurgePhaseUnsupported: a pod's slapd refused the three-field value
	// outright ("extra cruft after <age> <interval>"). Its accesslog overlay is
	// a stock build without the accesslog-purge-phase patch. Decided handling
	// (ADR-037, PURGE-ROTATION-PLAN Phase 3): attempt, then report. No version
	// probing, no marker convention — the failure is loud, bounded, and names
	// exactly what to fix.
	reasonPurgePhaseUnsupported = "PurgePhaseUnsupported"
)

// purgeWithhold is a per-database refusal: something this database declares
// cannot be honoured, so its purge value is not written at all this pass.
type purgeWithhold struct {
	Reason  string
	Message string
}

// purgePodPlan is the olcAccessLogPurge decision for one (pod, database).
//
// Value is what to write; "" means the attribute must be absent (the "none"
// opt-out surviving composition — a schedule must never resurrect a purge the
// user turned off).
//
// Fallback is the two-field value to fall back to when the server refuses
// Value's third field. It is empty whenever Value carries no phase, so the
// fallback path cannot fire where there is nothing to fall back from. A fresh
// cluster on an unpatched image has no olcAccessLogPurge at all, and writing
// nothing there would restore the unpurged-journal failure that
// defaultAccesslogPurge exists to prevent — so the fallback is written and the
// condition reports what was lost, rather than the pod being left bare.
type purgePodPlan struct {
	Value    string
	Fallback string
	Withheld bool
	Reason   string
	Message  string
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

// splitPurgeAge returns the age half of an olcAccessLogPurge value, and whether
// the value carried anything beyond it.
//
// The attribute is whitespace-separated (slapd re-emits it with its own
// spacing, which is why strings.Fields rather than a split on " "). Under
// rotation only the age half comes from the database: the interval and the
// phase are a schedule over pods and belong to the cluster (ADR-037, second
// amendment, "Where the configuration lives").
//
// Pure, and deliberately not clever: it does not validate the age against
// slapd's [ddd+]hh:mm[:ss] grammar. The age is passed through verbatim — it is
// what the user wrote, or what defaultAccesslogPurge says — exactly as the
// non-rotating path passes the whole value through verbatim today.
func splitPurgeAge(purge string) (string, bool) {
	fields := strings.Fields(purge)
	switch len(fields) {
	case 0:
		return "", false
	case 1:
		return fields[0], false
	default:
		return fields[0], true
	}
}

// databaseDeclaresPurgeInterval reports whether the DATABASE's own field — not
// the operator's default — carries an interval. The distinction matters: the
// shipped default is "7+00:00 1+00:00" and carries one, but nobody declared it,
// so a database that says nothing is not in conflict with the cluster's
// schedule and must not be refused.
func databaseDeclaresPurgeInterval(sd *ldapv1alpha1.SlapdDatabase) bool {
	if sd == nil {
		return false
	}
	v := strings.TrimSpace(sd.Spec.Replication.AccesslogPurge)
	if v == "" || strings.EqualFold(v, noneSentinel) {
		return false
	}
	_, extra := splitPurgeAge(v)
	return extra
}

// planPodPurge composes the olcAccessLogPurge value for one pod of one
// database. Pure; the one place the two owners of the attribute meet.
//
// With rotation off the result is byte-identical to the pre-ADR-037 behaviour:
// desiredAccesslogPurge's answer, unchanged, with no fallback. That is a hard
// compatibility requirement and has its own test.
//
// With rotation on, ALL DATABASES ON A POD SHARE THAT POD'S PHASE — aligned,
// not spread (ADR-037, second amendment, "Databases align, they do not
// spread"). The phase is a function of the ordinal alone; the database enters
// only through the age. Spreading the databases over n·D slots would disturb
// every pod throughout the period and defeat the purpose, which is to
// concentrate a pod's disturbance into the window where it is not writing.
func planPodPurge(plan rotationPlan, sd *ldapv1alpha1.SlapdDatabase, ordinal int32) purgePodPlan {
	resolved := desiredAccesslogPurge(sd)
	if !plan.Active {
		return purgePodPlan{Value: resolved}
	}
	// "none" survives composition: a schedule must not resurrect a purge the
	// user turned off. composePurge would answer "" for a blank age anyway;
	// returning here keeps the fallback empty too.
	if resolved == "" {
		return purgePodPlan{}
	}

	if databaseDeclaresPurgeInterval(sd) {
		return purgePodPlan{
			Withheld: true,
			Reason:   reasonPurgeIntervalDeclared,
			Message: fmt.Sprintf("spec.replication.accesslogPurge is %q, which declares an "+
				"interval. The cluster's spec.rotation owns the schedule (interval %s = "+
				"spec.replicas × slotLength, phase per pod), so this database's purge value is "+
				"WITHHELD rather than silently overridden — the pods keep what they have. Leave "+
				"the age alone and drop the interval (%q), or turn spec.rotation.enabled off",
				strings.TrimSpace(sd.Spec.Replication.AccesslogPurge), formatLogAge(plan.Interval),
				firstField(sd.Spec.Replication.AccesslogPurge)),
		}
	}

	age, _ := splitPurgeAge(resolved)
	phase := purgePhase(ordinal, plan.Replicas, plan.SlotLength)
	return purgePodPlan{
		Value:    composePurge(age, plan.Interval, phase),
		Fallback: age + " " + formatLogAge(plan.Interval),
	}
}

// firstField renders the age half of a declared value for a message.
func firstField(v string) string {
	age, _ := splitPurgeAge(v)
	return age
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
//     want of evidence it does not need. Rotation off is that case, and the
//     condition is published anyway — setCondition never removes one, so a
//     cluster that turns rotation OFF would otherwise keep a stale False.
//   - the whole verdict is recomputed from this pass (ADR-001), so a fleet
//     upgraded onto a patched image clears it with no special case.
func aggregateAccesslogPurgeConverged(
	rotating bool, withheld *purgeWithhold, results []purgePhaseObservation,
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
	case !rotating:
		return metav1.ConditionTrue, reasonRotationDisabled,
			"the cluster's spec.rotation is off, so olcAccessLogPurge carries this database's " +
				"own age and interval and every pod purges on its own schedule (ADR-037)"

	// The withhold is decided from the SPEC, not from any pod, so the
	// unread-pod note is omitted here: it is true but irrelevant, and reading
	// "not assessed: slapd-0, slapd-1, slapd-2" under a refusal suggests the
	// pods were unreachable when in fact the operator deliberately wrote
	// nothing to them. Measured on a live three-pod cluster.
	case withheld != nil:
		return metav1.ConditionFalse, withheld.Reason, withheld.Message

	case len(unsupported) > 0:
		return metav1.ConditionFalse, reasonPurgePhaseUnsupported,
			fmt.Sprintf("the purge PHASE was refused by slapd on %s. That slapd has no "+
				"accesslog-purge-phase patch, so the third value of olcAccessLogPurge is "+
				"rejected as cruft; the operator left the two-field <age> <interval> value in "+
				"place, which keeps the journal bounded but makes those pods purge in step with "+
				"each other instead of on the rotation grid. Run a slapd image carrying the "+
				"patch (2.7.1-0+slaptain2 or later — `slapd -VV`), or turn "+
				"spec.rotation.enabled off on the cluster%s",
				strings.Join(unsupported, ", "), unreadNote)

	case len(assessed) == 0:
		return metav1.ConditionUnknown, reasonNoReachablePod,
			fmt.Sprintf("could not read or write the accesslog overlay on any pod, so the purge "+
				"schedule could not be verified%s", unreadNote)

	default:
		return metav1.ConditionTrue, reasonConverged,
			fmt.Sprintf("every one of the %d pod(s) read carries its own purge phase on the "+
				"cluster's grid%s", len(assessed), unreadNote)
	}
}

// accesslogPurgeConvergedFor renders the published condition.
func accesslogPurgeConvergedFor(
	sd *ldapv1alpha1.SlapdDatabase,
	rotating bool,
	withheld *purgeWithhold,
	results []purgePhaseObservation,
) metav1.Condition {
	status, reason, msg := aggregateAccesslogPurgeConverged(rotating, withheld, results)
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
