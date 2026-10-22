# ADR-040: What a data-plane image is called, and which one a cluster runs

**Status:** Accepted 2026-10-22 — after decisions 1 and 2 were tested against
the tree and both had to be corrected; see the notes inside each. **Amended the
same day**, reversing decision 8 and deleting decision 4: read the amendment
before any decision section. ADR-021's choice of OpenLDAP 2.7 as the default
**stands**; what this ADR changes is the default *flavour* (pure, not patched)
and the reason 2.7 is held.
**Date:** 2026-10-22

## Context

A slapd image tag today names the **slaptain release and nothing else**:

| tag | OpenLDAP | built by | patches |
|---|---|---|---|
| `slapd:0.3.1` | 2.7.1 | us, vendored fork | ours, in `debian/patches/series` |
| `slapd:0.3.1-ol26` | 2.6.x | Debian's prebuilt binary | Debian's only |
| `slapd:0.3.1-ol26src` | 2.6.x | us, from source | `local-patches/` — two of them |

Three problems follow, and one of them is not cosmetic.

**The OpenLDAP version is invisible.** `slapd:0.3.1` says nothing about 2.7.1,
and `-ol26` / `-ol26src` name neither which 2.6 nor which patches. That gap is
already being papered over: `d2dd310` added an image **label** so the 2.6
source build could "say so", which is a workaround for a tag that cannot
carry it.

**The patched build is the default**, so an experimental feature's prerequisite
is implicit. ADR-037's rotation needs `accesslog-purge-phase.patch`; a user who
turns rotation on gets it without ever deciding to run a patched slapd.

**The default image decides an on-disk format, and the operator sets it from
its own tag.** 2.7 writes LMDB 1.0, which 2.6 cannot open, and
`docs/OPENLDAP-VERSIONS.md` records the break as symmetric: there is no
shortcut in either direction. Because the operator defaults both data-plane
images from `OPERATOR_IMAGE_TAG`, **moving the default OpenLDAP version is not
a default change — it is a forced migration of every cluster that said
nothing**, arriving on operator upgrade, at every site. That is the class
ADR-013 and ADR-035 exist to prevent, and it blocks any default move in either
direction, including a future 2.7 → 2.8.

ADR-021 deferred version gating explicitly: *"No operator feature is 2.7-only
today, so nothing has to branch on the slapd version. The mechanism stays an
open question until the first such feature exists."* That is still true, and
this ADR answers the question it left open — differently than it anticipated.

## Options considered

### Option 1: Upstream-dominant, k3s / CloudNativePG style — rejected

`slapd:2.7.1-slaptain3`: upstream version, then our revision. Reads well, and
the string already exists — `debian/changelog` says `2.7.1-0+slaptain3` and
`slapd -VV` prints it.

Rejected on two counts. It is wrong for `slapd-init`, which contains
`bootstrap.sh` and `startup_config.py` — slaptain behaviour that changes often
and implements a **contract with the operator** — and whose OpenLDAP content is
only the tools, present so `slapcat`/`slapadd`/`slaptest` cannot drift from the
server. The pair must be pinned together (ADR-021), so one scheme serves both,
and a scheme that names the OpenLDAP build hides the thing that actually
governs whether the init image works.

And the premise does not hold here. CloudNativePG can lead with upstream
because its images are independently consumable across a range of operator
versions. Slaptain's init image is a matched set with one operator, and that
contract has broken before: the pre-ADR-004 chart spoke the old init contract
and produced a slapd with `cn=config` and no data database at all.

### Option 2: Status quo plus more labels — rejected

Labels are the right place for provenance and the wrong place for identity. A
reader picking a tag should not need `skopeo inspect` to learn which OpenLDAP
they are about to run.

### Option 3: Slaptain-dominant with an explicit upstream token — chosen

`<slaptain>-ol<openldap>[-slaptain]`. This is Strimzi's shape
(`kafka:0.43.0-kafka-3.8.0`) and, as it turns out, it is also today's scheme
with the default flavour's suffix elided. The defect was never the ordering; it
was the elision plus an opaque flavour vocabulary.

