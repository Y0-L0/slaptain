# Purge Rotation Implementation Plan (ADR-037)

The accesslog purge stalls the writes of the pod that runs it. This plan
implements the fix: per-pod purge phases plus a write endpoint that rotates in
lockstep, so the pod taking writes is never the pod purging.

Read [ADR-037](adrs/adr-037-purge-off-the-write-path.md) first, including all
three amendments. This plan implements its decisions and does not re-argue them.

## Resuming this work

Nothing is implemented yet. The prerequisite — a slapd that can express a purge
phase — exists as a local patch and a 2.6 image; see Phase 0 for what that
leaves undone.

Current state:

- `images/openldap-deb/local-patches/accesslog-purge-phase.patch` adds an
  optional third value to `logpurge`, epoch-anchored. Built into the
  `-ol26src` image pair by `images/openldap-deb/Containerfile.ol26src`.
- The patch is **not** in `images/openldap-deb/debian/patches/series`, so the
  default 2.7.1 pair cannot express a phase. That is Phase 0.

## What the measurement already settled — do not re-litigate

ADR-037's third amendment measured staggering on its own. It changes nothing a
client can see: staggered arms land on the synchronised arm and nowhere near
the arm where the write target did not purge.

Two consequences bind this plan:

- **Staggering alone is not a deliverable.** Phases without a rotating write
  endpoint produce a cluster that measures exactly like today. Phase 3 below
  therefore ends in a state that is *correct and useless*; do not mistake a
  green Phase 3 for working software, and do not ship there.
- **The whole value is in Phase 4.** Everything before it is scaffolding for
  the one thing that moves the number: the writes not being where the purge is.

## Hazards to read before writing any code

**1. `n` is `spec.replicas`, never readiness.** `n` fixes `T = n·t`, and `T` is
the interval written into every pod's `cn=config`. Deriving `n` from Ready pods
would rewrite the whole fleet's configuration on one transient restart — the
churn ADR-013 exists to prevent. Readiness belongs only in writer *selection*,
which persists nothing. Someone will try to "fix" this; the comment in the code
must say why it is not a bug.

**2. `t` is fixed and `T = n·t` varies.** Scaling then leaves existing phases
untouched and only moves the interval, and an interval change does not move a
phase. Holding `T` fixed and recomputing `t = T/n` re-phases the entire fleet on
every scale event.

**3. Nothing here may touch the pod template.** This is Services and `cn=config`
only. The golden templates in `operator/internal/controller/testdata/` pin that;
if they go red, the change is wrong. Do **not** regenerate them with
`-update-pod-template-golden`.

**4. The slapd config parser is strict about the time format.** `log_age_parse`
accepts `[ddd+]hh:mm[:ss]` with two-digit fields; `0:05` is rejected where
`00:05` is accepted, and a value with days must also carry minutes. Emit
`hh:mm:ss` or `ddd+hh:mm:ss` unconditionally and unit-test the formatter against
known values before anything else uses it.

**5. An unpatched slapd rejects a three-field value** with
`<olcAccessLogPurge> extra cruft after <age> <interval>`. See Phase 3 for the
decided handling — attempt, then report.

## Conventions this plan follows (verified against the codebase)

- The purge value is decided in one place. That was `desiredAccesslogPurge(sd)`
  over a two-token string when this plan was written; since Phase 5 it is
  `planPodPurge` over three typed quantities (`composePurge` renders them).
  Either way, one composition seam.
- `ensureAccesslogPurge(ctx, conn, host, dataDN, purge)` (`:1720`) already runs
  **once per pod**, from a call chain that has `ordinal int32` in scope. Per-pod
  divergence is a value change on an existing path, not new machinery.
- Services are reconciled on `SlapdClusterReconciler` with SSA patches —
  `reconcileClusterIPService` (`slapdcluster_controller.go:691`) is the pattern
  to copy.
- There is no clock abstraction and none is wanted: pure functions take
  `now time.Time`, and only the reconciler calls `time.Now()`.
- Red-first (CLAUDE.md Test Discipline). Every pure function in Phase 1 gets its
  assertions written, and observed failing, before its implementation.
- After CRD changes: `make operator-manifests` (syncs `charts/operator/crds/`)
  and `make verify-generated`.

## Phase 0 — the phase parameter reaches the default image

Without this the feature is unusable on 2.7.1, which is what every cluster runs.

1. Add `accesslog-purge-phase.patch` to
   `images/openldap-deb/debian/patches/series`. It applies cleanly to 2.7.1 —
   checked, seven hunks, offsets only — but has never been **built** there.
