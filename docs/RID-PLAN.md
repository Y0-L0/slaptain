# ADR-031 implementation plan — RID coordinates

Breakdown of [ADR-031](adrs/adr-031-rid-coordinates.md) into reviewed
milestones. The ADR is authoritative on semantics; where this plan and the ADR
disagree, the ADR wins and this file is wrong.

Branch: `feat/rid-matrix`, off `main`. The withdrawn first implementation lives
on `feat/rid-allocation-ledger` and is **reference material, not a base** — see
"Salvage" per milestone for what is lifted from it.

---

## Seven hazards to read before writing any code

1. **One coordinate function, not two.**

   ```
   rid = 1 + column·M + offset          M = P + S
   offset <  P   →  a provider at my own site, pod ordinal `offset`
   offset >= P   →  my link to peer site `offset − P`
   ```

   | term | is | from |
   |---|---|---|
   | `column` | which database (0..C-1) | the ledger — the one allocated term |
   | local block | `P` slots, indexed by the provider's pod ordinal | structural |
   | remote block | `S` slots, indexed by the peer's `siteIndex` | `serverIDIndex`, or `externalPeers[].siteIndex` off-mesh |
   | `P`, `S`, `C` | 8 pods/site, 12 sites, 49 databases | `SlapdMesh.spec.ridLayout`, defaulted |

   `P + S`, not `P · S`: a pod holds at most one link per peer site
   (ADR-031 decision 9), so the remote axis is sites and not site×pod. It is
   written once, in `internal/ridcoord`, and every caller2. **Withhold means leave alone, not delete.** `olcSyncRepl` is installed with
   one atomic `modReq.Replace`, so a set shorter than the pod's current one is a
   deletion. Every refusal path — column unassigned, coordinate out of envelope,
   an unresolvable provider — must skip that pod's convergence entirely and
   leave its stanzas untouched. ADR-027's identity gate has the right shape.

3. **A column may only be released when the database is gone.** Reusing a
   column whose predecessor's stanzas still sit on a pod hands the new database
   the old one's RIDs. ADR-005's finalizer guarantees teardown precedes release,
   so the rule is: free a column when the `SlapdDatabase` is absent from the
   List, never on `deletionTimestamp`, never on `suspend`, never on
   `replication.enabled: false`.

4. **A stale read of the column map renumbers the cluster.** An empty read of a
   populated map reads as "fresh cluster", and allocating from it reassigns
   every column. Unlike every other status field here, the map is derived from
   its own previous value, so the write carries the `resourceVersion` it was
   derived from and a 409 requeues.

5. **A failed List is not an empty database set.** A transient list error must
   requeue with the ledger untouched. Treating it as "no databases" frees every
   column of the databases it failed to see — hazard 3 at fleet scale.

6. **Parse the ordinal, never infer it from list position.** Discovery today
   drops non-`Running` pods and sorts the survivors by IP string, so position
   carries no identity at all. The ordinal comes from the StatefulSet pod name;
   a name that will not parse is a **refusal naming the pod**, never a
   positional fallback. This is the hazard that makes hazard 1 reachable.

7. **The local site's own index must not collide with a declared peer's.**
   Under `meshRef` both come from `serverIDIndex` and ADR-028 enforces
   uniqueness. Off-mesh, self is `serverIDBase / 100` while peers declare
   `siteIndex` directly, so a hand-set `serverIDBase` that is not a multiple of
   100 — or simply equal to a peer's decade — silently puts two providers in one
   row. Refuse at the point where both are known.

---

## Milestones

Each milestone is reviewed and committed before its dependents launch.
Ownership is stated so concurrent agents never share a file.

### M1 — Providers carry ordinals

**Goal.** Discovery returns provider *identity*, not bare addresses.
`discoverRemotePeerAddresses` → `(ordinal, address)` pairs, ordinal parsed from
the StatefulSet pod name, refusal on a name that will not parse (hazard 6).
`ExternalPeerStatus.discoveredAddresses` carries the ordinal.

