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
	_, _, _ = sd, siblings, layout
	return ridSegmentVerdict{}
}
