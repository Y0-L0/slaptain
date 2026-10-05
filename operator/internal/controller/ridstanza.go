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

	"sigs.k8s.io/controller-runtime/pkg/client"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// The consumption seam of ADR-031: where a coordinate stops being arithmetic
// in a package nothing calls and becomes the number on a syncrepl stanza.
//
// Everything here is pure except ridLayoutFor, which performs the single read
// the layout needs. The stanza builders are long and take a dozen arguments;
// the derivation deliberately does not live inside them, because a RID that is
// wrong by one is a stanza carrying another provider's number and slapd accepts
// a duplicate RID in silence (ADR-031, "What slapd actually requires").
//
// RID-PLAN hazard 1: one coordinate function, not two. Every number below comes
// from ridcoord.Layout.Local or ridcoord.Layout.Remote, and a refusal from
// ridcoord propagates verbatim — never a substituted, capped or improvised
// number.

// inClusterStanzaRIDs is the RID of every in-cluster provider's stanza, as seen
// from one pod: one entry per provider ordinal in [0, replicas), keyed by that
// ordinal, each value ridcoord.Layout.Local(segment, ordinal).
//
// selfOrdinal says who is asking, and it is a POINTER on purpose:
//
//   - non-nil — a read-write pod, which is itself a provider. Its own ordinal
//     is omitted from the result: a pod never replicates from itself. The slot
//     is NOT reclaimed or renumbered — reclaiming it would make the offset
//     depend on who is reading, which is the uniformity ADR-031 decision 1
//     trades two dead slots for.
//   - nil — the caller is not a provider at all, which is the read-only pod.
//     It receives the full set. That is the whole of "read replicas stay free":
//     an RO pod consumes from the same providers and therefore carries the same
//     RIDs the RW pods do (ADR-031 Consequences).
//
// A sentinel int32 was rejected for the second case. The RO caller would have
// had to remember to pass -1, and forgetting it — passing its own 0 — drops
// provider 0's stanza from an otherwise complete set, with no refusal anywhere:
// a short set under the atomic Replace is a deletion of that link (RID-PLAN
// hazard 2). A convention that must be remembered at a call site is the class
// of bug this scheme exists to make unexpressible, so the type carries it
// instead.
//
// A non-nil selfOrdinal outside [0, replicas) skips nothing and is not an
// error: there is no such provider to omit. The caller's own loop cannot
// produce one, and refusing would withhold a whole site's stanzas over a
// transient the ADR does not call a fault.
//
// Refuses with whatever ridcoord refused — an invalid layout, a segment outside
// 0..C-1, an ordinal outside 0..P-1 — and returns no partial map. A short
// stanza set is a deletion under the atomic Replace, so there is no such thing
// as "most of the RIDs".
func inClusterStanzaRIDs(layout ridcoord.Layout, segment, replicas int32, selfOrdinal *int32) (map[int32]int32, error) {
	panic("not implemented")
}

// externalStanzaRID is the RID of this pod's syncrepl link to peer site
// peerSiteIndex, for the database holding segment:
// ridcoord.Layout.Remote(segment, peerSiteIndex).
//
// The coordinate names the LINK, not the remote pod answering it. Which remote
// pod serves a link is a runtime selection that moves when the far end is
// replaced; the stanza's provider= says which pod, the RID says which link
// (ADR-031 decision 1). A pod holds at most one link per peer site per database
// (decision 9), which is what makes one slot per peer site enough.
//
// Refuses with whatever ridcoord refused. Nothing here caps or wraps a site
// index into range: the remote block is S wide, and a peer index past it has no
// slot, not a different one.
func externalStanzaRID(layout ridcoord.Layout, segment, peerSiteIndex int32) (int32, error) {
	panic("not implemented")
}