## Decision

### 1. The tag scheme

```
slapd:0.4.0                       # alias → the pure upstream default
slapd:0.4.0-ol2.7.1               # pure upstream 2.7.1, explicit
slapd:0.4.0-ol2.7.1-slaptain      # our patch set
slapd:0.4.0-ol2.6.10              # pure (Debian's prebuilt binary)
slapd:0.4.0-ol2.6.10-slaptain     # our 2.6 source build
```

`slapd-init` carries the identical scheme and is pinned with it, unchanged from
ADR-021: both images or neither.

The OpenLDAP component is **derived, never hand-written** — from
`debian/changelog` for builds we compile. Debian versions carry `+`, `~` and
possibly an epoch, none legal in an OCI tag, so a sanitising rule is required
and must be stable, or the tag moves for cosmetic reasons.

**And the prebuilt build must pin its Debian version.** The `-ol26` pair does a
bare `apt-get install slapd`, so its version is whatever the archive serves at
build time. That breaks this decision twice: the tag cannot be computed before
`podman build -t` runs, and — worse — **the tag would change without any change
in our tree**, because a Debian point release silently renames the artifact. So
`Containerfile.ol26` pins the version explicitly (`slapd=<version>`), which
makes it a declared fact in the tree rather than a property of the day.

That pin is worth having on its own account, independently of tagging: it makes
the image reproducible, and it turns a Debian point release into an explicit
commit that can be reviewed and validated, rather than a silent difference
between two builds of the same source. This was found by testing the decision
against the tree before accepting it; the original wording said the version
could be read with `dpkg-query`, which is true only *after* the build and
therefore too late.

`-debug` and `-asan` remain suffixes: they are build variants of a given
OpenLDAP build, orthogonal to both axes.

### 2. The unsuffixed build is pure; patched requires the token — *refined*

> **Refined by the amendment, not reversed.** Everything below stands: the
> naming, the inversion, the sequencing, and the reason step 3 cannot be
> skipped. What the amendment adds is a layer above it — `select.flavour` is
> how a cluster asks for pure or patched, so this decision governs what the
> artifact is **called** and the selector governs which one is **chosen**. A
> tag remains available to anyone who wants to pin an exact artifact.

`slaptain` over `patched`: it names the provenance, echoes the package revision
`2.7.1-0+slaptain3` that `slapd -VV` prints, and does not invite "patched with
what?" at every reading.

This inverts today's meaning and is the point. Running a slapd carrying
non-upstream behaviour patches becomes a deliberate act, and ADR-037's rotation
acquires a real two-act opt-in: turn the feature on, *and* ask for an image
that can express it. A user who does only the first gets the two-field fallback
and `AccesslogPurgeConverged=False/PurgePhaseUnsupported`, whose message
already names the required version.

*(Amended: the second act is normally `select.flavour: slaptain` rather than a
pinned tag. That is the refinement — the deliberateness this decision exists to
create is preserved, and it moves to a field that says what it means instead of
to a token a reader has to decode. Pinning a tag still works and still counts
as the second act. The condition message's instruction to check `slapd -VV` is
only sound once a pure build stops reporting `+slaptain3` — see the amendment's
prerequisites.)*

The patched line is expected to accumulate further patches over time, each for
a stated reason; `accesslog-purge-phase.patch` is the first.

**The inversion is sequenced, and the order is part of this decision.**
`accesslog-purge-phase.patch` sits in `debian/patches/series`, so *every*
`slapd:<tag>` ever built, and every e2e cycle ever run, used the patched build.
A pure 2.7.1 pair does not exist and has never been through the suite.
Inverting the alias first would therefore make the default image one that has
never run our tests — precisely what ADR-039 tier 1 forbids. So:

1. move the patch out of `series` into the `local-patches/` mechanism the 2.6
   source build already uses, so patchedness becomes a build axis;
2. build the pure pair;
3. put it through a **multi-site e2e cycle**;
4. only then invert the alias.

