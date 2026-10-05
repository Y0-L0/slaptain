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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// Reasons a RID segment is or is not usable. They are condition reasons, so
// they are what a human reads first; each one names a different human
// response: pick a free number, pick a smaller one, widen the layout.
const (
	// reasonRIDSegmentOK — the declared segment is in range and nothing else
	// in this cluster declares it.
	reasonRIDSegmentOK = "RIDSegmentUnique"
	// reasonRIDSegmentMissing — replication is on and no segment is declared.
	// The CEL rule on SlapdDatabaseSpec normally catches this, so reaching it
	// means an object that predates the field or one applied with validation
	// off.
	reasonRIDSegmentMissing = "RIDSegmentMissing"
	// reasonRIDSegmentOutOfRange — the segment is negative, or at or above the
	// C the layout in force admits.
	reasonRIDSegmentOutOfRange = "RIDSegmentOutOfRange"
	// reasonRIDSegmentCollision — another SlapdDatabase of the same cluster
	// declares this segment. Both sides refuse: whichever reconciles first
	// must not win, because the loser's stanzas are already on the pods.
	reasonRIDSegmentCollision = "RIDSegmentCollision"
	// reasonRIDLayoutInvalid — the layout itself cannot produce a usable
	// segment, so no segment is in range and the question of which one was
	// declared does not arise.
	reasonRIDLayoutInvalid = "RIDLayoutInvalid"
	// reasonRIDSegmentUnverified — uniqueness could not be ESTABLISHED: the
	// read that would have answered it failed. The withhold is identical to a
	// collision's (RID-PLAN hazard 3: unknown withholds), but the reason must
	// not be, because a reason is a diagnosis. RIDSegmentCollision tells a
	// human to go edit a CR; here there may be nothing wrong with any CR, and
	// the thing to look at is the API server, RBAC or the cache. Alerting and
	// slctl key on the reason, so conflating the two makes a real collision
	// during an apiserver blip indistinguishable from the blip.
	reasonRIDSegmentUnverified = "RIDSegmentUnverified"
)

// ridSegmentVerdict is the per-database answer: may this database's stanzas
// converge, and if not, what does a human do about it?
//
// Segment is meaningful only when OK; a refusal carries no usable coordinate.
type ridSegmentVerdict struct {
	OK      bool
	Reason  string
	Message string
	Segment int32
}

