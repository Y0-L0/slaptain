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
	"errors"
	"fmt"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/ridcoord"
)

// The mesh derivations of ADR-028 §4: from a SlapdMesh's site list plus this
// operator's own site identity, derive the wiring that used to be typed into
// every site's SlapdCluster by hand — serverIDBase, externalPeers, and the
// identity cross-check that makes both safe.
//
// Everything here is pure: slices and strings in, values and errors out, no
// client and no I/O. That is deliberate and not merely tidy. These functions
// decide values that are baked irreversibly into replicated data (a serverID
// ends up inside every CSN that pod writes), so they have to be exhaustively
// testable at unit speed rather than only observable on a three-site lab.
//
// They take the site SLICE rather than a whole SlapdMesh, and the site selector
// as a plain parameter rather than reading it off a SlapdCluster, because
// neither needs the object: the CR field that carries the selector does not
// exist yet (MESH-PLAN Phase 4 adds it) and the seam is honest without it.
//
// Nothing calls these yet. Phase 3 lands the seam; Phase 4 wires it.

var (
	// errSiteUnknown: a site name was not found in the mesh. Always loud —
	// never a silent fallback to the first site, which would hand this site a
	// serverID stride slot another site is already using.
	errSiteUnknown = errors.New("site is not a member of the mesh")
	// errSiteNotSelected: this site is a mesh member, but the cluster's site
	// selector does not include it — so this cluster does not span this site.
	// Distinct from errSiteUnknown because the caller's remedy differs: fix the
	// selector, not the mesh.
	errSiteNotSelected = errors.New("site is not in this cluster's site selector")
	// errSiteIdentity: this operator has no site identity (SITE_NAME unset).
	// Legal in general — it means "no mesh features" (ADR-028 §4) — but not
	// when something has asked for a mesh derivation.
	errSiteIdentity = errors.New("no site identity configured")
	// errMeshSites: the site list itself is unusable — empty, or carrying a
	// blank or duplicated name, or a missing, negative or duplicated
	// serverIDIndex. Both duplicate classes are two sites claiming one
	// serverID stride slot: the collision ADR-028 §4 names as the first thing to
	// verify.
	errMeshSites = errors.New("malformed mesh site list")
	// errServerIDCeiling: the stride-per-site scheme has run out of room. See
	// maxSiteIndex.
	errServerIDCeiling = errors.New("site index exceeds the serverID stride ceiling")
	// errSiteIndexPastLayout: a site declares a serverIDIndex at or above the
	// mesh's own ridLayout.maxSites, so the remote block of every segment has
	// no slot for it and no syncrepl RID can address that site (ADR-028
	// amendment 2026-10-05, ADR-031 decision 4). The CEL rule on SlapdMeshSpec
	// refuses it at admission; this is the same refusal for a mesh that
	// reached the cluster another way — an object older than the rule, a
	// restore, validation turned off.
	errSiteIndexPastLayout = errors.New("site index has no slot in the RID layout")
)