The patch is **not inert when the rotation is off**, which is why step 3 cannot
be skipped by arguing the two builds are equivalent in the default
configuration: `2.7.1-0+slaptain3` takes `rq_mutex` around the purge task's
reschedule and deliberately moves upstream's own unlocked `re->interval.tv_sec`
write inside that lock. That path runs on every schedule change regardless of
the feature. A green suite on one line says nothing about the other.

### 3. Provenance goes in OCI labels, derived not asserted

`org.opencontainers.image.version`, `.revision` (git SHA), plus the existing
`net.chuck-chuck-chuck.slaptain.local-patches`, which is computed from the
patch directory so it cannot drift from what was applied. `slapd -VV` stays the
second independent route to the same fact.

### 4. A cluster records the data-plane version it was born with — ~~DELETED~~

> **Superseded the same day; never implemented.** The probe must mount the PVC
> (an ADR-018 deletion lease) and the cache goes stale in exactly the migration
> it exists to protect. The selector replaces it: the user's declaration *is*
> the record. See the amendment, §"Decision 4 is deleted, not implemented".
> Kept for the argument, not the conclusion.

The operator's compiled-in default applies to **new clusters only**. An
existing cluster keeps what it was created with until a human changes it.

This is the `exporterDefaultVersion` mechanism (ADR-033) plus persistence, and
the same reasoning ADR-037 uses for `slotLength`: a value that fixes something
persisted is declared, not re-derived each pass. Without it every default move
is a forced migration; with it, moving the default costs existing clusters
nothing and the migration becomes opt-in on the operator's own schedule.

**The backfill records the OLD default, and this is the sharp edge.** Every
cluster in existence predates this field, so the operator writes the value on
first reconcile after the upgrade — and the value it must write is the version
those clusters have actually been running, **not** whatever the compiled-in
default has since become. Backfill the new default instead and the mechanism
inverts: the upgrade that exists to protect silent clusters is the one that
hands each of them an image that cannot open its own volume.

So the backfill is pinned to a constant naming the previous default rather than
to the current one, which means it cannot be written as "record the default".
It looks correct either way in review, it is one line, and it is the single
place where getting decision 4 wrong is worse than not having decision 4 at
all. It gets a red-first test of its own.

### 5. Detect to refuse, not to choose

A preflight reads the on-disk LMDB format version and **withholds** a pod whose
image cannot open its volume, reporting a condition that names the mismatch and
points at the migration runbook.

Inferring the image *from* the volume was considered and rejected: the init
container is the same image pair as slapd, so a wrongly-chosen 2.7 init
container cannot read a 2.6 volume either — the detection would need its own
version-agnostic component — and, more importantly, autodetect that overrides a
declaration makes the operator fight a user who is deliberately upgrading. The
declared version stays authoritative.

The shape is ADR-029's: a missing certificate is a legible state, so withhold
the StatefulSet and say why. This converts a fleet-wide `CrashLoopBackOff` into
something readable in one `kubectl describe`, and it is belt-and-braces for
decision 4.

### 6. Branch on observed capability, not on a version string

Where the operator must behave differently, it probes. The precedent is already
shipped: `isPurgeArityRejection` recognises a stock slapd refusing the
three-field `olcAccessLogPurge` and distinguishes it from the patch's own
bound. A version number is a proxy that goes stale the moment someone runs a
backport or a distro patch; a refusal is ground truth.

Note that the axis that actually matters today is **patched vs pure**, not 2.6
vs 2.7 — the purge-phase patch exists in both source builds. A version matrix
would have modelled the wrong thing.

### 7. Exactly one version line is tier 1, in both flavours

Tier 1 (ADR-039) is a standing promise of ongoing validation, so **every tier-1
cell costs a multi-site e2e cycle every release, forever**. There are two
independent axes here — patched vs pure, and 2.6 vs 2.7 — which is four cells,
and four recurring mesh cycles is not affordable.

So: **at any time exactly one OpenLDAP version line is tier 1, and within it
BOTH flavours are tier 1. The other version line is tier 2 in both its
flavours.** Two cells, two cycles.

