# ADR-031: A RID is a coordinate — (database, provider) — not an encoded address

**Status:** Accepted
**Date:** 2026-10-04

## Context

ADR-003 gives each `SlapdDatabase` a `spec.replication.ridBase` and derives
every syncrepl stanza's RID from it: in-cluster peer `i` → `ridBase + i + 1`,
external connection `j` → `ridBase + 50 + j + 1`. The deployer picks the base by
hand, one per database, conventionally 100 apart.

The scheme encodes two coordinates — database and peer — into one integer by
slicing a 1000-value space into a 2-dimensional array. Because the space is
1000, any such slicing is square-root-shaped: roughly 30 by 30 if balanced, and
the 100-wide block we actually chose is 10 by 100. Both axes are reachable in
practice. The consequences were never written down:

- At most 10 databases per cluster. Nothing states this and nothing enforces it.
- At most 50 in-cluster replicas and 49 external connections per database.
  `spec.replicas: 60` silently walks in-cluster RIDs into the external half of
  the block; enough peers times `replicasPerPeer` walks the external half into
  the next database's block.
- `ridBase` carries `+kubebuilder:validation:Maximum=949`, which overflows for
  any database with an external peer: `949 + 50 + 0 + 1 = 1000`, and slapd
  rejects it with `syncrepl id 1000 is out of range [0..999]`. The value is
  only safe for a purely in-cluster database.
- Cross-CR uniqueness is "the deployer's responsibility" (already in
  `docs/BACKLOG.md`). ADR-003 claims the operator validates it; the operator
  does not.

The investigation started from an unrelated observation — a `contextCSN`
reading `#065#` where the operator had written serverID 101 — and that question
is a separate one, recorded in `docs/BACKLOG.md`. What it exposed along the way
is that slaptain applies near-identical decimal decade arithmetic to RIDs and to
serverIDs, two quantities that share no constraint, no scope, and no
representation. Two schemes that are *almost* the same are harder to hold in
your head than two that are plainly different.

## What slapd actually requires of a RID

Verified against the OpenLDAP 2.7.1 tree we build from (`images/openldap-deb/`),
2026-10-02. None of this is documented anywhere in the repo today, and the whole
decision rests on it.

The RID is the internal connection ID of that consumer's operations
(`syncrepl.c:2242`):

```c
op->o_connid = SLAPD_SYNC_RID2SYNCCONN( si->si_rid );
```

Real client connections are numbered above the reserved band
(`connection.c:53`):

```c
static unsigned long conn_nextid = SLAPD_SYNC_SYNCCONN_OFFSET;   /* 999 + 1 */
```

→ the 999 ceiling is not a convention; the RID space is carved out of the bottom
of the connid space (`slap.h:1874-1880`).

Hard requirements:

1. Range 0..999 (`SLAP_SYNC_RID_MAX`), decimal only — `parse_syncrepl_line`
   uses `lutil_atoi`, base 10, with no hex fallback, and the cookie renders
   `rid=%03d` (`ldapsync.c:77`).
2. Unique per *server*, not per database, because it is the connid. The scope
   is the whole pod, across every database's stanzas.
3. Supplied explicitly. `GOT_REQUIRED = (GOT_RID|GOT_PROVIDER|GOT_SEARCHBASE)`
   (`syncrepl.c:6674`), rejected at `:7185` if missing. Nothing in slapd
   allocates a RID.

What slapd does **not** require, which is what makes this ADR possible:

- No persistence. Consumer sync state lives in `si_cookieState`, allocated once
  per backend and shared by every stanza on it (`syncrepl.c:7779` hands later
  stanzas the first one's pointer; `:7789` allocates only for the first). The
  durable form is the database's own `contextCSN`, keyed by **SID**. Changing a
  RID therefore loses no sync position and forces no refresh.
- No agreement with the peer. My RID for you and your RID for me are unrelated
  integers.
- No semantics in the value. The three sites that test it —
  `be_shadow_update` (`backend.c:781`), and the `contextCSN`-update catchers in
  `syncprov.c:2573` and `accesslog.c:2069` — ask only `SLAPD_SYNC_IS_SYNCCONN`,
  i.e. "is this below 1000". Band membership is semantic; the number is not.
- The only RID-keyed lookup in the server (`syncrepl.c:610`) resolves a cookie
  passed on the command line with `-c`, which we never pass.

Two further findings bear on the design:

- slapd does not reject a duplicate RID. `add_syncrepl` has no such check: two
  stanzas with the same RID both load, share a connid, and become
  indistinguishable in the logs. A collision is silent.
- An `olcSyncrepl` value's only handle other than its content is positional —
  `X-ORDERED 'VALUES'` (`bconfig.c:769`), i.e. `{0}rid=101 provider=…`. That is
  the `olcDatabase={N}` shape ADR-026 R1 exists to warn about.

→ a RID is a pod-local label that must be stable and unique, and nothing more.

## Why the operator must own it anyway

Stability is not slapd's requirement; it is ours, from two places. The operator
needs a key to match the stanzas on a pod against the stanzas it wants, and
humans correlate `rid=NNN` across log lines during an incident (`si_ridtxt`
appears in nearly every syncrepl log line).

The key cannot be any other field. `searchbase` is identical across a database's
stanzas. `provider` is exactly what goes stale: a cross-site peer's pod IP
changes whenever that pod is replaced (ADR-016, measured 130-268 s to repair),
and repairing that stanza is the single most load-bearing reconcile we have.
Position is the ADR-026 R1 trap.

→ the RID is the primary key of our table of syncrepl stanzas. It has no value
in itself, but it must be stable, or we need a second index on top of it, which
buys nothing and costs more.

## Options considered

### Option 1: Keep `ridBase` blocks — rejected

The status quo. Preserves the encoding, so a reader can tell from `rid=251` that
it is the mgmt database's second external peer. Keeps every ceiling above,
keeps the `Maximum=949` overflow, and keeps cross-CR uniqueness as unenforced
deployer paperwork.

### Option 2: Dense packing derived from a canonical order — rejected

Assign RIDs by position in a canonically sorted list of all desired stanzas.
Stateless, uses the sum of stanzas rather than a product, no ceilings.

Rejected on ownership, not aesthetics. Stanzas for database A live on
`olcDatabase={N}` owned by CR A; B's live elsewhere. Inserting a database shifts
its successors' RIDs, so reconciling B would rewrite A's stanzas — state B's
controller does not own (ADR-026 R2). No controller is permitted to perform that
write.

The same objection kills the two neighbouring ideas. Hashing the slot and
probing on collision displaces a sibling's assignment, and with ~50 stanzas in
1000 slots at least one collision is ~71% likely. Ranking databases by
`creationTimestamp` is append-only and therefore stable until the first delete,
after which the choice is between shifting everyone (same violation) and leaking
the block forever.

→ stability under an append/delete workload requires recorded assignment. There
is no derivation that escapes it.

### Option 3: Each `SlapdDatabase` allocates its own RIDs — rejected

First-fit over 0..999, each controller reading its siblings' recorded
assignments and yielding on overlap by `(creationTimestamp, uid)`. Reading a
sibling's status is not an ADR-026 R2 violation, and the scheme converges.

Rejected because it buys concurrency we do not need and pays for it in
machinery: a conflict rule, a yield, a requeue, and a window in which two
databases hold the same RID. Allocation is rare. We would rather have one
authority dictate the numbers than have several controllers argue about them.

### Option 4: A `<cluster>-rid-registry` ConfigMap — rejected, narrowly

Genuinely more durable than status, precedented here (`<name>-database-dirs` is
an operator-owned ConfigMap holding derived data, ADR-013 amendment), and a
human can read and repair it.

Rejected because the durability only pays if losing the ledger were both likely
and unrecoverable, and it is neither (see Consequences). It costs a second
object with its own lifecycle, and the consuming controller would need a new
read and new RBAC where it already resolves `clusterRef` and holds the cluster
CR.

### Option 5: Rebuild the ledger by reading RIDs back from the pods — rejected

Attractive as a recovery path: the authoritative RIDs are in `cn=config` on the
pods, so a lost ledger could be re-imported rather than reassigned.

Rejected on precedent. `verifySeedExists` was exactly this pattern — read live
state to decide whether to act — and was reverted (ADR-012); ADR-025's withhold
belt was then deliberately built in the opposite direction; ADR-008's amendment
states the general rule as "unreadable evidence never counts toward True". A
rebuild path cannot distinguish *absent* from *unreachable*, and it reintroduces
double ownership on the allocator, which is the worst place for it. The
reconstruction is also imperfect where it matters: an in-cluster stanza's
ordinal reads off its stable pod DNS, but an external pod-routed stanza whose
provider IP has changed cannot be mapped back to its slot — the same
instability that made the RID necessary as a key.

→ we accept a renumber in the degenerate case in exchange for one owner and no
live-state dependency in the regular case.


### Option 6: A ledger keyed by replication slot — implemented, then withdrawn

The first implementation of this ADR. Every replication *slot* a cluster needs
— `<db>/in-cluster/<ordinal>` and `<db>/external/<peer>/<k>` — was derived from
the resolved spec each pass, allocated a number from the lowest free value above
a monotonic high-water mark, and recorded in `SlapdCluster.status`. A slot kept
its number forever; a slot that left the desired set was garbage collected.

It worked. It was mesh-validated on three sites: ~36 s to converge, no pod
restarts, no stanza set ever short, writes never interrupted. It is withdrawn
anyway, for one structural reason and one consequence of it.

**The slot key depended on the resolved topology.** Under `meshRef` a cluster's
stored CR carries no `externalPeers` at all — `applyMeshWiring` derives them
into the in-memory spec (ADR-028). So "this cluster has no external peers" is
both a legitimate state and the symptom of having asked before resolution, and
the derivation cannot tell them apart. Ask too early and the ledger holds
in-cluster slots only; the consumer derives the same short set, finds every one
of them assigned, and converges — which, because `olcSyncRepl` is written with a
single atomic `Replace`, *deletes* every cross-site stanza. Nothing errors.
`RIDLedgerHealthy` reads `True`, the database reads `Running`.

Ordering fixes it in the allocating controller. It does not fix it in the
consuming one, which re-derives the same slots from a cluster object it fetched
itself, and so must establish independently whether what it is judging was
resolved. The naive test — `meshRef` set and no peers ⟹ unresolved — withholds a
**single-site mesh forever**, since `mesh_derive` skips self. What shipped was a
disagreement detector: the ledger carries `<db>/external/*` slots while this
pass derived none ⟹ unresolved. Sound, and a runtime heuristic standing in for
an invariant that should have been structural.

**The consequence** is that a quantity which has no business being dynamic was
computed from the most dynamic input available. Around that choice accumulated:
a slot-key format that two call sites had to spell identically, an injectivity
check, a fan-out resolution that the allocator and the stanza builder had to
agree on exactly, a five-reason gate, and ~1700 lines of operator code against
the ~15 lines of arithmetic it replaced.

→ the failure was not the ledger. It was keying the ledger on something derived.
A coordinate that comes from *declared identity* has nothing to be stale about.

## Decision

### 1. A RID is a coordinate in a static matrix

```
rid = 1 + column·(S·P) + siteIndex·P + ordinal
```

| term | meaning | source |
|---|---|---|
| `column` | which database | **allocated** — the only allocated quantity in the scheme |
| `siteIndex` | which site the *provider* is at | declared: the `SlapdMesh` site's `serverIDIndex`, or `externalPeers[].siteIndex` off-mesh |
| `ordinal` | which pod at that site is the provider | structural: the provider pod's StatefulSet ordinal |
| `S`, `P` | max sites, max pods per site | compile-time constants (see 5) |

`rid = 0` is never allocated, so a zero is always a bug and never a coordinate.

The coordinate names a **provider**: the database being replicated, and the
single remote server it is replicated from. It does not name the pod writing the
stanza, the connection index within a fan-out, or an address.

### 2. Everything dynamic stays out of it

A provider's *address* changes — a pod is replaced, an IP is reassigned, a
remote fleet scales. Its *identity* does not. Discovery's entire job is
answering "what address is provider (site, ordinal) on right now"; it has no
part in deciding the provider's number.

This is the whole difference from Option 6, and three properties follow:

- **`rid=317` names the same (database, provider) at every site in the mesh.**
  Every term but `column` is globally declared, and `column` is per cluster but
  derived from a database name that ADR-028 requires to be identical at every
  site. Correlating a RID across sites during an incident needs no decoder.
- **Remote churn stops rewriting `cn=config`.** Today `discoverRemotePeerAddresses`
  drops non-`Running` pods and sorts the survivors by IP string, so an unrelated
  remote pod blipping permutes the list and every local pod rewrites its whole
  stanza set. Under a provider coordinate, the stanza that names that provider
  gets a new `provider=` and nothing else moves.
- **Fan-out is not a dimension.** `replicasPerPeer: 1 → 3` adds stanzas whose
  RIDs are already determined. Nothing renumbers, and the allocator and the
  stanza builder no longer have to agree on a connection count.

### 3. The ledger holds one map

`SlapdCluster.status.ridColumns: map[string]int32` — database name → column.
At most `C` entries (41 under the chosen constants), written only by the
SlapdCluster controller, read by the SlapdDatabase controller.

Allocation is lowest-free, not high-water: with ~41 columns, never reusing one
would exhaust the space after 41 database creations in a cluster's lifetime. A
column is released only when the `SlapdDatabase` is gone — ADR-005's finalizer
already guarantees its stanzas are torn off every pod before the CR is released,
so a reused column cannot collide with a stanza that outlived its database.

Nothing else is recorded. `siteIndex` and `ordinal` are read from the spec and
from pod names on every pass, because they are facts about declared identity
rather than decisions.

### 4. Off-mesh peers declare their site index

Under `meshRef` the row is the peer site's `serverIDIndex`, which ADR-028
already requires to be declared and unique. A hand-written `externalPeers[]`
entry has no such declaration, so it gains `siteIndex`, required when the
cluster replicates and it is not under `meshRef`.

This is hand-assigned data, which `ridBase` also was — and the difference is the
scope. `ridBase` had to be unique across *CRs*, which no admission rule can
check, which is why it stayed unenforced deployer paperwork. `siteIndex` must be
unique within *one CR's peer list*, which is a CEL rule on the list itself. The
API server refuses a duplicate.

The local site's own index is `serverIDBase / 100` (ADR-017's decade), which is
0 for a cluster that is not in a mesh.