const (
	// serverIDStride is the per-site stride: a site's serverIDBase is its
	// declared serverIDIndex times this value.
	//
	// It is 0x10, and the hex matters. olcServerID is stamped into every CSN a
	// pod writes, and a CSN renders that field as three hex digits
	// (timestamp#count#sid#mod). Under the original decimal stride of 100 the
	// numbers everyone reasoned in — "101 is the second site's first pod" —
	// were not the numbers anyone could read: 101 renders as 065, 201 as 0c9,
	// and every diagnosis began by converting. A stride of 16 puts the site in
	// the leading hex digit and the pod ordinal in the trailing one, so the
	// sid reads as the coordinate it is: 011 is site 1 pod 0, 0c3 is site 12
	// pod 2. Nothing downstream parses it — ADR-017's derivation is
	// serverIDBase + ordinal + 1 whatever the stride is — so the change buys
	// legibility and costs only the migration below.
	//
	// 4 bits for the pod and 8 for the site is the deliberate split of slapd's
	// 12-bit sid space, and it is NOT symmetric by accident. Pod ordinals are
	// reused: a StatefulSet that scales down and back up takes ordinal 2
	// again. Site indices are retired, never reused — a decommissioned site's
	// index stays burnt so a later site cannot inherit its CSN lineage — so
	// the site axis is the one that monotonically consumes address space and
	// the one that gets the spare nibble. 15 pods per site is ample where
	// N-way multi-provider is already poor practice past about 5, and
	// read-only replicas carry no serverID at all (ADR-017).
	//
	// Changing this value again is a MIGRATION, not an edit. A site that
	// already exists would file its future writes under a different sid from
	// its history, its old sid would sit in every peer's contextCSN vector
	// forever without advancing, and per-site CSN tracking would stop being
	// comparable with itself (MESH-PLAN hazard 2, ADR-017). The move from 100
	// to 16 was taken while the only deployments were labs, recreated rather
	// than migrated; see ADR-028's amendment of 2026-10-05.
	serverIDStride = 16

	// maxSiteIndex is where the scheme runs out. slapd caps olcServerID at 4095
	// and SlapdCluster.spec.replication.serverIDBase at 4094, so the last index
	// that can be given a whole stride slot is 255 (base 4080, leaving pod
	// serverIDs 4081..4095).
	//
	// It is a bound on the INDEX, not on the number of sites: indices are
	// sparse-legal (a decommissioned site's index is retired rather than
	// reused), so a two-site mesh can breach this just as easily as a 256-site
	// one. That sparseness is exactly why the site axis is the wide one.
	//
	// It is also, in practice, the slack ceiling of the two that bound a site
	// index. ridLayout.maxSites (default 12) is far lower and is the one a
	// deployer actually meets; validateSiteIndicesAgainstLayout checks this
	// one first anyway, because a limit of the server cannot be widened and
	// deserves the message that says so.
	//
	// A breach is refused loudly rather than wrapped, clamped or shared: every
	// alternative silently gives two sites one slot, which is the exact
	// failure mode this whole scheme exists to prevent.
	maxSiteIndex = 4094 / serverIDStride // 255

	// maxPodsPerSite is the other half of the split: a site may hold at most
	// stride-1 pods, because ordinal `stride-1` at site N takes the sid the
	// stride reserved as site N+1's base. See
	// validateLayoutAgainstServerIDStride for where this is enforced and why
	// it is enforced on the declared layout rather than on spec.replicas.
	maxPodsPerSite = serverIDStride - 1 // 15
)

// meshSiteIndex resolves a site name to its POSITION in the slice, applying the
// shared normalisation rules and validating the whole site list on the way.
//
// The position is an addressing detail — where the entry sits — and is
// deliberately not the site's serverID slot; that is the serverIDIndex field
// the validation below enforces. Callers that want the slot read the field
// through this position.
//
// The list is validated wholesale, names and indices together, so a malformed
// mesh fails every derivation rather than half of them: a mesh with two sites
// on one stride slot is broken for the peer set too, even though peers never read an
// index.
//
// Comparison semantics are decideSeedSite's, deliberately: both sides trimmed,
// case NOT folded (ADR-028 §3 amendment). A site is whatever the operator chart
// was given; folding would let two distinct SITE_NAME values collide, which is
// precisely the stride-slot collision §4 warns about.
func meshSiteIndex(sites []ldapv1alpha1.MeshSite, name string) (int, error) {
	if len(sites) == 0 {
		return 0, fmt.Errorf("%w: no sites declared", errMeshSites)
	}
	seen := make(map[string]struct{}, len(sites))
	seenIdx := make(map[int32]string, len(sites))
	idx := -1
	want := normalizeSiteName(name)
	for i, s := range sites {
		n := normalizeSiteName(s.Name)
		if n == "" {
			return 0, fmt.Errorf("%w: site %d has an empty name", errMeshSites, i)
		}
		if _, dup := seen[n]; dup {
			return 0, fmt.Errorf("%w: site %q is declared twice — two sites claiming "+
				"one identity share one serverID stride slot", errMeshSites, n)
		}
		seen[n] = struct{}{}

		// The index is required and never inferred. Defaulting it — to the
		// position, or to anything else — would put back the silent
		// renumbering the explicit field exists to remove, and would do it on
		// precisely the edit nobody reviews carefully.
		if s.ServerIDIndex == nil {
			return 0, fmt.Errorf("%w: site %q has no serverIDIndex; it is required "+
				"and is never inferred from list position", errMeshSites, n)
		}
		si := *s.ServerIDIndex
		if si < 0 {
			return 0, fmt.Errorf("%w: site %q has a negative serverIDIndex (%d)", errMeshSites, n, si)
		}
		if other, dup := seenIdx[si]; dup {
			return 0, fmt.Errorf("%w: sites %q and %q both claim serverIDIndex %d — "+
				"they would share one serverID stride slot, and slapd validates nothing: "+
				"contextCSN keeps the highest CSN per serverID, so each site's writes "+
				"read as already-seen at the other", errMeshSites, other, n, si)
		}
		seenIdx[si] = n

		if n == want && idx < 0 {
			idx = i
		}
	}
	// The whole list is validated before the lookup verdict, so a malformed
	// mesh reports as malformed even when the requested site happens to resolve.
	if want == "" {
		return 0, fmt.Errorf("%w: set siteName on the operator chart (SITE_NAME)", errSiteIdentity)
	}
	if idx < 0 {
		return 0, fmt.Errorf("%w: %q (names are compared verbatim, case included)", errSiteUnknown, want)
	}
	return idx, nil
}