2. `make build-openldap-deb && make build-slapd build-init`.
3. Smoke-test: a three-field `logpurge` through `slaptest`, as in
   `Containerfile.ol26src`'s A/B — stock rejects it, patched accepts it.
4. Confirm `slapd -VV` on the 2.7 image carries the `+slaptain` marker.

**Done when** a 2.7.1 image accepts `logpurge 0+00:15 0+00:05 0+00:02`.

## Phase 1 — the decisions, as pure functions (no cluster, red first)

A new file, `operator/internal/controller/purgerotation.go`, holding only
arithmetic:

```go
func formatLogAge(d time.Duration) string            // hazard 4
func purgePhase(ordinal, n int32, t time.Duration) time.Duration
func rotationSlot(now time.Time, t time.Duration, n int32) int32
func composePurge(age string, interval, phase time.Duration) string
func writeTarget(slot, n, d int32, eligible func(int32) bool) (int32, bool)
```

`writeTarget` walks the preference order `d, d−1, … 1`, skips the pod purging
this slot (offset 0), and takes the first for which `eligible` reports true.
With `n = 2` and only the purging pod eligible it returns that pod — ADR-037's
decided degenerate case, writes continuing badly beating writes not continuing.

Assertions worth having before the code exists: the grid is invariant under when
you ask it; `purgePhase` is distinct for every ordinal and `< T`; a scale-up
leaves lower ordinals' phases unchanged; `writeTarget` never returns the purging
pod unless it is the only candidate; `formatLogAge` round-trips through the
format `log_age_parse` accepts.

**Done when** those are green and at least two deliberate mutations have been
observed to turn them red.

## Phase 2 — API types

On `SlapdCluster`, a rotation block carrying `enabled`, the slot length `t`, and
`d`. `d` stays a tunable: ADR-037 bounds it by `d·t ≥ P` and `(n−d)·t ≥ D`, and
`D` has no measured value yet.

`SlapdDatabase.spec.replication.accesslogPurge`
(`slapddatabase_types.go:254`) keeps the **age** only. When cluster rotation is
on, an interval in that field is **refused with a condition**, never silently
overridden — ADR-024's rule that a field which cannot be honoured is rejected
rather than ignored.

**Done when** `make verify-generated` is clean and the CRD round-trips.

## Phase 3 — converge the per-pod value

Thread the ordinal into the composition at `desiredAccesslogPurge` and emit
`<age> <interval> <phase>` per (pod, database). **All databases on a pod share
that pod's phase** — aligned, not spread (ADR-037, second amendment).

Capability handling, decided: **attempt, then report.** Write the three-field
value; on rejection, leave the two-field value in place and set a condition
naming the pod and the reason. No version probing, no marker convention. The
failure is loud, bounded, and tells the user exactly what to fix.

**Done when** every pod carries a distinct phase, and a cluster on an unpatched
image reports the condition instead of looping silently.

**Not done when it works** — see "what the measurement already settled". This
phase changes no client-visible number by design.

**Landed.** Three RW pods, two databases, `slotLength: 10m`:

```
slapd-0  2+00:00 00:30:00 00:00:00   7+00:00 00:30:00 00:00:00
slapd-1  2+00:00 00:30:00 00:10:00   7+00:00 00:30:00 00:10:00
slapd-2  2+00:00 00:30:00 00:20:00   7+00:00 00:30:00 00:20:00
```

Distinct per pod, aligned across databases, interval `3 × 10m`. The value is
composed at both write sites — the overlay's ADD and the converging pass —
because a stock slapd rejects the three-field value at config-parse time and
takes the whole ADD down with it, which would leave a fresh cluster on an
unpatched image with no accesslog overlay at all.

Four conditions carry what cannot be honoured: `RotationConfigured` on the
SlapdCluster (the four checks of ADR-037's fourth amendment, all captured live)
and `AccesslogPurgeConverged` on each SlapdDatabase (a declared interval is
withheld; a pod that refuses the phase is named). Both reported, neither
refused.

The e2e's teeth were proven the way the third amendment's warning demands: an
operator built to write one phase for EVERY pod was pushed, deployed, and
watched converge onto the fleet, and the spec caught it — `pod slapd-1 … has
purge phase "00:00:00", want "00:10:00"`.

Two notes for Phase 4. The writer displacement is parsed and reported but
nothing reads it yet: it is the write Service's input. (Phase 5 renamed it
`slotsFromWriteToPurge` and inverted its sign — see below.) And `rotationSlot`
— the only one of the five pure functions this phase did not call — is likewise
Phase 4's, which is why the grid still needs no clock anywhere.

## Phase 4 — the write Service rotates