### 5. The envelope is a declared constant, and exceeding it is a refusal

`S` and `P` are compile-time constants, not spec fields. A spec field would let
two sites in one mesh disagree, and global legibility (decision 2) depends on
every site computing the same number.

| `P` pods/site | `S` sites | `S·P` | databases |
|---|---|---|---|
| 3 | 5 | 15 | 66 |
| **4** | **6** | **24** | **41** |
| 4 | 8 | 32 | 31 |
| 5 | 8 | 40 | 25 |

**Chosen: `P = 4`, `S = 6`, `C = 41`.** Four pods per site is one scale-up above
the three-per-site N+2 the sizing guidance assumes; six sites is one above the
five that the (N−1)/N single-site-loss arithmetic lands on. Databases are the
axis least likely to grow — the lab runs two — so the headroom goes to topology.

Exceeding any axis is a refusal naming which one and what the ceiling is. It is
never a wrap, never a silent collision. Raising a constant in a later release
renumbers the mesh, which costs exactly what this ADR's upgrade costs (one
`cn=config` write per pod per database, no restart, no resync) and gets an
`UPGRADING.md` entry.

### 6. The ledger gates stanza convergence, and a withhold leaves stanzas alone

A `SlapdDatabase` whose column is not yet in the ledger writes no stanzas and
reports `Degraded/RIDColumnUnassigned`. It does **not** converge toward an empty
set: `olcSyncRepl` is written with one atomic `Replace`, so a short set is a
deletion. The same rule covers a coordinate outside the envelope — refuse the
whole pod's stanza set, report, requeue.