The cut falls on that axis and not another for two reasons, one forced and one
chosen:

- **Forced.** The project's commitment is that nobody is required to run a
  patched slapd in production. That commitment is incompatible with pure at
  tier 2, because tier 2 says in terms that the project does not assert fitness
  for production. Both flavours of the default version are therefore tier 1,
  and this is not a preference.
- **Chosen.** A "default version" already implies a single answer, and
  promising two is incoherent. ADR-021 effectively made this cut already — 2.7
  default, 2.6 as the ADR-011 interop lane — without naming it. Naming it is
  what makes the inverse expressible.

Consequences worth stating. "Supported equally well" for the pure line means
everything **that line can express**: the rotation cannot be among them, since
a stock slapd refuses the three-value `olcAccessLogPurge` — consistent, because
the rotation is tier 2 anyway. The rule scales without growing: a future 2.8
does not add a cell, it *moves* the tier. And promoting the second line to
tier 1 stays possible as a deliberate, priced decision — doubling the matrix is
exactly the cost ADR-028's one-path rule exists to make visible rather than
incur by drift.

The mechanism for the second cycle already exists: `SLAPD_TAG_SUFFIX` points
the whole suite at another slapd pair, so this is a second *run*, not a second
harness.

### 8. The default version is OpenLDAP 2.6 — ~~REVERSED~~

> **Reversed the same day: the default stays OpenLDAP 2.7 and becomes `pure`.**
> The weighing below is the error, not the facts. ITS#10597 is temporary and
> self-resolving (2.7.2, inside the same format line, no migration); a
> migration is permanent. Trading a time-limited bug for a permanent migration
> is the wrong trade. See the amendment, §"Decision 8 is reversed". Point 2 —
> that whatever fixes ITS#9398 is available on both versions — survives intact
> and is reinforced: the exposure is not version-specific, and as of
> 2026-10-23 it is not purge-specific either (`docs/ITS-9398-REPRODUCTION.md`).

This inverts ADR-021. It is a choice made on today's evidence, not a conclusion
waiting on a measurement, because the project is alpha and the way to settle it
is to run the chosen configuration and let the runs accumulate. One green cycle
proves nothing; a hundred of them is what confidence is actually made of.

In order of weight:

1. **ITS#10597 is 2.7-only.** A shared `entryUUID` is freed from an unlocked
   path on every client search that completes inside a write — a use-after-free
   under read-heavy load, which is the ordinary shape of a directory workload.
   Fixed only in master. The risk is unbounded.
2. **The reason ADR-021 gave for 2.7 is retracted** (its 2026-10-04 amendment):
   the storm is ITS#9398, not ITS#9580, and 2.7 fixed nothing because no
   release has. Whatever does fix it — the `syncprov-checkpoint` on the log
   database is the current candidate, with controls in `docs/its9398/` showing
   `1 1` holding the on-disk `contextCSN` current where `100 5` does not — is
   a configuration available on **both** versions, so it is not a reason to
   prefer either.
3. **"We did not mess this up" is the right posture for alpha software** asking
   people to trust it with a directory. Debian's 2.6 is what every distribution
   ships and every operator already knows, and the pure 2.6 line is literally
   Debian's own binary.
4. The cost of this choice — a dump/reload whenever a cluster eventually moves
   to 2.7 (LMDB 1.0, `docs/OPENLDAP-VERSIONS.md`) — is **bounded and
   documented**, and ADR-023 is designed to make it cheaper still.

**What would invalidate it: OpenLDAP 2.7.2.** It carries the milestone for
ITS#9398 and should carry ITS#10597's fix. When it ships, the version-specific
case for 2.6 largely evaporates and in-place LMDB upgradeability becomes the
dominant argument. Revisit then, not before.

### Execution order

> **Rewritten by the amendment.** The original order existed to sequence a
> *version* flip behind born-with pinning. There is no version flip — 2.7
> stays — and no born-with pinning. What remains is a **flavour** flip, which
> carries no format risk at all: pure and patched 2.7.1 open the same volumes.