**Owns.** `operator/api/v1alpha1/slapdcluster_types.go` (the status type),
`operator/internal/controller/slapdcluster_controller.go` (discovery and its
in-file consumers), the single read at `slapddatabase_controller.go:2505`,
`operator/cmd/slctl/cmd/status.go` and `inspect.go` (they render the old
field), the generated deepcopy and CRDs, and the tests for those.

**Also M1's, so it does not fall between milestones:** `uri` and
`podAddresses` peers never go through discovery and so produce no provider of
their own. The consumer builds theirs — list index is the ordinal for
`podAddresses`, a `uri` peer is one provider at ordinal 0 — so that every mode
hands the same type downstream.

**Red-first.** The parse is a pure function: name → ordinal, with a refusal
case. Table test before implementation.

**Settles an open question** (see below): whether provider selection runs over
live addresses or declared ordinals.

**Roles.** test-author → implementer → reviewer (correctness).

**Done when** discovery yields ordinals, a malformed pod name refuses that peer
with the pod named, and nothing else changed behaviour.

### M2 — The coordinate (pure seam) — runs in parallel with M1

**Goal.** `internal/ridcoord`: the formula, the layout type (`P`, `S`, derived
`M` and `C`) with its defaults 8/12, `Local(column, ordinal)` and
`Remote(column, siteIndex)` refusing each axis separately, and `Decode(rid)`
as their inverse — returning which block the offset fell in.

**Owns.** `operator/internal/ridcoord/**` — a new package, no Kubernetes
imports, no I/O.

**Red-first.** Everything here is pure. The properties to pin by name: round
trip for every legal coordinate; `rid = 0` never produced; each axis refuses at
its ceiling and the message names which axis; the maximum legal coordinate is
≤ 999 (980 at the default layout); `C = floor(999/M)` holds for every layout
the bounds admit; a local and a remote coordinate never collide.

**Roles.** test-author → implementer → reviewer (conformance against ADR
decision 1 and 5).

**Done when** the package is green and the envelope arithmetic is proven by
test rather than by comment.

### M3 — The column ledger

**Goal.** `SlapdCluster.status.ridColumns`, allocated lowest-free by the
SlapdCluster controller, released only on database absence (hazard 3), guarded
on `resourceVersion` (hazard 4), requeue-on-list-failure (hazard 5),
`RIDLedgerHealthy` condition with reasons that name the human action.

**Owns.** `operator/api/v1alpha1/slapdcluster_types.go` (status field — after
M1 has landed), `operator/internal/controller/slapdcluster_ridledger.go` (new),
the one call site in `slapdcluster_controller.go`, the `applyStatusAt` helper,
and their tests.

**Depends on** M2 (envelope refusal), M1 (file ownership only).

**Salvage.** `applyStatusAt` and its doc comment lift almost verbatim from the
withdrawn branch; move it next to `applyStatus` rather than leaving it in
`slapdcluster_restore.go`. The condition-reason shape lifts too; the reason set
shrinks.

**Roles.** test-author → implementer → **two reviewers** (conformance and
correctness). This milestone carries hazards 3, 4 and 5 — the silent ones.

**Done when** a column survives unrelated reconciles, a deleted database frees
its column, a list error changes nothing, and a stale-read write 409s.

### M4 — Consumption, the gate, and off-mesh site indices

**Goal.** Stanzas take their RID from `ridcoord` keyed on the provider;
`externalPeers[].siteIndex` added with a CEL uniqueness rule over the peer list;
self index derived from `serverIDBase / 100` with the collision refusal
(hazard 7); the gate withholds a database whose column is unassigned and leaves
its stanzas alone (hazard 2).

**Owns.** `operator/api/v1alpha1/slapdcluster_types.go` (the peer field),
`operator/internal/controller/slapddatabase_controller.go`,
`operator/internal/controller/slapddatabase_ridgate.go` (new), their tests, and
the regenerated CRDs.

**Depends on** M1, M2, M3.

**Roles.** test-author → implementer → reviewer (correctness, with hazard 2 as
the named target: prove that every refusal path leaves `olcSyncRepl` untouched).

