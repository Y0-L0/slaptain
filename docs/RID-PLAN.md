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

5. **The local site's own index must not collide with a declared peer's.**
   Under `meshRef` both come from `serverIDIndex` and ADR-028 enforces
   uniqueness. Off-mesh, self is `serverIDBase / 100` while peers declare
   `siteIndex`, so a hand-set base that is not a multiple of 100 — or simply
   equal to a peer's decade — silently puts two providers in one slot. Refuse
   where both are known.

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

### M4 — Consumption, the gate, and off-mesh site indices

**Goal.** Stanzas take their RID from `ridcoord`, keyed on the database's
declared segment and the provider's identity; `externalPeers[].siteIndex` added
with a CEL uniqueness rule over the peer list; self index derived from
`serverIDBase / 100` with the collision refusal (hazard 5); the withhold from
M3's detector wired into stanza convergence, leaving existing stanzas alone
(hazard 2).

**Also M4's: resolving the layout.** `ridLayoutFor(ctx, cluster)` in the
SlapdDatabase controller — under `meshRef` it reads `SlapdMesh.spec.ridLayout`,
otherwise it returns the defaults and does no I/O. Its failure mode is
hazard 3's shape: an unreadable mesh means the layout is **unknown**, and
unknown withholds rather than converging. `judgeRIDSegment` deliberately takes
the layout as a parameter, which is what keeps it testable without a client.

**Also M4's, and tracked as a release gate:** `SlapdMesh.spec.ridLayout` ships
in M3 as live API that **nothing reads** — the judgement hardcodes
`ridcoord.DefaultLayout()`. A user can declare `{podsPerSite: 400, maxSites:
400}` (C = 1), declare `ridSegment: 5`, and have both the API server and the
operator accept it, because the operator is judging against the default C of
49. ADR-024's rule is that a spec field which cannot be honoured is rejected or
converged, never ignored. Nothing miscomputes while the bridge ignores the
layout too; the damage lands here, when the same CR starts producing RIDs from
a layout nobody agreed to. M4 reads the field or M4 does not ship. Found by the
M3 correctness review, 2026-10-05.

**Also M4's, and it is the release gate** named below: the three pre-existing
paths that hand `Replace` a short set (`docs/BACKLOG.md`). M1 added two triggers
to one of them; nothing ships until this closes.

**Owns.** `operator/api/v1alpha1/slapdcluster_types.go` (the peer field),
`operator/internal/controller/slapddatabase_controller.go`,
`operator/internal/controller/slapddatabase_ridgate.go` (new), their tests, and
the regenerated CRDs.

**Depends on** M1, M2, M3.

**Roles.** test-author → implementer → reviewer (correctness, with hazard 2 as
the named target: prove that every refusal path leaves `olcSyncRepl` untouched).

**Two structural moves M4 should make while it is in this file**, both raised
by M3 and both production code the test author could describe but not write:

- **Lift the phase selection into a pure function** — `databasePhase(pendingWork,
  appliedPods, ridRefusal)` or similar. It is currently inline in `Reconcile`,
  so the precedence it encodes (a RID refusal outranks `PartiallyApplied` but
  never `NoPodsReachable`) cannot be unit-tested and is asserted nowhere. This
  is the plan's own design corollary — push the decision into a thin pure
  function so the e2e stays an integration shell.
- **Delete `legacyRIDBase`.** M3's bridge keeps RIDs byte-identical by computing
  `base = segment × 100` and refuses segments above 9 to stay inside slapd's
  band. M4 replaces the arithmetic with `ridcoord` and the bridge goes with it,
  which is also what lifts the temporary 0..9 ceiling back to `0..C-1`.

**Release gate, not optional (added 2026-10-05).** M1's correctness review
confirmed that a discovery refusal reaches the atomic `Replace` as a short set,
and that the same path opens a one-pass window on operator upgrade. Both are
instances of the pre-existing three-path defect in `docs/BACKLOG.md`, and both
are reachable only between M1 and M4 — which land in the same release. M4's
hazard-2 withhold is therefore what makes M1 shippable, and M5 must not run
before it.

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
