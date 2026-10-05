/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// RID-PLAN hazard 2, the last of its three unguarded paths (docs/BACKLOG.md,
// "Three paths hand `Replace(\"olcSyncRepl\")` a short set with no error").
//
// olcSyncRepl is installed with one atomic modReq.Replace, so a stanza set
// shorter than the pod's current one is a DELETION of the links it omits, not
// a partial update. Three paths in reconcileReplication's external-peer
// resolution skip a peer and carry on building:
//
//  1. a peer whose bind-password Secret cannot be read;
//  2. a discovery peer whose providers have not been resolved yet;
//  3. a peer that resolved to no address at all (the builder's
//     `len(ep.URIs) == 0 { continue }`).
//
// Each tears that peer's cross-site stanzas off every pod of the site until it
// resolves, and nothing reports it: the pass is not marked skipped and the
// database stays Running. ADR-031 decision 6 prescribes the opposite — refuse
// the whole pod's stanza set, report, requeue — and that is what this file
// judges.
//
// It is deliberately the same verdict type as judgeRIDSegment,
// judgeClusterFleet and judgeExternalPeers: the outcome is one withhold with
// one condition, because "can this database's stanzas be given RIDs at all"
// and "can every link they name be resolved at all" have exactly the same
// answer for every pod of the fleet. A second mechanism would publish a second
// condition that can never usefully disagree with the first.

// Reasons an external peer could not be resolved into a stanza. They are
// condition reasons, so they are what a human reads first, and each names a
// different response: fix the Secret or the RBAC that reads it; look at the
// remote cluster and the kubeconfig that reaches it; put an address on the CR.
//
// Three causes, three reasons, following the RIDSegmentUnverified /
// RIDLayoutUnresolved precedent: the withhold is identical, the diagnosis must
// not be, because alerting and slctl key on the reason.
const (
	// reasonPeerBindPasswordUnreadable — the peer names a bind-password Secret
	// and that Secret could not be read: absent, RBAC, a cache miss. The
	// credential is unknown, not empty — see peerResolution.SecretErr for why
	// the two are not the same thing.
	reasonPeerBindPasswordUnreadable = "PeerBindPasswordUnreadable"
	// reasonPeerProvidersUndiscovered — a discovery peer has resolved no
	// provider. On a first bringup this is simply "discovery has not answered
	// yet"; it is also what a bad kubeconfig, a remote RBAC denial, a remote
	// cluster with no Running pods, an unparseable remote pod name and a
	// duplicate remote ordinal all look like from here (ADR-031 M1 publishes
	// a refusal as an empty provider list). The human response is the same in
	// every one of those cases — go look at the remote cluster — which is why
	// they share a reason.
	reasonPeerProvidersUndiscovered = "PeerProvidersUndiscovered"
	// reasonPeerAddressesEmpty — a peer that does not use discovery resolved
	// to no address: no uri, no podAddresses. The CR names a peer site with
	// nothing to connect to, which is a defect in the CR and does not heal on
	// its own.
	reasonPeerAddressesEmpty = "PeerAddressesEmpty"
	// reasonPeerResolutionIncomplete — a declared peer produced no outcome at
	// all, so whether it resolved is not merely bad but UNKNOWN. This is the
	// short set caught one level up: a resolution loop that still `continue`s
	// past a peer drops both the peer's stanzas and its outcome, and a judge
	// that only inspected the outcomes it was handed would call that set
	// complete and approve the deletion it describes. Structural; it fires
	// only on a caller that skipped a peer.
	reasonPeerResolutionIncomplete = "PeerResolutionIncomplete"
)

// peerResolution is what one declared external peer resolved to in this pass —
// the attempt, not the result. One per declared peer, in declared order,
// INCLUDING the ones that failed: a failed peer that produces no outcome is
// the short set this whole file exists to refuse.
//
// It carries only what the three refusals turn on. Everything else a
// resolvedExternalPeer needs (RID, URIs, bind DN, TLS paths) stays at the call
// site, which is where it is already assembled.
type peerResolution struct {
	// Name is the declared peer this outcome belongs to, as it appears in
	// spec.replication.externalPeers.
	Name string

	// Discovery is true when the peer finds its providers through the remote
	// cluster's Kubernetes API. It is what separates "discovery has not
	// answered yet" from "this CR names no address at all" — two states that
	// are indistinguishable by address count alone and that send a human to
	// two different places.
	Discovery bool

	// BindPasswordSecret names the Secret the peer's bind password is read
	// from, or "" when the peer names none (the per-database credential is
	// derived instead, and no read happens).
	BindPasswordSecret string

	// SecretErr is the error from reading that Secret; nil when it was read.
	//
	// An EMPTY password is not an error here, and the distinction is the whole
	// boundary: "could not read it" is not "not set". externalBindIdentity
	// deliberately keeps an empty credential rather than borrowing the
	// per-database one, so that a broken override fails loudly at the consumer
	// instead of being silently masked. A readable Secret carrying an empty
	// password therefore resolves, and only an unreadable one withholds.
	SecretErr error

	// Addresses is how many provider addresses the peer resolved to — the
	// length of the URI list the stanza builder would be handed.
	Addresses int
}

// judgePeerResolutions decides whether this pass's external-peer resolution may
// produce a stanza set at all.
//
// Contract (ADR-031 decision 6, RID-PLAN hazard 2):
//
//   - Every declared peer resolving cleanly → OK, and the caller builds the
//     stanza set exactly as it does today.
//   - Any one peer failing → a refusal, and therefore NO stanza set: the whole
//     database's convergence is withheld for this pass, every pod keeps the
//     stanzas it has, and the caller requeues. Not a set missing that peer —
//     that set is a deletion.
//   - An empty peer list is not a failure. A cluster with no external peers
//     converges its in-cluster stanzas normally.
//   - A declared peer with no outcome is itself a refusal
//     (reasonPeerResolutionIncomplete): an unaccounted peer is unknown, and
//     unknown withholds.
//
// Determinism, so a condition message cannot flap between two equally correct
// texts:
//
//   - Peer-major, in DECLARED order. The first failing peer in
//     spec.replication.externalPeers decides the Reason; the Message names
//     every failing peer with its own cause, in the same order. Peer-major
//     rather than the cause-major order judgeExternalPeers uses, because these
//     three are one question asked of one peer — "did this peer resolve" — and
//     the live loop encounters them peer by peer, so a verdict that mirrors
//     the loop cannot drift from it. judgeExternalPeers' checks are different
//     questions about the whole list, which is why it is ordered the other way.
//   - Within one peer: an unreadable Secret before an empty address list. A
//     peer can fail both ways at once, and the credential is the fault that
//     does not resolve itself.
//
// Pure: no client, no context, no I/O. The caller performs the Secret read and
// the discovery lookup and reports their outcomes here, exactly as
// resolveRIDSegment performs the List that judgeRIDSegment is pure without.
//
// Only OK, Reason and Message are meaningful in the returned verdict; Segment
// is not a peer-level quantity and stays zero.
func judgePeerResolutions(peers []ldapv1alpha1.ExternalPeer, outcomes []peerResolution) ridSegmentVerdict {
	// Not implemented: M4b's implementer fills this in.
	return ridSegmentVerdict{}
}