// ridLayoutFor resolves the RID layout in force for sc.
//
// The release gate M3 left open: SlapdMesh.spec.ridLayout ships as live API
// that nothing reads, so a declared layout is silently ignored — which ADR-024
// forbids outright (a spec field that cannot be honoured is refused or
// converged, never ignored).
//
// Contract:
//
//   - No meshRef: ridcoord.DefaultLayout(), and NO I/O at all. A cluster with
//     no mesh has only to be self-consistent; cross-site agreement is a
//     legibility property and not a correctness one (ADR-031 decision 5).
//   - Under meshRef: the mesh's spec.ridLayout, defaulted PER AXIS. A zero axis
//     reads as unset rather than as unusable, because API-server defaulting
//     runs only on the API server's path — a mesh unmarshalled from a manifest,
//     restored from a backup, or carrying only one of the two fields reaches
//     this code with a zero in the other.
//   - An absent ridLayout on a mesh that exists is not an error: it means the
//     defaults, exactly as omitting it documents.
//   - An unreadable mesh — not found, RBAC, a cache miss — means the layout is
//     UNKNOWN, and unknown withholds (RID-PLAN hazard 3). It returns an error,
//     and the Layout it returns alongside is deliberately NOT usable: a caller
//     that drops the error must still withhold rather than converge on
//     defaults, because defaulting a layout that the mesh may have narrowed
//     renumbers every stanza on every pod of this site and no other.
func ridLayoutFor(ctx context.Context, c client.Client, sc *ldapv1alpha1.SlapdCluster) (ridcoord.Layout, error) {
	panic("not implemented")
}

// Reasons an external peer list cannot be turned into remote coordinates. They
// are condition reasons, so each names a different human response: declare an
// index, pick a free one, pick a smaller one, or stop asking for a second link.
const (
	// reasonPeerSiteIndexMissing — a peer declares no siteIndex and the
	// cluster is not under meshRef, so nothing derives one. The CEL rule
	// normally catches this; reaching it means a CR that predates the field or
	// one applied with validation off.
	reasonPeerSiteIndexMissing = "PeerSiteIndexMissing"
	// reasonPeerSiteIndexCollision — two peers declare one siteIndex, so they
	// share a remote slot: one RID for two providers, which is the shared
	// connid state slapd never rejects (RID-PLAN hazard 5).
	reasonPeerSiteIndexCollision = "PeerSiteIndexCollision"
	// reasonPeerSiteIndexOutOfRange — a siteIndex is negative or at or above
	// the layout's S. The remote block has no slot for it.
	reasonPeerSiteIndexOutOfRange = "PeerSiteIndexOutOfRange"
	// reasonReplicasPerPeerUnsupported — a fan-out above 1 asks one pod for a
	// second link to one peer site, and the remote block is S wide precisely
	// because that does not happen (ADR-031 decision 9). Refused until ADR-032
	// redefines the quantity as links per site PAIR.
	reasonReplicasPerPeerUnsupported = "ReplicasPerPeerUnsupported"
)

// judgeExternalPeers decides whether sc's external peer list can be given
// remote coordinates at all, under the layout in force.
//
// Everything it refuses is a RID collision or a RID that does not exist, so the
// answer is the same withhold ADR-031 decision 6 prescribes for a refused
// segment — hence the shared verdict type. Only OK, Reason and Message are
// meaningful here; Segment is not a peer-level quantity and stays zero.
//
// The checks, in the order a human can act on them:
//
//  1. replicasPerPeer >= 2, cluster-wide default or per peer. Refused naming
//     the peer and the value (ADR-031 decision 9).
//  2. A peer with no siteIndex. Under meshRef every peer is derived and carries
//     one; off-mesh it is the deployer's to declare.
//  3. A siteIndex outside 0..S-1 for the layout in force.
//  4. Two peers declaring one siteIndex, named BOTH — symmetric, because
//     neither of them may win the race to write a stanza over the other's.
//
// Pure: the peer list is already resolved on sc (mesh wiring is applied in
// memory before this is reached), so there is no read and no ordering question.
func judgeExternalPeers(sc *ldapv1alpha1.SlapdCluster, layout ridcoord.Layout) ridSegmentVerdict {
	panic("not implemented")
}
