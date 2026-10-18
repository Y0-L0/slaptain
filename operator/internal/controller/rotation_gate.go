/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The purge-schedule gate (ADR-037, ninth amendment "the API, consolidated";
// ADR-024 R6).
//
// WHEN the purge runs is the cluster's, in one of two mutually exclusive forms:
//
//	form A  spec.accesslogPurge.interval  — one schedule, every pod together
//	form B  spec.rotation                 — staggered phases + the write endpoint
//
// WHAT each database keeps is the database's and is nowhere in this file.
//
// There is ALWAYS an interval. Form A declares it, form B derives it as n·t,
// and a cluster that says nothing gets defaultPurgeInterval. That is what
// dissolves the defect the ninth amendment records rather than patching it: no
// database declares an interval, so there is nothing to refuse, so there is no
// withhold, so there is no way for a database to end up with no purge value at
// all.
//
// spec.rotation's grammar is checked by a CEL rule on the CRD, and the ADR is
// explicit that the grammar is ALL it checks: "0s" parses as a Go duration and
// is pronounced valid, while t = 0 collapses every pod's phase k·t onto zero
// and silently restores the synchronised purge this whole feature exists to
// remove. A rule that promises more than it checks is the failure ADR-037 names
// in its own closing argument — a record of an intention is not a record of a
// state.
//
// So these checks are the operator's, and each is REPORTED rather than refused:
//
//  1. both forms declared at once (reasonScheduleDeclaredTwice) — refused,
//     never silently preferred, the shape ADR-028 uses for meshRef beside an
//     explicitly set derived field
//  2. slotLength <= 0 (reasonSlotLengthNotPositive)
//  3. a slotLength that is not a whole number of seconds
//     (reasonSlotLengthNotWholeSeconds) — olcAccessLogPurge's fields are
//     [ddd+]hh:mm[:ss] and "500ms" has no representation there
//  4. slotsFromWriteToPurge ≡ 0 (mod n) (reasonWriteSlotCollidesWithPurge) —
//     the rule needs spec.replicas, which a CRD marker cannot see
//  5. slotLength absent while enabled is true (reasonSlotLengthMissing)
//  6. a form-A interval that is non-positive or not whole seconds
//     (reasonPurgeIntervalNotUsable)
//
// Reported, not refused, for two reasons the ADR gives: a named condition is a
// better message than a refused apply, and the operator has to cope with an
// older CRD anyway. A faulty rotation block WITHHOLDS THE ROTATION AND NOTHING
// ELSE — the cluster keeps purging on the default interval, which is what every
// cluster had before this feature existed. Nothing here touches the StatefulSet
// or the pod template.
const rotationConfiguredCondition = "RotationConfigured"

// defaultPurgeInterval is how often a cluster that declared no schedule sweeps
// its accesslog journals. It is the interval half of the old two-token default
// "7+00:00 1+00:00", unchanged: every cluster has been running it since before
// ADR-037, and a default that moved when the field was decomposed would be a
// schedule change nobody asked for.
const defaultPurgeInterval = 24 * time.Hour

// defaultSlotsFromWriteToPurge is v's default: purge the slot AFTER writing.
// ADR-037 prefers it and measures it first — the writer hands off and then
// immediately works its journal down, having just stopped taking writes.
const defaultSlotsFromWriteToPurge = 1

// The reasons RotationConfigured publishes. Positively named condition, like
// every other one this operator carries: True is the good case, and "rotation
// is off" is a good case rather than a fault.
const (
	reasonRotationDisabled          = "RotationDisabled"
	reasonRotationActive            = "RotationActive"
	reasonSlotLengthMissing         = "SlotLengthMissing"
	reasonSlotLengthNotPositive     = "SlotLengthNotPositive"
	reasonSlotLengthNotWholeSeconds = "SlotLengthNotWholeSeconds"
	// reasonWriteSlotCollidesWithPurge: slotsFromWriteToPurge resolved to slot
	// 0 — the pod that is purging. The value need not be literally zero: every
	// multiple of spec.replicas resolves there, which is exactly why a bare
	// `minimum: 1` could never express the rule.
	reasonWriteSlotCollidesWithPurge = "WriteSlotCollidesWithPurge"
	// reasonScheduleDeclaredTwice: spec.accesslogPurge.interval and
	// spec.rotation both declare WHEN the purge runs. Refused rather than
	// resolved by precedence: silently preferring one makes the other a field
	// that has never been in force, which is the class of defect this whole
	// milestone exists to remove.
	reasonScheduleDeclaredTwice = "ScheduleDeclaredTwice"
	// reasonPurgeIntervalNotUsable: a form-A interval the API server accepted
	// (its rule checks the duration grammar and nothing else) that has no
	// representation on the server — non-positive, or finer than the whole
	// seconds olcAccessLogPurge counts in.
	reasonPurgeIntervalNotUsable = "PurgeIntervalNotUsable"
)

// rotationPlan is the cluster's whole purge-schedule decision, resolved once
// per pass and handed to everything that needs it.
//
// Interval is ALWAYS positive and is the one every pod writes, whichever form
// produced it. Active is the narrower question — whether the pods are
// STAGGERED and the write endpoint moves — and is the only field the write path
// reads: false means every consumer behaves exactly as it did before ADR-037,
// whether rotation was switched off or declared so badly that it cannot be
// honoured. The Status/Reason/Message triple is what the condition publishes,
// and it distinguishes those two.
//
// Replicas is n — spec.replicas, DECLARED and never observed. n fixes
// Interval = n·t, and the interval is persisted into every pod's cn=config, so
// deriving n from readiness would rewrite the whole fleet's configuration on a
// single transient restart: the churn ADR-013 exists to prevent. This is not a
// bug to fix. Readiness belongs in writer SELECTION (writeTarget), which
// persists nothing.
type rotationPlan struct {
	Active                bool
	SlotLength            time.Duration // t, form B only
	Interval              time.Duration // always positive: n·t, declared, or the default
	Replicas              int32         // n
	SlotsFromWriteToPurge int32         // v, resolved into [1, n-1]
	Status                metav1.ConditionStatus
	Reason                string
	Message               string
}

// evaluateRotation resolves spec.accesslogPurge and spec.rotation into the plan
// the rest of the operator works from. Pure: it reads the CR, reads no clock,
// performs no I/O, and is therefore assertable for every state a cluster can
// reach.
func evaluateRotation(sc *ldapv1alpha1.SlapdCluster) rotationPlan {
	// Every exit through here keeps a usable interval. A cluster whose
	// schedule could not be read still purges — the journal stays bounded
	// whatever the user got wrong, which is the one property this gate may not
	// trade away.
	flat := func(status metav1.ConditionStatus, reason, msg string, iv time.Duration) rotationPlan {
		return rotationPlan{
			Interval: iv, Replicas: declaredReplicas(sc),
			Status: status, Reason: reason, Message: msg,
		}
	}
	off := func(msg string, iv time.Duration) rotationPlan {
		return flat(metav1.ConditionTrue, reasonRotationDisabled, msg, iv)
	}
	if sc == nil {
		return off("no cluster to rotate", defaultPurgeInterval)
	}

	declaredInterval, intervalFault := declaredPurgeInterval(sc)
	r := sc.Spec.Rotation
	rotationWanted := r.Enabled != nil && *r.Enabled

	switch {
	case rotationWanted && sc.Spec.AccesslogPurge != nil && sc.Spec.AccesslogPurge.Interval != nil:
		// Both forms. Neither is taken: the cluster falls back to the default
		// interval, which keeps the journals bounded while the condition says
		// what to delete.
		return flat(metav1.ConditionFalse, reasonScheduleDeclaredTwice,
			fmt.Sprintf("spec.accesslogPurge.interval is %s AND spec.rotation.enabled is true. "+
				"Those are the two forms of one decision — WHEN the purge runs — and they are "+
				"mutually exclusive: form A is one interval with every pod purging together, "+
				"form B derives the interval as spec.replicas × slotLength and staggers the "+
				"pods. Setting both is refused rather than silently resolved by precedence, "+
				"because a field that is quietly never in force is the defect ADR-024 R6 "+
				"exists to remove. Delete one of them; meanwhile every pod purges on the "+
				"operator's default interval of %s and nothing is staggered",
				sc.Spec.AccesslogPurge.Interval.Duration, defaultPurgeInterval),
			defaultPurgeInterval)

	case intervalFault != "":
		return flat(metav1.ConditionFalse, reasonPurgeIntervalNotUsable, intervalFault,
			defaultPurgeInterval)

	case !rotationWanted:
		return off("spec.rotation.enabled is not set, so every pod purges on the same schedule "+
			"— interval "+formatLogAge(declaredInterval)+" — and the write endpoint does not "+
			"move (ADR-037)", declaredInterval)
	}

	fault := func(reason, msg string) rotationPlan {
		return flat(metav1.ConditionFalse, reason, msg, defaultPurgeInterval)
	}
	if r.SlotLength == nil {
		return fault(reasonSlotLengthMissing,
			"spec.rotation.enabled is true but spec.rotation.slotLength is not set, and it has no "+
				"default — ADR-037 fixes no number for t on purpose. Nothing rotates until it is "+
				"set; pick a slot length your deployment can afford a purge to take (6h and 12h "+
				"are the production-shaped values the ADR expects). Meanwhile every pod purges on "+
				"the operator's default interval of "+formatLogAge(defaultPurgeInterval))
	}

	t := r.SlotLength.Duration
	switch {
	case t <= 0:
		return fault(reasonSlotLengthNotPositive,
			fmt.Sprintf("spec.rotation.slotLength is %s. The API server accepts it — its rule "+
				"checks the duration GRAMMAR and nothing else — but a slot length of zero or less "+
				"puts every pod's phase k·t at zero, which is the synchronised purge this feature "+
				"exists to remove. Nothing rotates until it is positive", t))
	case t%time.Second != 0:
		return fault(reasonSlotLengthNotWholeSeconds,
			fmt.Sprintf("spec.rotation.slotLength is %s, which is not a whole number of seconds. "+
				"olcAccessLogPurge's fields are [ddd+]hh:mm[:ss], so neither the derived interval "+
				"nor the derived phases have a representation on the server. Nothing rotates until "+
				"it lands on a second", t))
	}

	n := declaredReplicas(sc)

	// v is a SIGNED displacement taken mod n, so −1 and n−1 are one answer and
	// a declared value survives a scale event. The rule is v mod n ≠ 0, not a
	// minimum: zero and every multiple of n put the write on the pod that is
	// purging. n = 1 is the one case the rule cannot cover — with a single pod
	// every v is a multiple of n and the pod is unavoidably both — so the check
	// is skipped there rather than refusing a cluster for having one replica.
	want := int32(defaultSlotsFromWriteToPurge)
	declared := false
	if r.SlotsFromWriteToPurge != nil {
		want, declared = *r.SlotsFromWriteToPurge, true
	}
	resolved := (want%n + n) % n
	if declared && n > 1 && resolved == 0 {
		return fault(reasonWriteSlotCollidesWithPurge,
			fmt.Sprintf("spec.rotation.slotsFromWriteToPurge is %d, which resolves to slot 0 of "+
				"%d — the pod that is purging. Zero and every multiple of spec.replicas land "+
				"there, which is why the rule is `v mod n ≠ 0` and not a minimum, and why it is "+
				"the operator reporting it rather than the API server refusing it. Pick a value "+
				"that is not a multiple of %d (1 purges the slot after writing, −1 the slot "+
				"before). Nothing rotates meanwhile; every pod purges on the operator's default "+
				"interval of %s",
				want, n, n, formatLogAge(defaultPurgeInterval)))
	}
	if resolved == 0 {
		// n = 1: the single pod writes and purges in the same slot whatever v
		// says. 1 is the value writeTarget's degenerate fallback reads as "the
		// only pod there is".
		resolved = 1
	}

	plan := rotationPlan{
		Active:                true,
		SlotLength:            t,
		Interval:              time.Duration(n) * t,
		Replicas:              n,
		SlotsFromWriteToPurge: resolved,
		Status:                metav1.ConditionTrue,
		Reason:                reasonRotationActive,
	}
	plan.Message = fmt.Sprintf("rotating every %s over %d pod(s): purge interval %s, pod k purges "+
		"at k×%s counted from the epoch, and the write endpoint selects the pod that purges %d "+
		"slot(s) later. Sizing is yours (ADR-037 decision 6) — the operator will not refuse a "+
		"rotation whose phases are too long for your write rate",
		t, n, plan.Interval, t, plan.SlotsFromWriteToPurge)
	return plan
}

// declaredReplicas is n, and it is spec.replicas — DECLARED, never observed.
// A non-positive count reads as one pod: a StatefulSet scaled to zero still has
// a declared shape, and the grid must stay defined rather than divide by it.
func declaredReplicas(sc *ldapv1alpha1.SlapdCluster) int32 {
	if sc == nil || sc.Spec.Replicas <= 0 {
		return 1
	}
	return sc.Spec.Replicas
}

// declaredPurgeInterval resolves form A: spec.accesslogPurge.interval, or the
// operator's default. The second return is a fault message — non-empty when the
// declared value has no representation on the server — and the first is then
// the default, because the journals must stay bounded whatever the user wrote.
func declaredPurgeInterval(sc *ldapv1alpha1.SlapdCluster) (time.Duration, string) {
	if sc == nil || sc.Spec.AccesslogPurge == nil || sc.Spec.AccesslogPurge.Interval == nil {
		return defaultPurgeInterval, ""
	}
	iv := sc.Spec.AccesslogPurge.Interval.Duration
	switch {
	case iv <= 0:
		return defaultPurgeInterval, fmt.Sprintf("spec.accesslogPurge.interval is %s. The API "+
			"server accepts it — its rule checks the duration GRAMMAR and nothing else — but an "+
			"interval of zero or less is not a schedule. Every pod purges on the operator's "+
			"default of %s until it is positive", iv, formatLogAge(defaultPurgeInterval))
	case iv%time.Second != 0:
		return defaultPurgeInterval, fmt.Sprintf("spec.accesslogPurge.interval is %s, which is "+
			"not a whole number of seconds. olcAccessLogPurge's fields are [ddd+]hh:mm[:ss], so "+
			"it has no representation on the server. Every pod purges on the operator's default "+
			"of %s until it lands on a second", iv, formatLogAge(defaultPurgeInterval))
	}
	return iv, ""
}

// rotationConfiguredFor renders the plan as the published condition.
func rotationConfiguredFor(sc *ldapv1alpha1.SlapdCluster, plan rotationPlan) metav1.Condition {
	cond := metav1.Condition{
		Type:               rotationConfiguredCondition,
		Status:             plan.Status,
		Reason:             plan.Reason,
		Message:            plan.Message,
		LastTransitionTime: metav1.Now(),
	}
	if sc != nil {
		cond.ObservedGeneration = sc.Generation
	}
	return cond
}

// reconcileRotationGate records the verdict on the SlapdCluster and returns the
// plan. It withholds nothing: a rotation that cannot be honoured leaves the
// cluster behaving as it did before the feature existed, and the condition
// carries the news. The SlapdDatabase controller re-evaluates the same pure
// function from the same CR, so the two can never disagree about the grid.
func (r *SlapdClusterReconciler) reconcileRotationGate(
	ctx context.Context, sc *ldapv1alpha1.SlapdCluster,
) rotationPlan {
	plan := evaluateRotation(sc)
	if plan.Status == metav1.ConditionFalse {
		logf.FromContext(ctx).Info("rotation declared but not usable; nothing rotates",
			"cluster", sc.Name, "reason", plan.Reason, "message", plan.Message)
	}
	setCondition(&sc.Status.Conditions, rotationConfiguredFor(sc, plan))
	return plan
}