The gate is one map lookup against one key. There is no derived slot set to
disagree about, and therefore no resolution-ordering question on the consumer
side.

### 7. A stale read of the ledger is guarded, not tolerated

Status is normally last-writer-wins here because every field is an observation
recomputed each pass. The column map is not: it is computed from its own
previous value, and an empty read of a populated map does not look stale — it
looks like a fresh cluster, and reallocating from it renumbers everything. So
the ledger write carries the `resourceVersion` it was derived from; a 409
requeues, by which point the informer has caught up.

### 8. `spec.replication.ridBase` is removed

With no deployer-assigned number left, the field, its `Maximum=949` overflow,
its CEL requirement, and the two Helm chart uniqueness checks all go.

## Consequences

- **The upgrade renumbers every stanza once** — one `cn=config` write per pod
  per database. No restart, no resync, no data movement: slapd keys durable sync
  state on SID, never on RID. See `docs/UPGRADING.md`.
- **A lost ledger costs a renumber, not an outage.** The column map can be
  cleared by hand; the operator reallocates and rewrites every stanza. The
  provider coordinates are unaffected, so only the database axis moves.
- **`slctl` can decode a RID**, at any site, with no cluster state: `317` →
  column 13, site 1, ordinal 0. A decode subcommand becomes worth having.