// The operator's access to the new kind. Read-only and permanently so: a mesh
// has no single writer by construction, so a corrective write would be action
// on state this operator does not own — ADR-026 R2, restated for the mesh layer
// in ADR-028 §6 ("report and stall"). Status is included because ADR-028 §6's
// standing parity condition will land there; the spec is never written.
//
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdmeshes,verbs=get;list;watch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdmeshes/status,verbs=get;update;patch

// validateSelfSite cross-checks this operator's own site identity against the
// mesh's site list — the Phase 1 resolver's promotion, now that sites[] exists
// to check against.
//
// A configured identity that is absent from the mesh is an error the caller
// must surface, never a silent default to some site. Defaulting is how two
// sites end up sharing a serverID stride slot, and that failure is invisible: slapd
// validates nothing, contextCSN tracks the highest CSN per serverID, and the
// only symptom is that some writes never propagate.
func validateSelfSite(sites []ldapv1alpha1.MeshSite, identity string) error {
	_, err := meshSiteIndex(sites, identity)
	return err
}

// serverIDBaseForSite derives the site's serverID stride slot from its DECLARED
// serverIDIndex: index × serverIDStride, with ADR-017's serverIDBase + ordinal
// + 1 applied downstream unchanged. Index 2, pod ordinal 3 → sid 36, which is
// where the hex stride earns its keep: the CSN field reads 024, site digit then
// pod digit, with no arithmetic between the number on screen and the pod it
// names.
//
// The number comes from the field and never from the site's position in
// spec.sites. That is the point: olcServerID is baked into every CSN a pod has
// written, so a site whose number moves files its future writes under a
// different sid from its history and per-site CSN tracking stops being
// comparable with itself. Positional derivation would trigger that on an edit
// nobody thinks twice about — inserting a site in the middle, or a formatter
// sorting the list. Bound to a field, spec.sites is freely reorderable and the
// identity is immovable.
//
// A three-site mesh declaring indices 0/1/2 yields bases 0/16/32, whose pods
// read 001.. / 011.. / 021.. in a CSN. This is NOT what a pre-stride mesh ran
// (it had 0/100/200, reading 001 / 065 / 0c9), and that discontinuity is
// deliberate: see serverIDStride for why re-striding an existing site is a
// migration and ADR-028's amendment of 2026-10-05 for how the labs took it.
//
// Indices are mesh-wide, never per-cluster-selector: two clusters on one mesh
// spanning different subsets must still agree about who owns stride slot 1. (Two
// clusters may safely share serverID values with EACH OTHER — a CSN is only
// compared within one replication topology, and clusters with different
// suffixes never exchange CSNs. What must not happen is two SITES sharing a
// stride slot inside one topology.)
//
// See serverIDStride for why the stride is not a free choice, and maxSiteIndex
// for where it stops.
func serverIDBaseForSite(sites []ldapv1alpha1.MeshSite, identity string) (int32, error) {
	pos, err := meshSiteIndex(sites, identity)
	if err != nil {
		return 0, err
	}
	// meshSiteIndex has already proven the pointer is non-nil, non-negative and
	// unique across the mesh.
	idx := *sites[pos].ServerIDIndex
	if idx > maxSiteIndex {
		return 0, fmt.Errorf("%w: site %q declares serverIDIndex %d; the stride-per-site "+
			"scheme allows at most %d before olcServerID's 4095 limit",
			errServerIDCeiling, normalizeSiteName(identity), idx, maxSiteIndex)
	}
	return idx * serverIDStride, nil
}

