# ADR-028: The mesh is a layer of its own — `SlapdMesh`, and what "mesh-scoped" means

**Status:** Accepted (implemented and validated on a three-site lab, 2026-09-18)
**Date:** 2026-09-17

## Context

Every slaptain CRD is namespaced and describes one Kubernetes cluster.
`SlapdCluster`, `SlapdDatabase` and `SlapdSchema` say nothing about the other
sites and cannot: the sites are independent clusters sharing no API server,
which is the architecture (ADR-007, ADR-016).

A multi-site deployment is nevertheless one distributed system, and it works
only while N independent clusters agree on a set of facts. Nothing holds those
facts today. They live in `tests/e2e.sh` — a test harness — as an ordered
sequence of `kubectl` and `helm` calls, so standing up a mesh outside the lab
means reading 1500 lines of shell and reproducing what it does by hand.

The agreement is larger than it looks. From the current code:

| fact | rule across sites | enforced by |
|---|---|---|
| `SlapdDatabase` name and `suffix` | identical | nothing |
| `spec.replication.ridBase` | identical per database across sites | nothing |
| `spec.replication.ridBase` | unique between databases within a site | nothing (`docs/BACKLOG.md`) |
| `<db>-credentials` / `replication-password` | identical (ADR-008) | nothing; `e2e.sh` pre-creates the Secret before the CR exists |
| `SlapdSchema` set | identical, or a superset everywhere | nothing |
| `spec.replication.serverIDBase` | **differs** — one decade per site | nothing |
| `spec.seed` | exactly one site carries it | ADR-025's withhold belt, partially |
| CA cross-trust Secrets | N×(N−1) | nothing |
| `externalPeers` | N×(N−1) | nothing |

Eight invariants, no enforcement, and every violation fails silently rather than
loudly. Two of them have already cost us: the 19-day dead `db2` link was a
violated uniform-password invariant, and the ADR-025 glue suffix a violated
single-seed invariant.

Two failure modes deserve stating, because "the resource is simply missing at one
site" understates both:

- A missing `SlapdDatabase` is not an absence but a broken link. The other sites'
  stanzas name that suffix, the search returns `noSuchObject`, and because a
  failed logbase search halts delta-syncrepl outright (ADR-019, corrected
  Consequences), the link stops rather than degrades.
- A missing `SlapdSchema` diverges the DIT. Entries replicate in carrying an
  objectClass that site does not know, the add fails, and that site silently
  lacks entries the others have.

## Options considered

**A management cluster running a mesh controller.** Rejected. It reintroduces the
central control plane the architecture exists without, makes one site special,
and puts a new availability dependency in front of a system whose selling point
is per-site autonomy.

**One flat `SlapdMesh` carrying sites, databases and schemas.** Rejected once
several `SlapdCluster`s can share a set of sites: the site inventory would be
duplicated per cluster, and "whose databases?" has no coherent answer in a mesh
object that several clusters reference. The decomposition below removes the
duplication instead of relocating it.

**Pure GitOps with hand-maintained per-site overlays.** Rejected as the *whole*
answer, kept as part of it. Per-site Kustomize or Helm reconciles steady state
correctly, but it does not establish the pairwise trust bootstrap, and nothing in
that stack knows the eight invariants — a mistyped `serverIDBase` renders,
applies and reconciles cleanly.

**The operator fans out: create missing `SlapdDatabase`/`SlapdSchema` objects on
peer clusters.** Rejected, and recorded because it is the obvious idea. It writes
to state on a cluster the operator does not own (ADR-026 R2), it has no
single-writer rule so N operators race to create the same object, and it makes
every site's control plane a dependency of every other's. Fan-out belongs in the
GitOps layer, which already does it well.

**Promote `tests/e2e.sh` to a user-facing installer.** Rejected. A test fixture
carries test assumptions — throwaway CAs, hardcoded NodePorts, PVC wipes, seed
blocks stripped with `awk` — and promoting it means maintaining a shell program
against a contract it was never written to hold.

## Decision