1. Pin the Debian version in `Containerfile.ol26` (decision 1). **Landed.**
2. Patch out of `series` → build the pure 2.7.1 pair → mesh cycle → invert the
   alias (decision 2). Build axis and pure pair **landed**; cycle B validated
   the pair (109/2, both failures explained); the alias inversion is open.
3. The selector (`spec.images.slapd.select.{storageFormat,flavour}`), and the
   volume preflight that refuses legibly (decision 5).
4. **Then** flip the default flavour to `pure`, gated on its two prerequisites
   — a validated pure pair (met) and the version-string collision fixed (open,
   `docs/BACKLOG.md`). `lmdb0.9` becomes reachable through the selector rather
   than through a tag.

Steps 1 and 2 are independent of the flavour choice and landed first.

**The ordering risk inverted with decision 8.** It used to be "do not move the
default before a cluster records its format". It is now "do not make `pure` the
default while a pure build still reports `+slaptain3`" — because the shipped
`PurgePhaseUnsupported` condition tells operators to verify the patch with
`slapd -VV`, and on the day pure becomes the default that instruction misleads
the majority.

**This is an order of LANDING, not of releases.** All four can ship in one
release and are intended to — a release is a snapshot, and what must happen in
sequence is the code arriving, not the tags being cut. Splitting them across
releases buys a user nothing (the migration is experienced once either way) and
costs a manual publish each, since `publish.yml` has never worked.

### Revisit condition

If slapd images ever become independently consumable — the operator supporting
a *range* of data-plane versions rather than a matched set — upstream-dominant
becomes right and decision 1 should flip. Recorded here so that the condition
is noticed rather than rediscovered.

## Amendment, 2026-10-22: the image is SELECTED, and decision 8 is reversed

Decisions 2, 4 and 8 were accepted this morning and are revised the same day —
but not in the same way, and the difference matters to anyone executing them:

- **Decision 2 is REFINED, not reversed.** The unsuffixed build is still the
  pure one and patched still takes the token. What changes is that the tag
  stops being how a *user* asks for a flavour — the selector is — so decision 2
  now governs what an artifact is **called**, not how one is **chosen**. Its
  sequencing stands verbatim, and its alias inversion is still to do.
- **Decision 4 is DELETED**, never implemented.
- **Decision 8 is REVERSED**: the default stays OpenLDAP 2.7.

The reasoning is recorded rather than the text quietly edited, because the two
that did not survive were undone by arguments worth more than the conclusions.

### What replaces decision 4, and layers over decision 2: a selector, with a default

```yaml
spec:
  images:
    slapd:
      select:                    # optional; unset takes the defaults below
        storageFormat: lmdb1.0   # the COMPATIBILITY axis — what can open your volumes
        flavour: pure            # the TRUST axis — do you run our patches
```

Two axes, each named for what it actually governs, neither a version number
pretending to be a guarantee. An explicit `tag` still pins an artifact for
anyone who wants one.

**A selector keyed on the storage format cannot silently break a cluster**,
which a version-keyed one can. "The current lmdb0.9 image" moving 2.6.10 →
2.6.11 is safe by construction; moving across formats is impossible, because
the selector names the format. So slaptain keeps shipping newer slapd builds —
security fixes included — and the only thing that could strand a volume is the
one thing the user pinned.

It also drives **both** images. "Pin both or neither" (ADR-021) stops being a
documented rule a user can violate and becomes unviolatable: one selector, one
matched pair.

### Decision 4 is deleted, not implemented

Recording the version a cluster was born with was designed to stop a default
move from handing an existing cluster an unopenable volume. It does not survive
contact:

- **The probe is not free.** Reading the LMDB meta page means mounting the PVC,
  and ADR-018 says any pod object naming a PVC — finished pods included — is a
  deletion lease that blocks pod re-roll. The mechanism re-creates the hazard
  we wrote an ADR about.
- **The cache goes stale in exactly the case it exists for.** A deliberate
  2.6 → 2.7 migration leaves a recorded line that now names the old format, and
  the operator would use it to pick the old image — *fighting the migration*.
  That is decision 5's own objection to inferring an image from a volume, which
  this ADR failed to apply to itself.
