# ADR-031 implementation plan — RID coordinates

Breakdown of [ADR-031](adrs/adr-031-rid-coordinates.md) into reviewed
milestones. The ADR is authoritative on semantics; where this plan and the ADR
disagree, the ADR wins and this file is wrong.

Branch: `feat/rid-matrix`, off `main`. The withdrawn first implementation lives
on `feat/rid-allocation-ledger` and is **reference material, not a base** — see
"Salvage" per milestone for what is lifted from it.

---

## Five hazards to read before writing any code

The list was seven. Three of them — release gated on database absence, the
stale-read renumber, and a failed List reading as an empty database set — were
properties of allocating the segment into status, and went away with it
(ADR-031 Option 7). What is left is what the scheme needs regardless.

1. **One coordinate function, not two.**

   ```
   rid = 1 + segment·M + offset          M = P + S
   offset <  P   →  a provider at my own site, pod ordinal `offset`
   offset >= P   →  my link to peer site `offset − P`
   ```

   | term | is | from |
   |---|---|---|
   | `segment` | which database | `spec.replication.ridSegment`, declared |
   | local block | `P` slots, indexed by the provider's pod ordinal | structural |
   | remote block | `S` slots, indexed by the peer's `siteIndex` | `serverIDIndex`, or `externalPeers[].siteIndex` off-mesh |
   | `P`, `S`, `C` | 8 pods/site, 12 sites, 49 segments | `SlapdMesh.spec.ridLayout`, defaulted |

   Written once, in `internal/ridcoord`, and every caller goes through it —
   the stanza builder, the validator, `slctl decode`. Two spellings do not fail
   to compile; they produce a stanza carrying another provider's number, which
   slapd accepts in silence.

2. **Withhold means leave alone, not delete.** `olcSyncRepl` is installed with
   one atomic `modReq.Replace`, so a set shorter than the pod's current one is
   a deletion. Every refusal — a colliding segment, a segment outside the
   layout, an unresolvable provider — must skip that pod's convergence entirely
   and leave its stanzas untouched. ADR-027's identity gate has the shape.

3. **An unprovable uniqueness is not a proven one.** The collision check reads
   the cluster's other `SlapdDatabase`s. If that read fails, uniqueness is
   unknown — and unknown must withhold, not converge. Treating a failed List as
   "no siblings, therefore no collision" is how two databases end up writing
   stanzas that share RIDs, which slapd accepts in silence.

4. **Parse the ordinal, never infer it from list position.** Discovery drops
   non-`Running` pods and (before M1) sorted by IP string, so position carries
   no identity. The ordinal comes from the StatefulSet pod name; a name that
   will not parse is a refusal naming the pod. Landed in M1.

5. **Two peers must not declare one `siteIndex`.** A peer's RID slot is
   `P + siteIndex`, so two peers sharing an index share a slot — one RID for
   two providers, which is the shared-connid state slapd never rejects. Under
   `meshRef` ADR-028's `serverIDIndex` uniqueness covers it; off-mesh a CEL
   rule over the peer list does, and the operator refuses again for a CR
   applied by hand.

   This hazard previously read "the local site's own index must not collide
   with a declared peer's". That was wrong and is corrected here: the local
   block is indexed by pod ordinal and the remote block by peer site, so the
   two never overlap and the local site needs no index at all (ADR-031
   decision 4).

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

### M3 — `spec.replication.ridSegment`, and the collision refusal

**Goal.** The declared field, its bounds and CEL, the chart validation that
follows it, and the pure detector that refuses two databases sharing a segment
or a segment outside the layout.

**Owns.** `operator/api/v1alpha1/slapddatabase_types.go` (the field, replacing
`ridBase`), `operator/api/v1alpha1/slapdmesh_types.go` (`ridLayout`),
`operator/internal/controller/ridsegment.go` (new — the detector and the layout
lookup), `charts/slapd/templates/_helpers.tpl` and
`charts/slapd-mesh/templates/_helpers.tpl` (the rule survives, renamed), the
generated CRDs, and their tests.

**Where the check lives: the SlapdDatabase controller, not the cluster.** It is
the controller that writes stanzas, so it is the one that must refuse to. A
database lists its siblings, finds any that declare its own segment, and
withholds its own convergence — symmetric, so both sides of a collision stall
and neither wins. No cross-controller state, no condition to read, no ordering.
A cluster-level summary condition is worth having later and is in
`docs/BACKLOG.md`, not here.

**Red-first.** The detector is pure: given this database, its siblings, and the
layout, return a verdict. Cases: a clean set; two databases colliding (named,
both); a segment at `C` and above; a negative; a database of another cluster
ignored; `replication.enabled: false` siblings still counted, because their
stanzas may exist from before the flag flipped.