### 1. Four layers, each referencing the one above

    SlapdMesh  ←meshRef—  SlapdCluster  ←clusterRef—  SlapdDatabase / SlapdSchema

`SlapdMesh` describes the sites and the fabric between them, and nothing else:

- `sites[]` — name, API endpoint for remote peer discovery, kubeconfig Secret name
- `network` — `pod-routed` | `multus`, plus the NAD reference
- trust — per-site CA Secret names, or a cert-manager issuer

Trust belongs here because trust is between *sites*: site B's CA is site B's CA
whichever LDAP cluster is talking to it. Per-cluster server certificates are
issued from it.

`SlapdCluster` gains `meshRef` and a `sites` selector (defaulting to every site
in the mesh), so a cluster may span a subset — cluster X over A+B, cluster Y over
A+B+C, one mesh. Without the selector, overlapping topologies would need
overlapping meshes, reintroducing the duplication this layering removes.

`SlapdDatabase` and `SlapdSchema` keep `clusterRef` unchanged and are thereby
transitively mesh-aware.

`SlapdMesh` is namespaced, like everything else: uniform RBAC, and two teams can
run independent meshes in one cluster.

### 2. "Mesh" is the noun for the sites; "full mesh" stays an adjective

The corpus already uses "mesh" two ways: as a noun for the multi-site deployment
("three-site mesh", "mesh-wide" — the dominant use, and what ADR-025 and ADR-027
mean throughout), and inside `docs/REPLICATION.md` as a noun for the in-cluster
pod topology ("the RW mesh", "a three-pod mesh"). The industry term for the first
is the one we adopt — Cilium's ClusterMesh names exactly this — so the CR is
`SlapdMesh` and the noun is reserved for it.

The adjective survives untouched: "full mesh" is standard topology vocabulary and
"This is a full mesh: 6 persistent connections for 3 nodes" stays as written. We
only stop using bare "mesh" as a *noun* for the in-cluster topology — "the RW
mesh" becomes "the RW pods", "no in-cluster mesh" becomes "no in-cluster syncrepl
stanzas". A handful of phrases, worth fixing before the CRD name lands, because
afterwards every stale use reads as a reference to the object.

### 3. A mesh-scoped resource carries no per-site fields

This is the rule that makes parity achievable rather than merely desirable. A
mesh-scoped CR must be byte-identical at every site; therefore it may not contain
a field whose value differs per site, and any per-site behaviour must be
expressed by *naming* a site rather than by editing the object.

`spec.seed` is the only current violation. Today it is present at the founder and
stripped everywhere else — not a per-site value but a per-site *edit*, performed
by `strip_seed_block`'s `awk` in `tests/e2e.sh`, which is the clearest possible
evidence that the field is modelled wrong. It becomes a declaration, `seed.site:
site-a`, read identically everywhere, with each operator comparing that name
against its own identity. ADR-025 stops being a deployment procedure and becomes
a property of the spec.

`bootstrapFrom` is *not* a violation. Restore is create-everywhere by design: the
slapadd-all-pods machine loads every RW pod directly rather than letting syncrepl
carry the data (ADR-014, amendment 2026-06-09), and the same reasoning extends
across sites. `slapcat` emits `entryUUID` and `entryCSN` and `slapadd` preserves
them, so N sites restoring one artifact produce identical entries rather than the
independent creations that produce an ADR-025 glue suffix. Argued, not measured —
multi-site `bootstrapFrom` has never been run.

→ Identical-across-sites becomes checkable as existence plus a hash, with no
field-by-field comparison to get wrong.

*Implemented for `seed` on the operator side, 2026-09-17 (MESH-PLAN Phase 2a),
which settled two comparison semantics this ADR had not specified:*

- **Case is not folded.** `site-a` and `SITE-A` are different sites. A site is
  whatever the operator chart was given, and folding would let two distinct
  `SITE_NAME` values collide — the precise failure §4 warns about, where two
  sites claiming one identity collide their `serverID` decades.