// judgeRIDSegment decides whether sd's declared segment may be used, given the
// other SlapdDatabases in its namespace and the layout in force.
//
// Contract (ADR-031 decisions 3 and 6):
//
//   - An unusable layout is judged first: no segment is in range while no
//     segment exists.
//   - An absent segment on a replicating database is a refusal, not a zero.
//   - A segment outside 0..C-1 is a refusal naming the ceiling and the layout
//     that produced it.
//   - A segment another database of THIS cluster declares is a refusal naming
//     that database — symmetrically, so neither side of a collision wins.
//     A sibling counts whether or not its own replication is enabled: its
//     stanzas may still be on the pods from before the flag was flipped.
//   - Siblings belonging to another cluster are ignored; a sibling that is sd
//     itself is not a collision with itself.
//
// Pure: no client, no context, no I/O. The caller does the List and owns what
// a failed List means (RID-PLAN hazard 3: unknown uniqueness withholds), and
// the caller resolves the layout — under meshRef from SlapdMesh.spec.ridLayout,
// otherwise the defaults. The SlapdCluster is deliberately NOT a parameter:
// everything this judgement needs is on sd, and an unused argument is an
// invitation to make it load-bearing later, inconsistently.
func judgeRIDSegment(sd *ldapv1alpha1.SlapdDatabase, siblings []ldapv1alpha1.SlapdDatabase, layout ridcoord.Layout) ridSegmentVerdict {
	// 1. The layout, first. A layout that admits no segment has no ceiling to
	//    compare a declared segment against, so every later question is
	//    unanswerable and a reason naming the segment would send a human to
	//    change the wrong field.
	if err := layout.Validate(); err != nil {
		return ridSegmentVerdict{
			Reason: reasonRIDLayoutInvalid,
			Message: fmt.Sprintf(
				"the RID layout in force admits no segment: %v. Fix spec.ridLayout on the SlapdMesh (podsPerSite and maxSites must each be at least 1, and their sum at most %d).",
				err, ridcoord.MaxRID),
		}
	}
	maxSegments := layout.MaxSegments()

	// 2. Declared at all. nil is absence, never the int32 zero: reading it as
	//    0 makes every undeclared database collide with segment 0.
	if sd.Spec.Replication.RIDSegment == nil {
		return ridSegmentVerdict{
			Reason: reasonRIDSegmentMissing,
			Message: fmt.Sprintf(
				"spec.replication.ridSegment is not declared. Declare a segment in 0..%d, or set spec.replication.enabled=false to exclude this database from replication.",
				maxSegments-1),
		}
	}
	segment := *sd.Spec.Replication.RIDSegment

	// 3. In range for THIS layout. The ceiling is the layout's C, not a
	//    constant and not the CRD's Maximum: a mesh-wide layout change is
	//    exactly the same CR getting a different verdict (ADR-031 decision 5).
	if segment < 0 || segment >= maxSegments {
		return ridSegmentVerdict{
			Reason: reasonRIDSegmentOutOfRange,
			Message: fmt.Sprintf(
				"spec.replication.ridSegment %d is outside 0..%d: the RID layout in force (podsPerSite=%d, maxSites=%d) admits %d segments. Declare a segment in that range, or widen spec.ridLayout on the SlapdMesh.",
				segment, maxSegments-1, layout.PodsPerSite, layout.MaxSites, maxSegments),
		}
	}

	// 4. Unique among the databases that share a pod with this one, which is
	//    what clusterRef names. The scan is symmetric by construction — it
	//    asks only "does anyone else declare my number" — so both sides of a
	//    collision refuse and neither wins the race to write stanzas.
	if other := collidingSibling(sd, siblings, segment); other != "" {
		return ridSegmentVerdict{
			Reason: reasonRIDSegmentCollision,
			Message: fmt.Sprintf(
				"spec.replication.ridSegment %d is also declared by SlapdDatabase %q of cluster %q. Both databases withhold their syncrepl stanzas until one of them declares a different segment; the operator never reassigns.",
				segment, other, sd.Spec.ClusterRef),
		}
	}

	return ridSegmentVerdict{
		OK:      true,
		Reason:  reasonRIDSegmentOK,
		Segment: segment,
	}
}

// collidingSibling names the lowest-named other SlapdDatabase of sd's cluster
// declaring `segment`, or "" when none does.
//
// Lowest-named rather than first-seen so that a refusal message does not
// depend on the order the API server happened to return the List in: a
// condition whose text flaps between two equally correct names reads as
// churn.
//
// A sibling is counted regardless of its own replication.enabled. ADR-031
// decision 3 ties segment reuse to the other database's stanzas being GONE
// from the pods, and flipping a flag to false removes nothing from cn=config.
func collidingSibling(sd *ldapv1alpha1.SlapdDatabase, siblings []ldapv1alpha1.SlapdDatabase, segment int32) string {
	found := ""
	for i := range siblings {
		other := &siblings[i]
		// The caller hands over a plain List, so sd is in it. Matching on
		// name+namespace rather than assuming a pre-filtered slice: a judge
		// that collided with itself would stall every database forever.
		if other.Name == sd.Name && other.Namespace == sd.Namespace {
			continue
		}
		// A RID is unique per SERVER (ADR-031, slapd's connid band). Two
		// clusters share no pod, so their segments cannot collide.
		if other.Spec.ClusterRef != sd.Spec.ClusterRef {
			continue
		}
		if other.Spec.Replication.RIDSegment == nil || *other.Spec.Replication.RIDSegment != segment {
			continue
		}
		if found == "" || other.Name < found {
			found = other.Name
		}
	}
	return found
}

// ── Wiring ───────────────────────────────────────────────────────────────────

// resolveRIDSegment is judgeRIDSegment plus the one read it needs: the
// same-namespace SlapdDatabases it is compared against.
//
// RID-PLAN hazard 3: an unprovable uniqueness is not a proven one. A failed
// List is not "no siblings, therefore no collision" — it is "unknown", and
// unknown withholds.
//
// The withhold is identical to a collision's; the REASON is not. A reason is a
// diagnosis, and alerting and slctl key on it: RIDSegmentCollision sends a
// human to go edit a CR, which here may be perfectly correct, and would make a
// real collision that happened during an apiserver blip indistinguishable from
// the blip.
func (r *SlapdDatabaseReconciler) resolveRIDSegment(
	ctx context.Context,
	sd *ldapv1alpha1.SlapdDatabase,
	layout ridcoord.Layout,
) ridSegmentVerdict {
	var siblings ldapv1alpha1.SlapdDatabaseList
	if err := r.List(ctx, &siblings, client.InNamespace(sd.Namespace)); err != nil {
		return ridSegmentVerdict{
			Reason: reasonRIDSegmentUnverified,
			Message: fmt.Sprintf(
				"could not list the SlapdDatabases of namespace %q, so this database's ridSegment cannot be shown to be unique: %v. Syncrepl stanzas are withheld until the read succeeds — an unproven uniqueness is not a proven one. Nothing here says a CR is wrong: look at the API server, the operator's RBAC and its cache.",
				sd.Namespace, err),
		}
	}
	return judgeRIDSegment(sd, siblings.Items, layout)
}