1. A `<name>-write` Service selecting exactly one pod, via the
   `statefulset.kubernetes.io/pod-name` label. Copy the SSA pattern from
   `reconcileClusterIPService`.
2. Each reconcile: compute the slot from `time.Now()`, pick the writer with
   `writeTarget` and an eligibility predicate, patch the selector if it moved.
3. Eligibility: Ready; not purging this slot; not read-only under ADR-036's
   authorship gate; not working off backlog, lag or a recent absence
   (ADR-037 decision 4). **ADR-036 is still Proposed** — write that term as a
   seam that is inert until it lands, never as a dependency that blocks.
4. `RequeueAfter` the time remaining to the next slot boundary, which is exact
   because the grid is absolute.
5. A status condition naming the current writer and the next rotation time.
   Observation only.

**Done when** an e2e shows the Service selector following the grid, and the pod
it names is never the pod whose phase is current.

**Landed 2026-10-18.** `<cluster>-write` selects one pod by
`statefulset.kubernetes.io/pod-name` and moves at every slot boundary. Five
specs green on a single-site lab cluster (`E2E_LABEL_FILTER=write-rotation`),
red first against the Phase 3 operator — 5/5 failing on `services "slapd-write" not found`.

Three things worth carrying forward.

*The requeue was the trap.* The controller's steady-state `RequeueAfter` is
gated on `spec.replication.enabled`; a Running cluster without replication
returns a bare `ctrl.Result{}` and nothing schedules another pass. A rotation
requeue returned from inside that branch rotates replicated clusters and
silently never rotates the others. It is merged at the end of the pass instead
(`requeueAfterFor`), and the mutation that puts it back in the branch was
observed red.

*Readiness is per pod now.* The operator had only `sts.Status.ReadyReplicas`,
which is a count and names nobody. The rotation lists pods and reduces them with
`readyOrdinals`; a pod that could not be read is absent from the map and absence
is not readiness. Measured live: with `slotLength: 5s` and `slapd-1` deleted,
the endpoint went `slapd-0 → slapd-2` across the slot `slapd-1` was due to take,
and gave it back the second it went Ready again.

*Rotation off retires the endpoint rather than deleting or freezing it.* The
selector loses its pod-name entry and the Service becomes an alias of
`<cluster>`. Deleting it would turn a configuration change into a write outage
at every client that followed the advice; freezing it would pin every write to a
pod whose purge nothing phases any more, which is the measured stall arriving at
the moment the user believes they returned to the old behaviour. It is never
*created* when rotation is off, so a cluster that never rotated stays
byte-identical.

**The precondition, confirmed rather than discovered.** A Kubernetes Service
steers new connections and does not re-wire established ones. Measured through a
NodePort mirror of the endpoint, reading the writing pod out of each entry's own
`entryCSN` `#sid#`: with the endpoint moved `slapd-1 → slapd-2`, a fresh
connection wrote with sid `003` (slapd-2) and the connection held across the move
wrote with sid `002` (slapd-1). That is this document's own "Known risks" entry
observed, not a new finding: the rotation is for short-lived connections by
design, and a pooling client needs a maximum connection lifetime short relative
to `slotLength`. Where that ratio stops paying is unmeasured and no number is
invented for it.

## Out of scope, deliberately