**Done when** every stanza's RID is a coordinate, a missing column withholds
without writing, and the gate has exactly one reason to withhold on.

### M5 — First live integration (gate — nothing builds on unit-green code)

**Goal.** Deploy M1–M4 to the three-site lab and debug to green. Unit-green is
not works: the first real bringup is where an implicit precondition or a
mode-flag that means something different in a mesh shows up.

**Owns.** Nothing by default. Product fixes found here land as reviewed commits
through the owning milestone's agent.

**Roles.** tester, with the implementer resumed on demand. Image-tag carve-out
applies — the loop is build → push → deploy, so the tag comes from a commit;
focused WIP commits, reviewed as a series, never pushed to a remote.

**Done when** a three-site mesh converges, `slctl inspect` is clean, and every
RID on every pod decodes to the provider its stanza names.

### M6 — Remove `ridBase`, and the tooling that reads RIDs

**Goal.** Delete the field, its CEL rule, its `Maximum=949`, and both Helm
uniqueness checks. `slctl`: `rid-uniqueness` stays, `rid-ledger-match` becomes
ledger-and-coordinate agreement, and a `decode` surface appears — it costs
nothing now that a RID is self-describing. **`decode` must not simply surface
`ridcoord`'s refusal**: a RID in 981..999 is inside slapd's band and outside
the matrix, and a human pasting one off a pre-upgrade stanza needs that
sentence, not an error (found by the M2 test author, 2026-10-05). `docs/UPGRADING.md` gets the entry.

**Owns.** `operator/api/v1alpha1/slapddatabase_types.go`, `charts/**`,
`tests/resources/**`, `operator/cmd/slctl/**`, `docs/UPGRADING.md`,
`docs/TUNING.md` and `CLAUDE.md` where they name `ridBase`.

**Salvage.** The UPGRADING entry survives ~90% — one renumber, no restarts, the
operator-down CRD window, the untested rollback. Its measured numbers were
measured on a renumber and stay true. The three `docs/BACKLOG.md` findings lift
as a patch.

**Roles.** implementer → reviewer (conformance). Mechanical.

### M7 — e2e

**Goal.** Specs that would fail if the scheme regressed: a RID decodes to the
provider its stanza names, on every pod and every database; a database added
mid-run gets a column without renumbering its siblings; a remote pod replaced
keeps its RID and changes only `provider=`.

**Owns.** `tests/e2e/**`, `tests/e2e.sh`.

**Roles.** tester. Full mesh run with skip counts reported per gate.

---

## Dependency order

```
M1 ─┬─► M3 ─► M4 ─► M5 ─► M6 ─► M7
M2 ─┘
```

M1 and M2 run concurrently on disjoint paths. Everything after M4 is serial,
because M5 is a gate rather than a step.

---

## Open questions to settle in flight

**Settled, no longer open: what a remote provider's RID is keyed on.** The
remote slot is keyed by **peer site**, not by remote pod, so reselecting which
pod at that site answers — it was replaced, it was never `Running`, discovery
saw it late — changes `provider=` and moves no RID. The earlier draft of this
plan carried this as an open question about address availability; ADR-031
decision 1 removes the question rather than answering it.

What remains for M1 is unchanged and smaller: discovery must return
`(ordinal, address)` so the diagonal `(localOrdinal + k) % remoteN` indexes
identity instead of IP-sort position, and a pod name that will not parse is a
refusal naming the pod.

**Whether `slctl decode` needs the cluster at all.** Everything but `column` is
globally declared; the column needs the ledger. A decode that prints
`column 13` without resolving the name is still useful offline, and resolving it
when a cluster is reachable is strictly better. M6 decides the surface.

---

## What this plan does not cover

- serverID allocation (ADR-017). Deliberately left alone; unifying two
  almost-identical schemes is what ADR-031's Context argues against.
- The three unguarded `Replace` paths in `docs/BACKLOG.md`. Same hazard class as
  hazard 2, pre-existing, and out of scope here.
- Rollback to `ridBase`. Untested by decision; `docs/UPGRADING.md` says so
  plainly and names the manual path.