// validateSiteIndicesAgainstLayout refuses any site whose declared
// serverIDIndex cannot be addressed, under the two ceilings that bound it.
//
// They are checked in that order on purpose. slapd's serverID ceiling is a
// property of the server and cannot be widened, so an index past it gets the
// reason that says so; the layout's maxSites is configurable, so an index past
// only that one gets a reason naming the field a human can actually change.
//
// The whole mesh is validated, not just this site: a peer whose index has no
// remote slot breaks the stanza this cluster would write FOR that peer, and
// the admission rule refuses the SlapdMesh object wholesale for the same
// reason. Sites that declare no index are left to meshSiteIndex, which refuses
// them with the message that question deserves.
//
// layout is the mesh's own, defaulted per axis — the same resolution
// ridLayoutFor performs, repeated here rather than plumbed because this
// derivation is pure and must stay so.
func validateSiteIndicesAgainstLayout(sites []ldapv1alpha1.MeshSite, layout ridcoord.Layout) error {
	for _, s := range sites {
		if s.ServerIDIndex == nil {
			continue
		}
		idx := *s.ServerIDIndex
		if idx > maxSiteIndex {
			return fmt.Errorf("%w: site %q declares serverIDIndex %d; the stride-per-site "+
				"scheme allows at most %d before olcServerID's 4095 limit",
				errServerIDCeiling, normalizeSiteName(s.Name), idx, maxSiteIndex)
		}
		if idx >= layout.MaxSites {
			return fmt.Errorf("%w: site %q declares serverIDIndex %d and this mesh's "+
				"spec.ridLayout.maxSites is %d, so a peer's RID slot (podsPerSite + %d) falls "+
				"outside the %d-wide remote block and no syncrepl RID can address that site. "+
				"Declare an index in 0..%d, or widen spec.ridLayout.maxSites — which renumbers "+
				"the mesh once",
				errSiteIndexPastLayout, normalizeSiteName(s.Name), idx, layout.MaxSites,
				idx, layout.MaxSites, layout.MaxSites-1)
		}
	}
	return nil
}

// meshRIDLayout is the layout a SlapdMesh spec declares, defaulted per axis.
// The twin of ridLayoutFor's tail, for the pure derivation path which holds the
// spec already and must do no I/O. A zero axis is unset rather than unusable,
// because API-server defaulting runs only on the API server's path.
func meshRIDLayout(mesh *ldapv1alpha1.SlapdMeshSpec) ridcoord.Layout {
	layout := ridcoord.DefaultLayout()
	if mesh == nil || mesh.RIDLayout == nil {
		return layout
	}
	if mesh.RIDLayout.PodsPerSite != 0 {
		layout.PodsPerSite = mesh.RIDLayout.PodsPerSite
	}
	if mesh.RIDLayout.MaxSites != 0 {
		layout.MaxSites = mesh.RIDLayout.MaxSites
	}
	return layout
}

// externalPeersForSite derives the cross-site peer set for one site: every
// other site the cluster spans, addressed by ADR-007 dynamic discovery.
//
// The output reproduces what tests/e2e.sh builds by hand today — per-peer name,
// port 1025, the peer's CA Secret as tlsSecretName, and the peer's kubeconfig
// Secret for discovery — so Phase 4 can be checked by comparing cn=config
// against a hand-configured cluster rather than by reading the code twice.
//
// Three things it deliberately does not set:
//
//   - bindDN / bindPasswordSecretName. The operator derives the bind identity
//     per database (ADR-027's node-local cn=repl-<db>,cn=slaptain-auth plus that
//     database's replication password). A peer-level value spans every
//     SlapdDatabase and so can be correct for at most one of them — ADR-019 R9's
//     axis argument. Those fields survive as the ADR-011 override for foreign
//     sources.
//   - replicasPerPeer. Left nil so the CRD default (1, the diagonal) applies,
//     exactly as an unset field does today. Writing the default in explicitly
//     would make the derived object differ textually from the hand-written one
//     for no behavioural gain.
//   - uri / podAddresses. A mesh describes sites, and a site is reached through
//     its API server; static addressing is the pre-discovery path and stays
//     hand-configured.
//
// selector is the cluster's site subset (MESH-PLAN Phase 4's `sites` field).
// Empty or nil means the whole mesh, which is the default a cluster gets when
// it says nothing. The selector narrows the PEERS only — it never touches the
// serverID derivation above.
//
// Peer ORDER follows the mesh, not the selector, so the derived list is stable
// across reconciles and an unchanged mesh produces an unchanged cn=config.
func externalPeersForSite(sites []ldapv1alpha1.MeshSite, selector []string, identity string, replicasPerPeer *int32) ([]ldapv1alpha1.ExternalPeer, error) {
	selfIdx, err := meshSiteIndex(sites, identity)
	if err != nil {
		return nil, err
	}

	// Resolve the selector against the mesh first: a selector naming a site the
	// mesh does not have is a typo that would otherwise silently shrink the
	// peer set, and a shrunken peer set is a half-connected topology that still
	// looks healthy on both sides of the links that do exist.
	var selected map[int]struct{}
	if len(selector) > 0 {
		selected = make(map[int]struct{}, len(selector))
		for _, name := range selector {
			idx, err := meshSiteIndex(sites, name)
			if err != nil {
				return nil, fmt.Errorf("site selector: %w", err)
			}
			selected[idx] = struct{}{}
		}
		if _, ok := selected[selfIdx]; !ok {
			return nil, fmt.Errorf("%w: %q spans %v", errSiteNotSelected,
				normalizeSiteName(identity), selector)
		}
	}

	var peers []ldapv1alpha1.ExternalPeer
	for i, s := range sites {
		if i == selfIdx {
			continue // never peer with yourself: a stanza pointing at its own pods is a loop
		}
		if selected != nil {
			if _, ok := selected[i]; !ok {
				continue
			}
		}
		kubeconfig := s.KubeconfigSecretFor()
		// The peer's coordinate in the RID matrix's remote block is its
		// DECLARED serverIDIndex, never its position in spec.sites: the list
		// is freely reorderable and gaps are legal, so position names a
		// different site tomorrow (MESH-PLAN hazard 2). meshSiteIndex has
		// already validated every site's index, so the pointer is non-nil
		// here; it is read defensively anyway, because the alternative to a
		// nil check is a panic inside a reconcile.
		var siteIndex *int32
		if s.ServerIDIndex != nil {
			v := *s.ServerIDIndex
			siteIndex = &v // copied, so the derived peer never aliases the mesh object
		}
		peers = append(peers, ldapv1alpha1.ExternalPeer{
			Name:          normalizeSiteName(s.Name),
			Port:          externalPeerLDAPSPort,
			TLSSecretName: s.CASecretNameFor(),
			// ADR-031 decision 4: under meshRef nothing is hand-written, so
			// the derivation is what supplies each peer's remote coordinate.
			// Without it a mesh cluster's cross-site stanzas have no RID.
			SiteIndex: siteIndex,
			// The cluster's fan-out, stamped onto every derived peer. A mesh
			// cluster cannot reach externalPeers[].replicasPerPeer — the whole
			// field is derived and setting it is refused — so without this the
			// mesh path was pinned at 1 with no way to say otherwise
			// (ADR-028 amendment, 2026-09-28). nil stays nil, which the stanza
			// builder already reads as 1.
			ReplicasPerPeer: replicasPerPeer,
			Discovery: &ldapv1alpha1.ExternalPeerDiscovery{
				KubeconfigSecret: kubeconfig,
			},
		})
	}
	return peers, nil
}