// reconcileRIDSegment judges this database's declared segment AND its cluster's
// external peer list, publishes the verdict, and returns the segment the stanza
// builder derives its coordinates from — or, on a refusal, the verdict that
// withholds them.
//
// The two judgements share one condition and one withhold because they are one
// question with two halves: can this database's stanzas be given RIDs at all?
// A refused segment has no run of RIDs; a peer list that cannot be indexed has
// no remote slot. Either way every stanza on every pod is withheld, which is
// the single outcome ADR-031 decision 6 prescribes, so splitting them would
// publish two conditions that can never disagree.
//
// Called on EVERY pass, including the passes where the answer is "there is
// nothing to judge". That is the whole point: the condition is published
// inside the replication gate's own branch nowhere, so a database that STOPS
// replicating cannot keep the verdict it had when it did. That path is not
// hypothetical — "set replication.enabled=false to exclude this database" is
// the text of the refusal itself, so it is exactly where the message sends
// people, and it used to leave them a False condition naming a collision that
// could no longer exist, beside Ready=True. Absence cannot go stale, so the
// not-applicable answer is to remove the condition rather than to word one.
//
// Shaped after tunablesConvergedCondition, which is set unconditionally at the
// top of every pass for the same reason.
func (r *SlapdDatabaseReconciler) reconcileRIDSegment(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	layout ridcoord.Layout,
	layoutErr error,
) (int32, *ridSegmentVerdict) {
	if !sc.Spec.Replication.Enabled || !sd.ReplicationEnabled() {
		meta.RemoveStatusCondition(&sd.Status.Conditions, ridSegmentConditionType)
		return 0, nil
	}

	// The layout first, and WITH the error that produced it. A failed read and
	// a declared-but-unusable ridLayout both arrive here as a zero Layout, and
	// publishing "fix spec.ridLayout" for the first sends a human to a field
	// that may be perfectly correct — see judgeRIDLayout.
	if lv := judgeRIDLayout(sc, layout, layoutErr); lv != nil {
		setCondition(&sd.Status.Conditions, ridSegmentCondition(sd, *lv))
		refusal := *lv
		return 0, &refusal
	}

	v := r.resolveRIDSegment(ctx, sd, layout)
	if v.OK {
		// Then the cluster's own shape, then the peer list — in the order a
		// human can act on them, and each only once the question before it is
		// answered. A refusal naming a peer's siteIndex while the segment has
		// no run of RIDs at all would send them to the wrong field.
		if fv := judgeClusterFleet(sc, layout); !fv.OK {
			v = fv
		}
	}
	if v.OK {
		if pv := judgeExternalPeers(sc, layout); !pv.OK {
			v = pv
		}
	}
	setCondition(&sd.Status.Conditions, ridSegmentCondition(sd, v))
	if !v.OK {
		refusal := v
		return 0, &refusal
	}
	return v.Segment, nil
}

// ridSegmentConditionType is where a human looks for the verdict after the
// phase has moved on to something else.
const ridSegmentConditionType = "RIDSegmentValid"

// ridSegmentCondition renders a verdict as the condition that carries it.
func ridSegmentCondition(sd *ldapv1alpha1.SlapdDatabase, v ridSegmentVerdict) metav1.Condition {
	status := metav1.ConditionFalse
	msg := v.Message
	if v.OK {
		status = metav1.ConditionTrue
		msg = fmt.Sprintf("ridSegment %d is in range and declared by no other database of cluster %q",
			v.Segment, sd.Spec.ClusterRef)
	}
	return metav1.Condition{
		Type:               ridSegmentConditionType,
		Status:             status,
		Reason:             v.Reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	}
}