- **Both sides are trimmed**, selector as well as env var. A YAML-sourced
  `site: "site-a\n"` silently demoting the founder to a non-founder is the same
  bug the env var is trimmed for, and it would be invisible.

The gate is evaluated before any LDAP connection, so a non-founder never dials a
pod to seed. ADR-025's evidence belt is untouched and independent: this gate acts
on what the spec *declares*, the belt on what the directory shows has already
*happened*. Both still fire.

### 4. The operator derives the wiring; one per-site fact lives outside all CRs

From `meshRef` plus its own identity the operator derives `serverIDBase` (from the
site's index), `externalPeers` (from `sites[]`), the network mode and the trust
wiring. All four drop out of the user-facing spec.

Two `SlapdCluster`s on one mesh may safely share `serverID` values: a CSN is
compared only within one replication topology, and clusters with different
suffixes never exchange CSNs. That stops holding if two such clusters are ever
merged, and a decade-per-site scheme tops out at index 40 against
`olcServerID`'s 4095.

*Settled in Phase 3, 2026-09-17: the decade is declared, not positional.* Each
mesh site carries a required `serverIDIndex`, and the base is `index × 100`. The
arithmetic is unchanged — and deliberately decimal, because `serverID 203` reads
as "site 2, pod 3" in a CSN, which is how these are actually debugged — but the
index no longer comes from the site's position in `sites[]`. Deriving it from
position would have meant that reordering a YAML list silently renumbers every
site after the edit, splitting those pods' CSN history across two sids: a booby
trap on an edit nobody reviews. Indices must be unique and may be sparse, so
decommissioning a site does not renumber its neighbours. We chose this while the
only live deployment was a lab that could have been migrated — the point is that
it needed no migration at all: indices 0/1/2 reproduce the existing 0/100/200
exactly.

*Peer names are mesh site names.* `peer.Name` is not cosmetic — it is the
directory component of the CA mount path and therefore appears in every external
stanza's `tls_cacert`. Naming derived peers after mesh sites means a mesh-driven
cluster and a hand-written one agree only when both use the same site names; see
the Phase 4 correction in `docs/MESH-PLAN.md`.

*Implemented 2026-09-17 (MESH-PLAN Phase 4), with three decisions the ADR had
left open:*

- **The derivation is in-memory only.** It is never written back to
  `spec.replication`. Materialising it would make each site's `SlapdCluster`
  differ from its neighbours', which destroys the byte-identical property §3
  rests on — so we pay the price instead: every consumer of a derived field must
  resolve `meshRef` itself, and one that forgets sees an unwired cluster rather
  than a wrong one.
- **`meshRef` plus an explicitly set derived field is refused**, with a
  `MeshResolved: False` condition and `phase: Error`, one reason per failure
  cause. A failed resolution reconciles nothing else and requeues — defaulting
  to an empty peer set would tear every cross-site stanza off every pod on the
  next pass, which is a far worse outcome than stalling loudly.
- **`serverIDBase: 0` is read as unset**, because it is a plain `int32` with a
  CRD default of 0 and the two are indistinguishable on the wire. Accepted
  rather than changing the field to a pointer: the ambiguity can only cost a
  missing *error message*, never a wrong serverID, since the mesh's value is
  collision-free by construction and deriving over a typed 0 can only move
  toward correctness. Note the asymmetry with `serverIDIndex`, which is a
  pointer precisely because it had no such safe direction.
  A `serverIDBase` that *agrees* with the mesh is not a conflict, and that check
  is deliberately identity-free — otherwise the same object would be valid at one
  site and invalid at its neighbour, breaking byte-identity for exactly the field
  most likely to linger through an upgrade.

The operator must still answer "which site am I?". That fact belongs in the
operator's own installation config, not in any CR — putting it in `SlapdMesh`
would destroy the byte-identical property the whole design rests on. It is then
the single per-site fact in the system, and getting it wrong at two sites
collides their `serverID` decades, so it is the first thing `verify` asserts.

### 5. The packaging unit is a chart, not a bigger CR

"One thing to apply" is a packaging property, not a data-model property. We ship
`charts/slapd-mesh` containing all four kinds, applied with identical values to
every site — one chart, or one HelmRelease per site pointing at the same chart
and values. The layered model stays clean and the single-artifact convenience is
kept.

We accept what this gives up against a single fused CR: one object would be
atomic and validatable in the API, so an incoherent bundle could be refused at
admission and coherence would be a property of etcd. With a chart, coherence is
established at apply time and verified afterwards.

### 6. Parity checking: a continuous signal and an out-of-band prober

The operator owns the standing signal. Given the mesh's site list and peer
credentials it lists peer `SlapdDatabase`/`SlapdSchema` objects and surfaces a
condition — a peer missing a database, a divergent `ridBase`. `slctl mesh verify`
owns diagnosis, reading N contexts from wherever it runs, which is what you want
precisely when the operator cannot reach its peers.

Both read the same predicate, following `internal/suffixprobe` (one classifier,
several call sites) rather than growing a second copy of the comparison.

Every invariant in the table above is decidable from the Kubernetes API alone —
confirmed by running all nine against the three-site lab on 2026-09-17, where no
check needed an LDAP bind. `mesh verify` therefore works while replication is
broken, which is when it is worth having. It also draws the line against existing
tooling: `mesh verify` compares *specs* across sites over the k8s API, `slctl
inspect` compares *runtime state* within a site over LDAP. Neither subsumes the
other.

Both are read-only, permanently. A mesh has no single writer by construction, so
a repair write is exactly the corrective action on unowned state that ADR-026 R2
forbids. Report and stall.

Peer API access exists only in ADR-007 discovery mode; in `uri` mode the operator
has no remote credentials and must report parity as **unknown** rather than as
passing. Unreadable evidence never counts toward a pass — the same rule ADR-008's
CSN judging already follows.

### 7. Imperative bootstrap, declarative steady state

Cross-cluster trust is pairwise and circular — site A needs site B's CA before
site B exists — and no declarative system establishes it without a management
cluster. Every comparable project reaches the same conclusion: Cilium ClusterMesh
connects sites with a CLI and requires a unique cluster ID per cluster, Istio and
Submariner do the equivalent. (Recalled, not cited — check before quoting.)

We adopt the split rather than fighting it. Bootstrap — trust, CAs, shared
credentials, operator install — is a CLI run once per site pair. Steady state is
the chart above, applied by whatever the user already runs.

For the two bootstrap problems we adopt the ecosystem rather than growing our
own: cert-manager and trust-manager for the CA and its distribution, External
Secrets or documented SOPS for the shared `replication-password`. ADR-027 already
made the Secret the source of truth and made rotation work, so the operator side
is ready. This deletes `gencert.sh` and `setup_cross_trust` rather than porting
them, and buys CA rotation, which today's throwaway self-signed certificates do
not have.

## Consequences

Of the eight invariants, four stop existing — `serverIDBase`, `externalPeers`,
network mode and trust wiring are derived. The other four survive as properties
of an artifact: identical `SlapdDatabase`, `SlapdSchema`, `seed.site` and
credential references, applied everywhere by one chart and checked by a hash.
Construction for the first four, detection for the rest, and the difference is
real: a derived `serverIDBase` cannot be wrong, whereas a site that never
received the chart is *detectable* rather than impossible.

The mesh becomes documentable. A user gets one chart, one values file and a
bootstrap command instead of a test script, and the invariant table becomes user
documentation rather than tribal knowledge.

`verify` is load-bearing rather than a convenience, since it is the only
mechanism covering the four surviving invariants. Both silent-divergence bugs in
this project's history would have been caught in seconds by a check that already
knew what to compare.

`slctl` grows a multi-context mode. It is single-context today, so credential
resolution, context iteration and partial-failure reporting are new design, and a
site that cannot be reached must report as unknown rather than folding into a
pass.

Mesh changes need skew tolerance. Adding a site means editing the mesh everywhere,
and during the rollout sites run different generations — site A already knows a
database that site B does not. Mesh spec changes must therefore be additive and
tolerant, the same requirement ADR-027's one-directional migration window
imposed. This is a design constraint on every future mesh field, not an
implementation detail.

Blast radius grows. One bad mesh definition applied everywhere hits every site at
once, where per-site manifests would localise the mistake. More visible, and more
dangerous.

Continuous parity checking needs RBAC we do not have: the operator reads CRs on
peer clusters, so `scripts/mesh-authorize-peers.sh` must grant CR reads
alongside pod discovery.

The e2e suite is rewritten onto the tooling, which is the proof the tooling is
real. It is also the project's most load-bearing asset, so we port it
incrementally behind a flag — not as a big-bang swap, and not immediately before
a deadline that depends on it.

Nothing here is implemented. The invariant table was derived from reading the
code and then **audited by hand against the three-site lab on 2026-09-17**: 21
checks over nine invariants, all green, so the lab holds every invariant today
and none of them is hypothetical. The audit corrected the table once — `ridBase`
is two invariants, not one — and established that the whole set is decidable from
the Kubernetes API. The prototype is ~90 lines of shell, which bounds what the Go
implementation has to do.

What the audit does not cover: it reads specs, so it would not catch a spec that
is correct everywhere while a pod's `cn=config` has drifted from it. That is
`slctl inspect`'s half, and neither tool is a substitute for the other.

## Implemented (2026-09-18)

All seven phases of `docs/MESH-PLAN.md` landed. What the decision now rests on,
separated by how well it is known:

**Measured on a three-site lab.** `MeshResolved=True` at every site; the
`SlapdCluster` byte-identical across sites with no per-site field in it; derived
decades reproducing the hand-wired scheme exactly (`olcServerID` 1 / 101 / 201,
so no CSN history was split); stanzas naming logical site names; cross-site
convergence; `slctl inspect --short` 17/17 per site; the full-gate suite at 88/93
with zero failures, equal to the hand-wired path it replaced. Single-site
re-verified separately: a one-site mesh at index 0 behaves as a standalone
cluster always has.

**Constructed rather than checked** — four of the nine invariants stopped
existing, because the operator derives `serverIDBase`, `externalPeers`, network
mode and trust wiring. The remaining five survive as properties of one artifact:
`charts/slapd-mesh` applied identically everywhere, with eight render-time guards
refusing what the operator would otherwise accept and regret.

**Reasoned, not measured.** The `multus` transport on the mesh path,
`imagePullSecrets` in the generated values, and `TEST_RESOURCES=lab`. The
accesslog-mount repair is unit-guarded and was measured live once on a
single-site lab.

**Lost, and recorded rather than quietly dropped.** Deleting the hand-wired e2e
path removed the only coverage of two transports the operator still supports:
cross-site NodePort `uri` peers, and static Multus `podAddresses`. Neither is
expressible through a `SlapdMesh` — the mesh names sites and lets the operator
discover addresses, which is the point — so the coverage cannot be recovered by
porting. See `docs/BACKLOG.md`.

**Not built.** `slctl mesh verify` and the continuous parity condition remain
out of scope (§6 describes them); the hand-audit prototype is still their
executable spec. cert-manager and External Secrets adoption (§7) likewise.

## Open questions

- Multi-site `bootstrapFrom` is argued create-everywhere from `entryUUID`
  preservation and never measured. Worth a deliberate test before anyone relies
  on it.
- Whether `SlapdMesh` should eventually validate the bundle at admission — the
  one property §5 knowingly gives up — or whether `verify` covers it adequately.
- Whether the operator's parity condition belongs on `SlapdCluster` or on each
  `SlapdDatabase`. The latter is more precise and noisier.
- Whether a withheld seed deserves its own status condition. Raised by Phase 2a:
  `SlapdDatabase` carries `Ready`, `TunablesConverged` and `DataPresent`, and
  none is a natural home for a seed-gate verdict. A misconfigured identity is
  currently visible as `Degraded` / `PartiallyApplied` plus a log line naming the
  fix, which is loud but not specific. A `SeedWithheld` condition would be
  clearer and is API surface, so it was deliberately not added unbriefed.

## Related

- ADR-004: multi-resource CRD architecture — the layering this extends upward
- ADR-007, ADR-016: cross-site transports — the trust this bootstraps, and the discovery credentials parity checking reuses
- ADR-008: uniform replication password; unreadable evidence never counts toward a pass
- ADR-014: slapadd-all-pods restore — why `bootstrapFrom` is create-everywhere
- ADR-019: a failed logbase search halts replication — why a missing database is a broken link
- ADR-025: single-creator seed, mesh-wide — becomes `seed.site`
- ADR-026: R2, no corrective action on unowned state — why parity checking is read-only
- ADR-027: the Secret is the source of truth — what makes externally-managed secrets viable

---

## Amendment, 2026-09-28: cross-site fan-out was missed, not rejected

`externalPeers[].replicasPerPeer` (ADR-007) controls how many remote pods each
local pod opens a syncrepl connection to. A mesh cluster could not reach it:
`externalPeersForSite` never set it, `SlapdMesh` had no field for it, and
`checkMeshSpecConflicts` refuses a hand-written `externalPeers` alongside
`meshRef` — so there was not even a workaround. Every mesh ran pinned at 1.

This ADR records what it rejected; fan-out appears nowhere in it, and that
silence is the evidence. Missed.

**What it cost.** Coverage was never affected — anything reaching one remote pod
reaches that site's others by its own in-cluster replication. What was missing is
cross-site link REDUNDANCY: at 1, each local pod has a single path to each peer,
and losing the remote pod at the far end costs that pod its cross-site path until
discovery repoints it — the 130-268 s window of ADR-016, which
`docs/BACKLOG.md` now records as the dominant term in this project's e2e
wall-clock. The knob that mitigates the most expensive measured behaviour in the
system was the one a mesh could not turn.

**Decision: `spec.replication.replicasPerPeer` on SlapdCluster**, not on
SlapdMesh, defaulting to 1 when unset and stamped onto every derived peer.

The first proposal put it on `SlapdMeshSpec`, next to `network`, whose doc
comment argues that the transport belongs to the mesh because "two clusters on
one mesh cannot sensibly disagree about whether pod CIDRs are routed". That
argument does not carry over: one mesh backs SEVERAL clusters (a cluster's
`sites` subset selector exists precisely so cluster X can span A+B while Y spans
A+B+C), and two clusters *can* sensibly disagree about fan-out — it follows from
their pod count and how much a rediscovery window costs them. A mesh-wide value
would force a small test tree and a critical five-pod directory to share one.

Putting it on the cluster also avoids inventing a mesh-only concept: the same
field is the default for hand-declared peers, with `externalPeers[].replicasPerPeer`
still winning where set. One field, one meaning, reachable from both directions.

It is an **input** to the derivation, not an output, which is why it is absent
from `checkMeshSpecConflicts`: §4's rule is that a *derived* field set alongside
`meshRef` is refused, and this is not one.

### Asked and answered: should `meshRef` move under `spec.replication`?

Raised at the same time, since `applyMeshWiring` writes only into
`spec.Replication` today — `ServerIDBase`, `ExternalPeers`, `Network`, and
nothing else. On current behaviour, nesting would be more honest.

Rejected, for three reasons in descending weight:

1. **The scope is intended to be broader than it is.** `SlapdMeshSpec.trust`
   already carries an `issuerRef`, because §7 commits to cert-manager and
   trust-manager for the CA and its distribution. When that lands, the mesh
   derives TLS material, which is `spec.ldap.tls` — outside replication.
   `meshRef` is replication-only today only because trust automation is
   deferred. Nesting now means un-nesting then.
2. **Layering symmetry.** The chain is `SlapdMesh ← meshRef ← SlapdCluster ←
   clusterRef ← SlapdDatabase`. `clusterRef` is top-level; nesting only the mesh
   half would make the two membership pointers asymmetric for no reason.
3. **It is a membership statement, not a tunable.** "This cluster belongs to that
   mesh" reads oddly as a sub-field of how replication is configured —
   especially when the consequence of setting it is that several *other* fields
   stop being the user's to set.

The real complaint underneath was discoverability: someone reading
`spec.replication` sees `externalPeers` with no hint that it is derived
elsewhere. Fixed directly instead — each of the three derived fields now carries
a pointer back to `meshRef` in its doc comment, which puts the explanation where
the person who needs it is already looking.


## Amendment (2026-10-05): `serverIDIndex`'s range is ADR-031's to set

This ADR bounded `serverIDIndex` at 40, from slapd's own arithmetic —
`olcServerID` caps at 4095, so index 40 is the last whole decade — and said
gaps are legal and expected, a decommissioned site's index being retired
rather than reused.

ADR-031 then made a peer's syncrepl RID slot `P + siteIndex`, with the remote
block `maxSites` wide. That makes the usable index range **the narrower of the
two**, and the narrower one is now the RID layout's: at the default
`maxSites: 12`, index 12 has no slot to live in.

**The decision is that ADR-031's layout governs, and this ADR yields.** It is
the later and more specific constraint, it was chosen deliberately with its
trade-offs costed, and discovering an older ceiling of 40 is not a reason to
re-open it. So:

- a site's `serverIDIndex` must be **strictly below** the mesh's
  `spec.ridLayout.maxSites`, and slapd's 40 remains a hard ceiling above that;
- because `ridLayout` is configurable, this is no longer a constant the CRD can
  hardcode. The bound is validated against the layout the mesh itself declares —
  a CEL rule over `sites[]`, so a site index that no RID could address is
  refused at admission rather than at the first reconcile;
- **gaps remain legal**, and reuse remains the hazardous option for the reason
  this ADR already gives: `contextCSN` keeps the highest CSN per serverID, so a
  new site inheriting a retired decade can have its writes read as
  already-seen. What changes is that the index space is now finite, so a mesh
  that retires indices is spending a budget. Exhausting it means widening
  `maxSites`, which costs one mesh-wide renumber.

Sites are static in practice — a mesh is built with its sites and then runs —
so this is a bound to state and validate, not a lifecycle to engineer around.
`docs/BACKLOG.md` is where any add/remove-site work belongs if it ever earns
its place.

## Amendment (2026-10-05): the serverID stride is 16, because a CSN is hex

The original decision gave each site a **decade** of 100: `serverIDBase =
serverIDIndex × 100`, so site 1's pods are 101, 102, 103. Everything above and
everything in `docs/MULTI-SITE.md` reasons in those numbers, and so did every
conversation about a live mesh.

Nobody ever sees those numbers. A serverID is read in exactly one place — the
`#sid#` field of a CSN — and slapd renders that field as **three hex digits**.
So a three-site mesh reports:

```
contextCSN (dc=second,dc=example,dc=net):
  20261005183045.802002Z#000000#001#000000
  20261005183049.075984Z#000000#065#000000
  20261005183050.304084Z#000000#0c9#000000
```

`065` is 101 and `0c9` is 201. The scheme was legible in a notation it never
appears in, and every reading of a contextCSN vector began with a base
conversion — at precisely the moment one is least inclined to do arithmetic.

**The stride is now 16 (0x10).** Site 1's base is 16 and its first pod is sid
17, which reads `011`: site digit, then pod digit, with nothing between the
number on screen and the pod it names. The decimal convention everyone already
spoke — "hundreds for the site, units for the pod" — becomes *true* rather than
aspirational, in the only base that matters.

### What is and is not a free choice

`olcServerID` requires exactly three things, and we checked: an integer in
0..4095 (the CSN field is three hex digits), non-zero and unique within one
replication topology, and **stable for the life of the data** — `contextCSN` is
a vector keyed by sid, so a server that changes its sid orphans its own history
under the old key and starts a new one at zero. slapd never parses the number,
never orders by it, never derives anything from it, and accepts unknown sids
seen on the wire (ADR-017).

Everything else is ours: `serverIDBase + ordinal + 1` (ADR-011/ADR-017), sid ≥ 1
from birth, and the per-site stride. The stride is therefore a free choice in
the only sense that matters — no server behaviour depends on it — and
constrained in the sense that matters more: **changing it for a site that
already exists is a migration, not an edit.**

### Why 4 bits for the pod and 8 for the site

12 bits split at a nibble boundary two ways. We took 8+4:

| stride | site indices | pods/site | CSN reads as |
|---|---|---|---|
| 100 (before) | 0..40 | 95 | `065` = site 1 pod 0 |
| 0x100 = 256 | 0..15 | 255 | `101` = site 1 pod 0 |
| **0x10 = 16** | **0..255** | **15** | **`011` = site 1 pod 0** |

A non-nibble split (6+6, 5+7) was rejected outright: it gives up the
legibility that is the entire motivation and leaves arithmetic *worse* than the
decimal scheme it replaces. A configurable split was considered and rejected as
over-engineering — restricted to nibble boundaries it is a boolean, not a knob,
and it would be a second layout axis beside `ridLayout`, able to disagree with
the first.

The asymmetry is deliberate and rests on how the two axes consume their space.
**Pod ordinals are reused** — a StatefulSet that scales down and back up takes
ordinal 2 again. **Site indices are retired, never reused** (this ADR's own
rule, so a new site cannot inherit a retired site's CSN lineage), so the site
axis monotonically burns address space and is the one that needs the room. 255
site indices is a retirement a year for two and a half centuries; 15 RW pods at
one site is ample where N-way multi-provider is already poor practice past
about five, and read-only replicas carry no serverID at all (ADR-017).

One property no stride can buy: the site digit is hex, so index 12 reads `c`.
Hand out indices 0–9 first if that matters.

### The new bound this creates

At stride 100 the ceiling "99 pods per site" was slack enough to leave unwritten.
At stride 16 it is real: a site's pod ordinal 15 takes the serverID that is the
**next** site's base, and two pods in two sites writing under one sid is exactly
the collision this whole scheme exists to prevent — undetected by slapd,
invisible on the wire, symptom "some writes never propagate".

The bound is enforced on `spec.ridLayout.podsPerSite` (capped at 15), **not** on
`spec.replicas`. The operator already refuses `replicas > podsPerSite`
(`PodsPerSiteExceeded`, ADR-031), so capping the declared layout keeps
`podsPerSite` the *single* bound on replicas. A second, independently-reasoned
replicas check is how a cluster ends up legal by one bound and illegal by the
other.

The two site-index ceilings stay separate and are checked in the same order as
before: slapd's (now index 255, from 4095 ÷ 16) first, because it is a limit of
the server that no field widens and deserves the message that says so; then
`ridLayout.maxSites`, which is configurable and gets a message naming a field a
human can change. In practice `maxSites` (default 12) binds first and slapd's
ceiling is slack — which is the correct shape, since the leaky axis is the wide
one.

### Migration

**There is none, and that is a statement about timing, not about safety.** A
live site cannot be re-striped: its pods' history stays filed under the old sid,
the old sid sits in every peer's `contextCSN` vector forever without advancing,
and per-site CSN tracking stops being comparable with itself (MESH-PLAN hazard
2). The change was taken while every deployment was a disposable lab, recreated
rather than migrated.

This retires one claim made above in §4 and in `serverIDBaseForSite`: that
indices 0/1/2 yield 0/100/200 — "the values every existing multi-site deployment
is already running, so adopting the mesh costs no migration." That was true when
written. It is now false for any cluster built before this amendment, and such a
cluster must either keep its old bases by setting `serverIDIndex` to leave them
unchanged (impossible under the new stride for any index but 0) or be rebuilt.
Anything else is a renumber of live pods, which the operator does not do and a
human should not do either.

A cluster outside a mesh is unaffected: `spec.replication.serverIDBase` is a
bare integer with no stride attached, and a hand-set 100 remains perfectly legal
— ADR-011's legacy two-site layout (101-104 / 201-204) included.
