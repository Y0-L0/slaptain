# ADR-032: Cross-site links are a property of a site pair, not of a pod

**Status:** Proposed
**Date:** 2026-10-05

## Context

Every pod opens `replicasPerPeer` syncrepl connections to every peer site, so a
site holds `P × (S−1) × replicasPerPeer` outbound cross-site links. The default
of 1 gives four links to each peer at four pods per site.

Nothing chose that number. It is the product of a **local HA decision** (how
many masters a site runs) and a **WAN decision** (how many independent paths
should exist between two sites), which are unrelated questions. Scaling a site
from 3 pods to 8 multiplies its cross-site link count by 2.7, its WAN
duplication of every change by the same factor, and the inbound syncrepl
sessions every peer must serve — buying no additional delivery guarantee,
because relay already delivers every change to every local pod.

That coupling is the same shape ADR-031 was written to remove from RIDs: two
independent quantities multiplied into one, where the limit on both is nearer
than anyone checked.

**Relay is already load-bearing**, which is what makes a reduced topology
possible at all. At `replicasPerPeer: 1` with the diagonal, site A's pod 0
consumes from site B's pod 0 and from no other pod at B. A write landing on B2
therefore reaches A0 only because B0 logged a change it had itself received
over syncrepl and re-served it. The three-site measurement in
`docs/UPGRADING.md` — every canary reaching all twelve pods in under 1.3 s — is
that mechanism working in production shape. We do not depend on a full
cross-site mesh today and have not for some time.

**ADR-031 depends on an invariant this ADR owns.** Its remote block is `S` wide
because a pod holds at most one link per peer site per database. That holds for
`replicasPerPeer: 1` and fails for 2, which ADR-031 therefore refuses. This ADR
is where the quantity is redefined so the refusal can be lifted.

## Options considered

### Option 1: Keep it per pod — rejected

The status quo. Uniform, no pod is special, no assignment rule.

Rejected because the quantity it exposes is not the one an operator wants to
set. "How many connections does each pod make" has no operational meaning;
"how many independent paths exist between these two sites" is the question
behind every answer anyone would give. The per-pod form also scales the WAN
cost with a number chosen for local redundancy.

### Option 2: Per site, carried by designated pods — rejected

`R` links per peer, held by pods `0..R−1`.

Rejected on asymmetry. It introduces the first special pod role in the system,
against the N-way symmetry CLAUDE.md's Phase 2 rationale establishes ("no pod
is special at runtime"), and it creates a failure state the per-pod scheme does
not have: with `P = 4, R = 2`, losing pods 0 and 1 leaves two healthy pods
serving clients and accepting writes while the site is silently cut off from
every peer. Repairing that means re-assigning the role on failure, which means
flap handling, which is a worse problem than the one being solved.

### Option 3: Per site pair, spread across pods — chosen

`R` links per peer site, placed on `R` distinct local pods by a pure function.
Keeps the quantity operational, keeps every pod symmetric in *kind* (all pods
carry some remote links; none is the relay), and makes redundancy explicit
instead of an accident of `P`.

## Decision

### 1. The quantity is `R`: independent links between this site and that peer

Per site pair, per database. Not per pod, not per connection. `R = 2` means
"survive the loss of one link", which is a sentence an operator can act on.

### 2. Placement is a pure function, and the holders are distinct pods

For peer at index `p` with `R` links and `P_eff` local pods:

```
holders(p) = { (p·R + m) mod P_eff  :  m in 0 .. R−1 }
pod i holds a link to peer p  ⟺  i ∈ holders(p)
remote pod for that link       =  i mod P_remote
```

- The `R` holders are **distinct local pods**, which is what makes `R` mean
  redundancy: one pod lost costs one link, never the peer.
- The remote end stays the diagonal, so distinct holders reach **distinct
  remote pods** and one remote pod lost also costs one link. This preserves the
  property the current builder already aims at — a pod's link to a peer site
  goes to that site's pod of its own ordinal.
- `p·R` staggers the starting pod per peer, so links spread evenly rather than
  piling onto low ordinals.
- No state, no election, no assignment to store. The function is `(i, p, R,
  P_eff, P_remote)` and nothing else.

### 3. `R ≤ min(P_eff, P_remote)`, refused otherwise

Above that bound the distinctness guarantees fail and `R` stops meaning
redundancy. A spec that asks for more is refused naming the binding side.

### 4. `R` defaults to `P_eff`, which is today's topology exactly

At `R = P_eff` every pod holds one link to every peer — the current
`replicasPerPeer: 1` behaviour, reproduced cell for cell. So this ADR can land
without changing any deployed topology, and lowering `R` afterwards is a
separate, observable step.

Lowering the default is deliberately **not** decided here. It wants measurement
this ADR does not have: convergence latency at `R = 2` against `R = P`, under a
write burst, on a mesh. `docs/BACKLOG.md` carries it.

### 5. `replicasPerPeer` is superseded

`spec.replication.replicasPerPeer` and `externalPeers[].replicasPerPeer` are
replaced by `R`. They were added 2026-09-28 for "a second route when the remote
pod at the far end goes away" (ADR-028 amendment) — a need met twice over here:
site-level `R` provides the second route without duplicating one pod's WAN
path, and ADR-031's site-keyed remote slot means the far end going away costs a
reselection rather than a renumber.

### 6. ADR-031's invariant holds by construction

A pod holds at most one link per peer site, because `holders(p)` names each pod
at most once. ADR-031's refusal of `replicasPerPeer >= 2` is lifted by this
ADR's arrival, not by an exception.

## Consequences

- **Links per site become `R × (S−1)`**, independent of `P`. At `S = 12,
  R = 2` that is 22, whether a site runs three pods or eight.
- **Relay stops being masked.** It is load-bearing today but shadowed by three
  other links; at low `R` a relay failure is visible instead of absorbed. An
  e2e must assert that a write reaches a pod holding **no** link to the
  originating site — coverage we do not have, because no topology we have run
  needed it.
- **One extra intra-site hop** of latency for pods that hold no link to the
  originating peer. Small against a WAN hop, and unmeasured.
- **Monitoring is unaffected.** `externalPeerStatuses` is computed by the
  operator binding the peer directly (`checkPeerCSNConvergence`), not by asking
  local pods what they can see, so peer health and lag detection do not depend
  on which pod carries a link. A site cut off from a peer still surfaces: the
  check compares local newest CSNs against the peer's and reports `Lagging`.
- **Fewer syncrepl sessions is fewer cookie states**, which is not nothing
  while ITS#9398 is open. It is a side effect, not a justification — the same
  reduction concentrates more traffic on each surviving session.
- **Nothing renumbers.** Under ADR-031 the coordinate is the provider, so
  adding or removing a link adds or removes one stanza and leaves every other
  RID alone. Under `ridBase` this change was not implementable at all: external
  RIDs were `ridBase + 50 + j + 1` with `j` positional in the peer list, so
  pods carrying different peer subsets would have numbered the same peer
  differently.

## Related

- ADR-003 — the operator owns syncrepl configuration.
- ADR-016 — pod-routed cross-cluster replication; what a cross-site link costs
  to repair.
- ADR-028 — the mesh layer; `replicasPerPeer` at cluster level exists because a
  mesh cluster cannot reach the per-peer field, a problem a site-pair quantity
  does not have.
- ADR-031 — RID coordinates; decision 9 is the invariant this ADR satisfies.