- **It bought one outage at one rare event**, remediable by a one-line CR edit,
  in exchange for a probe, a cache, an invalidation story and a backfill.

With a selector there is nothing to record: the user's declaration *is* the
record, it lives in their GitOps source of truth, and the operator never writes
to their spec. Decision 5 shrinks to its valuable core — recognise a startup
failure and report it legibly, the `isPurgeArityRejection` shape decision 6
already prefers.

### Decision 8 is reversed: the default stays OpenLDAP 2.7, and becomes `pure`

The morning's case for 2.6 rested chiefly on ITS#10597, a 2.7-only
use-after-free under read-heavy load fixed only in master. The error was not
the fact but the weighing: **the two costs have different shapes.**

- The UAF is **temporary and self-resolving**. 2.7.2 carries its milestone, and
  default users receive it automatically — *within the same format line*, so no
  migration.
- A migration is **permanent**. Default 2.6 now and every such cluster
  eventually pays a dump/reload to reach 2.7.

Trading a time-limited bug for a permanent migration is the wrong trade.

What follows is the shape worth having: **whoever takes the default never
migrates**, and whoever overrides to lmdb0.9 does so deliberately, knowing they
may. ADR-023 is the path we owe the second group — its stated headline use case
is this exact break without the ADR-014 outage, and it should work, because
syncrepl is a protocol and not a format: a rebuilt pod on a 2.7 image
full-refreshes from its 2.6 peers, one ordinal at a time. Designed, plausible,
and unvalidated (ADR-027 records mixed-version mesh behaviour as reasoned
rather than measured).

**And there is a default, deliberately.** An earlier draft of this amendment
required the user to choose, with the chart failing at template time. That was
refused on the grounds that matter most for adoption: software that does not
start out of the box is software people stop evaluating. A default that nobody
ever has to migrate off is not a guess imposed on a user — it is the one
default that costs them nothing.

### Two prerequisites for the `pure` default

1. **Pure must be validated**, or we default to the artifact with the least
   history — the thing ADR-039 tier 1 forbids. Cycle B did this on 2026-10-22:
   109 passed, 2 failed on a three-site mesh, both failures explained below.
2. **The version-string collision becomes a blocker.** `SLAPTAIN_PATCHES=0`
   currently produces a build reporting `2.7.1-0+slaptain3`, identical to the
   patched one, while `PurgePhaseUnsupported` tells operators to verify the
   patch with `slapd -VV`. Latent while pure is the exception; with pure as the
   DEFAULT it misleads the majority, every time they trip it. See
   `docs/BACKLOG.md`.

### What cycle B taught about the two axes

They are not independent in the way the API suggests. Measured on the pure
pair:

- **`WriteOnly` passed.** Rotating the write endpoint needs no patch — it is a
  Service the operator repoints.
- **`PurgeOnly` failed**, correctly: the pure build refuses the three-value
  `olcAccessLogPurge`, so there is no phase to stagger.

So `flavour: pure` is compatible with `mode: WriteOnly` and incompatible with
any mode that stages phases. Neither the field documentation nor ADR-037 says
so, and both should: a user choosing the default flavour and then enabling the
rotation gets two thirds of a feature and one condition explaining why.

The storm also reproduced here — 6582 stale-cookie lines on a slapd carrying
**none** of our patches, which is the strongest evidence yet that ITS#9398 is
upstream's and not ours.

## Consequences

> **Reconciled with the amendment, 2026-10-23.** This section sits after the
> amendment but was written before it, and for a day it asserted the reversed
> decision 8 as though it were live — the default moving to 2.6, and every
> cluster eventually paying a dump/reload. Both are now corrected below. The
> bullets the amendment did not touch are unchanged.

- **`image.tag | default .Chart.AppVersion` survives as an identity** for the
  default case, because the tag still leads with the slaptain version. No third
  version line, no change to `scripts/image-tag.sh`'s two streams.
