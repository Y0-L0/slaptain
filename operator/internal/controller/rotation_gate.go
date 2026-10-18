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

// The rotation gate (ADR-037, fourth amendment "the API, and what admission
// cannot check").
//
// spec.rotation's grammar is checked by a CEL rule on the CRD, and the ADR is
// explicit that the grammar is ALL it checks: "0s" parses as a Go duration and
// is pronounced valid, while t = 0 collapses every pod's phase k·t onto zero
// and silently restores the synchronised purge this whole feature exists to
// remove. A rule that promises more than it checks is the failure ADR-037 names
// in its own closing argument — a record of an intention is not a record of a
// state.
//
// So four checks are the operator's, and each is REPORTED rather than refused:
//
//  1. slotLength <= 0 (reasonSlotLengthNotPositive)
//  2. a slotLength that is not a whole number of seconds
//     (reasonSlotLengthNotWholeSeconds) — olcAccessLogPurge's fields are
//     [ddd+]hh:mm[:ss] and "500ms" has no representation there
//  3. writerOffset outside [1, n-1] (reasonWriterOffsetClamped) — the upper
//     bound depends on spec.replicas, which a CRD marker cannot see
//  4. slotLength absent while enabled is true (reasonSlotLengthMissing)
//
// Reported, not refused, for two reasons the ADR gives: a named condition is a
// better message than a refused apply, and the operator has to cope with an
// older CRD anyway. The gate WITHHOLDS the feature and nothing else — a faulty
// rotation block leaves the cluster running exactly as it would with rotation
// off, which is the behaviour every cluster had before this feature existed.
// Nothing here touches the StatefulSet or the pod template.
const rotationConfiguredCondition = "RotationConfigured"

// The reasons RotationConfigured publishes. Positively named condition, like
// every other one this operator carries: True is the good case, and "rotation
// is off" is a good case rather than a fault.
const (
	reasonRotationDisabled          = "RotationDisabled"
	reasonRotationActive            = "RotationActive"
	reasonSlotLengthMissing         = "SlotLengthMissing"
	reasonSlotLengthNotPositive     = "SlotLengthNotPositive"
	reasonSlotLengthNotWholeSeconds = "SlotLengthNotWholeSeconds"
	reasonWriterOffsetClamped       = "WriterOffsetClamped"
)

// rotationPlan is the cluster's whole rotation decision, resolved once per pass
// and handed to everything that needs it.
//
// Active is the only field the write path reads: false means every consumer
// behaves exactly as it did before ADR-037, whether rotation was switched off
// or declared so badly that it cannot be honoured. The Status/Reason/Message
// triple is what the condition publishes, and it distinguishes those two.
//
// Replicas is n — spec.replicas, DECLARED and never observed. n fixes
// Interval = n·t, and the interval is persisted into every pod's cn=config, so
// deriving n from readiness would rewrite the whole fleet's configuration on a
// single transient restart: the churn ADR-013 exists to prevent. This is not a
// bug to fix. Readiness belongs in writer SELECTION (writeTarget), which
// persists nothing, and that is a milestone of its own.
type rotationPlan struct {
	Active       bool
	SlotLength   time.Duration // t
	Interval     time.Duration // T = n·t
	Replicas     int32         // n
	WriterOffset int32         // d, clamped to [1, n-1]
	Status       metav1.ConditionStatus
	Reason       string
	Message      string
}

// evaluateRotation resolves spec.rotation into the plan the rest of the
// operator works from. Pure: it reads the CR, reads no clock, performs no I/O,
// and is therefore assertable for every state a cluster can reach.
func evaluateRotation(sc *ldapv1alpha1.SlapdCluster) rotationPlan {
	off := func(msg string) rotationPlan {
		return rotationPlan{Status: metav1.ConditionTrue, Reason: reasonRotationDisabled, Message: msg}
	}
	if sc == nil {
		return off("no cluster to rotate")
	}

	r := sc.Spec.Rotation
	if r.Enabled == nil || !*r.Enabled {
		return off("spec.rotation.enabled is not set; each pod purges on its own schedule from " +
			"its own start, and the write endpoint does not move (ADR-037)")
	}

	fault := func(reason, msg string) rotationPlan {
		return rotationPlan{Status: metav1.ConditionFalse, Reason: reason, Message: msg}
	}
	if r.SlotLength == nil {
		return fault(reasonSlotLengthMissing,
			"spec.rotation.enabled is true but spec.rotation.slotLength is not set, and it has no "+
				"default — ADR-037 fixes no number for t on purpose. Nothing rotates until it is "+
				"set; pick a slot length your deployment can afford a purge to take (6h and 12h "+
				"are the production-shaped values the ADR expects)")
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

	n := sc.Spec.Replicas
	if n <= 0 {
		n = 1
	}

	plan := rotationPlan{
		Active:     true,
		SlotLength: t,
		Interval:   time.Duration(n) * t,
		Replicas:   n,
		Status:     metav1.ConditionTrue,
		Reason:     reasonRotationActive,
	}

	// d defaults to n-1: the writer is the pod that purges NEXT, having had the
	// longest time to work off its purge backlog and its replication lag.
	// ADR-037's second amendment calls that "the preferred end and the one
	// being measured first".
	want := n - 1
	declared := false
	if r.WriterOffset != nil {
		want, declared = *r.WriterOffset, true
	}
	plan.WriterOffset = min(max(want, 1), max(n-1, 1))

	if declared && plan.WriterOffset != want {
		plan.Reason = reasonWriterOffsetClamped
		plan.Message = fmt.Sprintf("rotating every %s over %d pod(s): purge interval %s, phases %s "+
			"apart. spec.rotation.writerOffset is %d, which is outside [1, %d] for spec.replicas=%d, "+
			"so the operator clamped it to %d — d = 0 would put the write target on the pod that is "+
			"purging, which is the whole thing the rotation avoids",
			t, n, plan.Interval, t, want, max(n-1, 1), n, plan.WriterOffset)
		return plan
	}

	plan.Message = fmt.Sprintf("rotating every %s over %d pod(s): purge interval %s, pod k purges "+
		"at k×%s counted from the epoch, writer offset d=%d. Sizing is yours (ADR-037 decision 6) — "+
		"the operator will not refuse a rotation whose phases are too long for your write rate",
		t, n, plan.Interval, t, plan.WriterOffset)
	return plan
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