**Salvage.** `internal/ridcoord` is unchanged and already green. The three
reason constants and the withhold shape lift from the withdrawn M3; the
allocator, the status field, `applyStatusAt` and the two envtest specs do not.

**Roles.** test-author → implementer → reviewer (correctness). One reviewer,
not two: the hazards that justified a second axis went with the ledger.

**Done when** a colliding pair both report `Degraded` naming each other and
neither writes a stanza, a clean set converges, and `ridBase` is gone from the
API, the charts and the fixtures.

### M4a — Stanzas take their RIDs from `ridcoord`

**Goal.** The consumption seam. Every syncrepl stanza's RID comes from
`ridcoord`, keyed on the database's declared segment and the provider's
identity: an in-cluster peer at ordinal `k` is `Local(segment, k)`, a link to
peer site `s` is `Remote(segment, s)`. The `legacyRIDBase` bridge is deleted,
which lifts the temporary 0..9 ceiling back to `0..C-1`.

**Owns.** `operator/internal/controller/slapddatabase_controller.go` (the
stanza builders and their callers), `operator/internal/controller/ridsegment.go`
(the bridge's grave, and `ridLayoutFor`), `externalPeersForSite` in
`operator/internal/controller/mesh_derive.go` — the derived peers must carry
their site's `serverIDIndex` as `siteIndex`, or a mesh cluster's cross-site
stanzas have no remote coordinate at all —
`operator/api/v1alpha1/slapdcluster_types.go` (`externalPeers[].siteIndex` and
its CEL rule), the generated CRDs, and their tests.

**Carries two of the three release gates:**

- **`ridLayoutFor` must actually read `SlapdMesh.spec.ridLayout`.** It ships
  unread in M3 — the judgement hardcodes `DefaultLayout()` — so a declared
  layout is silently ignored, which ADR-024 forbids. Under `meshRef` read the
  mesh; otherwise return the defaults and do no I/O. **An unreadable mesh
  means the layout is unknown, and unknown withholds rather than converging**
  (hazard 3's shape).
- **Delete `legacyRIDBase`**, and with it the 0..9 ceiling and its field-doc
  paragraph.

**Also M4a's:** lift the phase selection out of `Reconcile` into a pure
`databasePhase(...)`. The precedence it encodes — a RID refusal outranks
`PartiallyApplied` but never `NoPodsReachable` — is currently dead in every
test, because the envtest fixtures have no pods and the phase is already
`Error`.

**And refuse `replicasPerPeer >= 2`** (ADR-031 decision 9): the remote block is
`S` wide because a pod holds at most one link per peer site. ADR-032 replaces
the quantity with a site-pair `R`; until then, two links from one pod to one
peer site have no second slot to live in.

**Red-first.** The RID derivation is pure: given a segment, a layout, a local
ordinal and a resolved peer set, return the stanza RIDs. The bridge-equivalence
test from M3 is the thing that must now *change* — it pins the old formula, and
M4a is what replaces it. Expect it red, and rewrite it to assert the coordinate
rather than deleting it.

**Roles.** test-author → implementer → reviewer (correctness).

### M4b — The three short-set paths (the last release gate)

**Goal.** `docs/BACKLOG.md`'s "Three paths hand `Replace` a short set with no
error". `olcSyncRepl` is written atomically, so a set missing a peer's stanzas
is a deletion: an unreadable bind-password Secret, a discovery peer with no
resolved addresses, or `len(ep.URIs) == 0` each tear that peer's cross-site
stanzas off every pod until it resolves. M1 added two triggers to the second.

**Done when** an unresolvable peer withholds the pod's stanza convergence and
reports, rather than converging to a set that omits it — red-first, with a
cluster whose second peer has an unreadable Secret leaving *both* peers'
stanzas in place.

**Roles.** test-author → implementer → reviewer (correctness).

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

**Goal.** The `ridBase` → `ridSegment` rename lands in M3; M6 is what follows
it through the docs and the tooling. `slctl`: `rid-uniqueness` stays, `rid-ledger-match` becomes
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
M1 ─┬─► M3 ─► M4a ─► M4b ─► M5 ─► M6 ─► M7
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
- ~~The three unguarded `Replace` paths in `docs/BACKLOG.md`.~~ **They are
  M4b.** This exclusion predates M1, which added two new triggers to one of
  them and turned a pre-existing wart into this work's own release gate.
  Corrected 2026-10-05, found by the M4b test author reading the plan against
  its own milestone list.
- Rollback to `ridBase`. Untested by decision; `docs/UPGRADING.md` says so
  plainly and names the manual path.