- **Nine pushable images instead of seven** (a pure 2.7.1 pair joins), plus the
  derivation and sanitising of the OpenLDAP component.
- **`accesslog-purge-phase.patch` moved** out of `debian/patches/series` into
  the `local-patches/` mechanism the 2.6 source build already uses, so
  patchedness is a build axis (`SLAPTAIN_PATCHES`) rather than a property of
  the packaging fork. **Landed**, with a symmetric build gate: a patched build
  must accept the three-value `logpurge` and a pure one must refuse it, so
  "pure" is a verified property rather than a claim.
- **Two tier-1 cells, and a standing bill of two mesh cycles per release**
  (decision 7). The tier-1 version line is **2.7**, in both flavours. That is
  the price of promising that nobody must run a patched slapd in production,
  and it is a recurring cost, not a one-off. It is affordable only because
  `SLAPD_TAG_SUFFIX` already points the suite at another pair — the second
  cycle needs a run, not a harness.
- **ADR-021 keeps its central choice; what it loses is its reason.** 2.7 stays
  the default (decision 8, reversed by the amendment). ADR-021 held 2.7 for
  ITS#9580, which its own 2026-10-04 amendment retracted — the storm is
  ITS#9398 and no release fixes it. 2.7 is now held for a different reason
  entirely: **whoever takes the default never migrates.** The 2.6 line becomes
  the tier-2 one, reachable through the selector's `storageFormat: lmdb0.9`.
- **No cluster that takes the default ever pays an LMDB dump/reload.** That is
  the shape the amendment was chosen for, and it inverts what this section
  originally said. A deployment that overrides to `lmdb0.9` does so knowing it
  may migrate later; ADR-023 is the path we owe that group, and its headline
  use case is exactly this break without the ADR-014 outage — designed,
  plausible, and unvalidated.
- **The 2.7-only ITS#10597 use-after-free is accepted, deliberately and
  temporarily.** It is a read-heavy-load bug fixed only in master, and 2.7.2
  carries the fix *inside the same format line*, so default users receive it
  with no migration. The trade is a time-limited bug against a permanent one;
  2.7.2 is also what would reopen decision 8's revisit condition.
- **The selector is the API surface**, replacing decision 4's probe-and-cache
  entirely: `spec.images.slapd.select.{storageFormat,flavour}`, defaulting to
  `lmdb1.0` + `pure`, driving **both** images — which makes ADR-021's "pin both
  or neither" unviolatable rather than merely documented. Keyed on the storage
  format rather than a version, so newer slapd builds (security fixes included)
  keep shipping under the same selection and the only thing that can strand a
  volume is the axis the user pinned. Not yet implemented.
- **The `pure` default has a blocker, not just prerequisites.** A pure build
  currently reports `2.7.1-0+slaptain3`, identical to a patched one, while the
  shipped `PurgePhaseUnsupported` condition tells operators to verify the patch
  with `slapd -VV`. Latent while pure is the exception; on the day it becomes
  the default it misleads the majority. `docs/BACKLOG.md` carries the fix
  shape. Decision 3's "second independent route to provenance" does not exist
  on the axis that matters until this lands.
- **Existing pins break.** Anyone pinning `spec.images.*.tag` must re-pin. At
  0.x, and under ADR-035's pre-1.0 rule, the clean design wins; it needs an
  `UPGRADING.md` entry and its own release.

## Related

- ADR-021 — the dual-image decision this refines; its deferred version-gating
  question is answered by decision 6.
- ADR-029 — a missing prerequisite is a legible state (decision 5's shape).
- ADR-033 — the exporter's independent version line and compiled-in default
  (decision 4's mechanism).
- ADR-035 — pre-1.0, the clean design wins where compatibility and clarity pull
  apart.
- ADR-037 — the first patch in the slaptain patch line, and the feature whose
  opt-in chain decision 2 completes.
- ADR-039 — which tier a given image is offered in.
- ADR-013 — an operator upgrade must not roll or break a running cluster;
  decision 4 is what keeps a default move inside that rule.