// externalPeerLDAPSPort is slapd's non-privileged LDAPS container port — the
// port every slaptain pod listens on and the one tests/e2e.sh has always
// written into its peer specs. It matches the ExternalPeer.port CRD default;
// it is set explicitly here so the derived peer is complete on its own rather
// than depending on API defaulting having run.
const externalPeerLDAPSPort = 1025

// errPodsPerSitePastStride: the mesh declares a RID layout whose podsPerSite
// exceeds what one serverID stride slot can hold. See
// validateLayoutAgainstServerIDStride.
var errPodsPerSitePastStride = errors.New("podsPerSite exceeds the serverID stride")

// validateLayoutAgainstServerIDStride refuses a RID layout whose podsPerSite
// cannot fit inside one serverID stride slot.
//
// The two schemes are independent — a RID is a coordinate in ADR-031's matrix,
// a serverID is a site slot plus a pod ordinal — but they share one physical
// quantity: how many pods a site may run. The RID matrix would happily accept
// podsPerSite 64; the serverID stride would then hand site N's ordinal 15 the
// sid that is site N+1's base, and two pods in different sites would write
// under one sid. Nothing reports that. slapd validates no serverID, peers
// accept any sid seen on the wire (ADR-017), and the symptom is each site's
// writes reading as already-seen at the other.
//
// The bound is enforced on the DECLARED layout rather than on spec.replicas on
// purpose. The operator already refuses replicas > podsPerSite
// (reasonRIDPodsPerSiteExceeded), so capping podsPerSite keeps that one check
// the single bound on replicas. A second, independently-reasoned replicas
// check could disagree with the first, and two bounds on one quantity is how a
// cluster ends up legal by one and illegal by the other.
func validateLayoutAgainstServerIDStride(layout ridcoord.Layout) error {
	if layout.PodsPerSite > maxPodsPerSite {
		return fmt.Errorf("%w: spec.ridLayout.podsPerSite is %d and the serverID stride is %d, "+
			"so a site's pod ordinal %d would take serverID %d — the base of the NEXT site, "+
			"whose own pods would then write under a sid this site already uses. Declare "+
			"podsPerSite in 1..%d",
			errPodsPerSitePastStride, layout.PodsPerSite, serverIDStride,
			maxPodsPerSite, serverIDStride, maxPodsPerSite)
	}
	return nil
}
