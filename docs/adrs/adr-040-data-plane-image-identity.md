# ADR-040: What a data-plane image is called, and which one a cluster runs

**Status:** Accepted 2026-10-22 — after decisions 1 and 2 were tested against
the tree and both had to be corrected; see the notes inside each. Supersedes
ADR-021's choice of OpenLDAP 2.7 as the default (decision 8).
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

### 2. The unsuffixed build is pure; patched requires the token

`slaptain` over `patched`: it names the provenance, echoes the package revision
`2.7.1-0+slaptain3` that `slapd -VV` prints, and does not invite "patched with
what?" at every reading.

This inverts today's meaning and is the point. Running a slapd carrying
non-upstream behaviour patches becomes a deliberate act, and ADR-037's rotation
acquires a real two-act opt-in: turn the feature on, *and* pin an image that
can express it. A user who does only the first gets the two-field fallback and
`AccesslogPurgeConverged=False/PurgePhaseUnsupported`, whose message already
names the required version.

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

### 4. A cluster records the data-plane version it was born with

The operator's compiled-in default applies to **new clusters only**. An
existing cluster keeps what it was created with until a human changes it.

This is the `exporterDefaultVersion` mechanism (ADR-033) plus persistence, and
the same reasoning ADR-037 uses for `slotLength`: a value that fixes something
persisted is declared, not re-derived each pass. Without it every default move
is a forced migration; with it, moving the default costs existing clusters
nothing and the migration becomes opt-in on the operator's own schedule.

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

### 8. The default version is OpenLDAP 2.6 — chosen now, revisable on evidence

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

The decision is taken now; the flip is sequenced, because until a cluster
records the version it was born with (decision 4), moving the default hands
every cluster that said nothing an image that cannot open its own volume.

1. Pin the Debian version in `Containerfile.ol26` (decision 1).
2. Patch out of `series` → build the pure 2.7.1 pair → mesh cycle → invert the
   alias (decision 2).
3. Born-with pinning and the volume preflight (decisions 4 and 5).
4. **Then** flip the default to 2.6, and both 2.6 flavours enter the tier-1 e2e
   rotation while 2.7 leaves it.

Steps 1 and 2 are independent of the version choice and can land first.

### Revisit condition

If slapd images ever become independently consumable — the operator supporting
a *range* of data-plane versions rather than a matched set — upstream-dominant
becomes right and decision 1 should flip. Recorded here so that the condition
is noticed rather than rediscovered.

## Consequences

- **`image.tag | default .Chart.AppVersion` survives as an identity** for the
  default case, because the tag still leads with the slaptain version. No third
  version line, no change to `scripts/image-tag.sh`'s two streams.
- **Nine pushable images instead of seven** (a pure 2.7.1 pair joins), plus the
  derivation and sanitising of the OpenLDAP component.
- **`accesslog-purge-phase.patch` moves** out of `debian/patches/series` into
  the `local-patches/` mechanism the 2.6 source build already uses, so
  patchedness becomes a build axis rather than a property of the packaging
  fork. It should move once, when this lands.
- **Two tier-1 cells, and a standing bill of two mesh cycles per release**
  (decision 7). That is the price of promising that nobody must run a patched
  slapd in production, and it is a recurring cost, not a one-off. It is
  affordable only because `SLAPD_TAG_SUFFIX` already points the suite at
  another pair — the second cycle needs a run, not a harness.
- **ADR-021 is superseded on its central choice** (decision 8): 2.6 becomes the
  default and 2.7 becomes the tier-2 line. ADR-021's own rationale for 2.7 was
  already retracted by its 2026-10-04 amendment; what this ADR adds is the
  replacement and the order in which it is safe to execute.
- **Every cluster created after the flip will eventually pay an LMDB
  dump/reload** to reach 2.7, whenever that becomes the right move. Bounded and
  documented (`docs/OPENLDAP-VERSIONS.md`), and ADR-023 exists to make it
  cheaper — but it is a real debt taken deliberately, in exchange for not
  defaulting onto a known use-after-free.
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
