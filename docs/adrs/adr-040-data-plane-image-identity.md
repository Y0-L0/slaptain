# ADR-040: What a data-plane image is called, and which one a cluster runs

**Status:** Proposed
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
`debian/changelog` for builds we compile, from `dpkg-query -W slapd` for the
build we do not. Debian versions carry `+`, `~` and possibly an epoch, none
legal in an OCI tag, so a sanitising rule is required and must be stable, or
the tag moves for cosmetic reasons.

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
- **2.6 and 2.7 both become reachable as deployment targets** — subject to
  ADR-039's tiering, which is where the promotion is actually decided and
  priced. Decisions 4 and 5 are what make that choice safe; they do not by
  themselves promote anything.
- **The default's OpenLDAP version becomes a decision that can be taken**,
  in either direction, without breaking running clusters. Whether to take it —
  ADR-021's rationale rests on a misattribution corrected on an unmerged
  branch, and 2.7.1 carries ITS#10597 where 2.6 does not — is a separate
  decision this ADR deliberately does not make.
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