- **Hazard 6 stops existing** for the RID path. The address-resolution hazard it
  rode on does not: a peer with no resolved addresses still yields a short
  stanza set, which `slapddatabase_controller.go:135` already handles by
  stalling, and which `docs/BACKLOG.md` tracks for three other paths.
- **Discovery must return ordinals.** `discoverRemotePeerAddresses` returns bare
  IPs today. It returns `(ordinal, address)` pairs after this, with the ordinal
  parsed from the StatefulSet pod name and a **refusal** — not a positional
  fallback — if that parse ever fails.
- **`podAddresses` mode gains a documented meaning for list position**: index =
  provider ordinal. Reordering the list renumbers.
- **`uri` mode** is one opaque provider: its own `siteIndex`, ordinal 0.
- **Read replicas stay free.** They consume from the same providers, so their
  stanzas carry the same RIDs. An RO pod is never a provider, so its own ordinal
  never enters a coordinate.
- **A pod's own coordinate is unused on itself.** Every pod skips its own
  in-cluster stanza; that number is simply not written there. The matrix stays
  uniform rather than dense, which is the trade decision 1 makes deliberately.

## Scope

In: the RID scheme, the column ledger, the gate, discovery returning ordinals,
`ridBase` removal, the upgrade note.

Out: serverID allocation (ADR-017, unchanged and deliberately not unified — see
Context on two almost-identical schemes being worse than two different ones);
the three unguarded `Replace` paths in `docs/BACKLOG.md`; heartbeats; anything
ITS#9398.

## Related

- ADR-003 — operator owns syncrepl configuration; its per-database `ridBase`
  rule is superseded here.
- ADR-005 — the finalizer that makes column reuse safe.
- ADR-016 — pod replacement invalidates peer addresses; the reason a RID cannot
  be keyed on one.
- ADR-017 — `olcServerID` is a bare integer; `serverIDBase` supplies the local
  site index.
- ADR-026 — R1 positional `olcDatabase={N}`; R2 not writing state you do not
  own, which rejected Options 2 and 3.
- ADR-028 — the mesh layer; `serverIDIndex` is the site coordinate this ADR
  reuses.