- **The read Service** (ADR-037's ROs-plus-writer design). It is a freshness
  refinement with no measurement behind it, and it costs read capacity in
  inverse proportion to `readReplicas`. The write path is what the measurement
  says matters; ship that, then decide.
- **`d = n−2`, the cooldown slot.** Exists only for `n ≥ 3` and every arm so far
  is 2+2. Unknown, and left that way rather than guessed.
- **Upstreaming the patch.** It is ours and local; the packaging fork carries it.
- **Backup source selection** avoiding the purging pod.

## Known risks

- **`D`, the drain time, is unmeasured**, so `T ≥ P + D` cannot be evaluated for
  a real deployment and `d` cannot be fixed. The feature is sized on one half of
  its own rule. The rotation is for short-lived connections by design; a pooled
  client keeps writing to the old pod and the rotation does nothing for it.
- **Operator downtime degrades to today**: purges keep firing from `cn=config`,
  the writer stops rotating, and eventually the writer purges while taking
  writes. No worse than unmitigated — but the write Service must not be left
  selecting nothing.
- **Phase 0 is on the critical path** and touches the image every cluster runs.
- **Peak CSN lag was higher staggered** in two arms (34.7 s, 39.2 s against
  19.6 s synchronised). If that inversion is real, rotation mildly worsens
  replication lag while removing the client-visible stall. It does not change
  the decision; it may change what the release notes have to say.

## Phase 5 — decompose the purge directive by ownership (ADR-024 R6, ADR-037 ninth)

Not planned; forced by a measured, outage-class defect that the first four
phases created between them.

**What happened.** `olcAccessLogPurge` arrived from upstream pre-packaged as one
string field on `SlapdDatabase`. With rotation off the database declared
retention *and* interval; with rotation on the cluster took the interval and the
database's half was refused with a withhold. One field, two meanings, chosen by
another resource's mode.

On a fresh three-site mesh, a database declaring an interval on a cluster
**created** with rotation already on ended up with **no `olcAccessLogPurge` at
all** — every pod, every site. The withhold assumed there was an existing value
to keep, which is the same assumption the sixth amendment had already corrected
once on the unpatched-slapd path and which was never carried across to this one.
An unpurged journal grows to its map ceiling and then stops data writes, because
the accesslog overlay sits in the data database's write path.

The published examples made it reachable by following them:
`charts/slapd-mesh/examples/rotation.yaml` says to stack it on
`three-site.yaml`, which declared an interval.

**What was done.** Consolidating the API dissolves the defect rather than
patching it. There is no declared interval on a database, so there is nothing to
refuse, so there is no withhold, so there is no way to end up unpurged.

```yaml
# SlapdDatabase — what to keep
spec.replication.accesslogPurge:  {enabled, retention}

# SlapdCluster — when to purge, in exactly one of two forms
spec.accesslogPurge:              {interval}          # form A
spec.rotation:                    {enabled, slotLength, slotsFromWriteToPurge}
```

Landed with it:

- **Mutual exclusion is refused**, never resolved by precedence
  (`ScheduleDeclaredTwice`), and the default interval stays in force meanwhile.
- **`writerOffset` → `slotsFromWriteToPurge`**: a signed displacement taken mod
  `n`, so `-1` is `n-1` and survives a scale event. Default **1**. `minimum: 1`
  is gone; the rule is `v mod n ≠ 0`, which needs `spec.replicas` and is
  therefore reported rather than refused at admission.
- **The fallback walk degrades in the direction of the sign**, away from the
  purge, wrapping within the non-zero offsets: `-1` degrades to `-2`, not to
  `+1`, so a choice that cannot be honoured stays near what its author meant
  rather than crossing to the opposite end of it. An earlier ascending-only
  form (ADR-037 amendment 9) had one candidate whenever `v` sat at the end of
  the range, which handed writes to the purging pod with eligible pods
  unexamined; amendment 10 replaces it and withdraws the claim that the
  forbidden slot could fall off the end. The skip is explicit again, and the
  property test asserts the stronger form: never the purging pod **while any
  other pod is eligible**.
- **Two-token values are refused, never reinterpreted** (ADR-035: pre-1.0 the
  clean design wins). Because the field changed type, the refusal is the API
  server's rather than a condition — a stricter and earlier refusal than the
  ninth amendment anticipated, at the cost of one: a SlapdDatabase already
  stored with a string does not decode, so it must be rewritten rather than
  reported on.
- **`purgePodPlan` lost its `Withheld`/`Reason`/`Message` fields** and the
  reconciler lost two branches. The absence is the deliverable.

**The regression guard** is one ungated e2e assertion, deliberately the weakest
one available: every replicated database on every read-write pod carries *some*
`olcAccessLogPurge`. It is the defect's negation, it costs one search per pod per
database, and it would have caught the outage.


## Phase 5 — the lead time (2026-10-20)

Load measurement inverted the plan's premise: **staggering the purges is free;
moving the writes is what costs.** With rotation on and writes pinned,
throughput was indistinguishable from baseline; with writes following the
endpoint it fell 2.8 % and dropped iterations doubled, with 99.9 % of the loss
inside 30 s of a slot boundary.

The endpoint moved *at* the boundary, so the pod that stopped taking writes
began purging at the same instant and every connection not yet aged out
delivered writes into an active purge. `spec.rotation.leadTime` moves the
endpoint that far ahead of the boundary instead. It is a lookahead only — the
purge grid is untouched and `leadTime: 0` is byte-identical to the behaviour it
corrects.

One guard, evaluable from `n` and `t` alone: `d < (n−1)·t`. The lead is the
administrator's number, not the operator's — deriving it would need the client
connection lifetime, which the operator cannot see.

Shipped alongside: the runqueue-lock fix behind a SIGSEGV on a live
three-field-to-two-field transition, and the 2.7.1 purge-timing log line
backported to the 2.6 build so purge duration and delete rate can be read
rather than inferred.

See ADR-037 amendment 11 for the measurement, the feasibility arithmetic and
what was deliberately not decided.
