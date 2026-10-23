# ADR-041: A pod says what format its volume is, before slapd blames the disk

**Status:** Accepted
**Date:** 2026-10-23

Implements ADR-040 decision 5 ("detect to refuse, not to choose"), which named
the shape and left the mechanism open.

## Context

ADR-040 gave users `spec.images.select.storageFormat`, which is the first time
the on-disk LMDB format is something a user declares rather than something they
get by accident. One line of YAML now points a cluster at the wrong format, and
the field's own documentation invites the edit.

What happens next was measured on a live three-pod cluster (2026-10-23):
populated on OpenLDAP 2.7, then `storageFormat: lmdb0.9` applied.

```
mdb_db_open: database "cn=slaptain-auth" cannot be opened:
MDB_INVALID: File is not an LMDB file (-30793). Restore from backup!
backend_startup_one (type=mdb, suffix="cn=slaptain-auth"): bi_db_open failed! (-30793)
slapd stopped.
```

Four things in that are wrong or misleading, and none of them is slapd's fault
— it is a generic message meeting a situation it was not written for:

1. **The volume is healthy, and the message prescribes a restore.** On a
   slaptain cluster that means the ADR-014 scale-to-0 machine over good data.
   This is the whole reason the ADR exists: the native failure does not merely
   fail to explain, it points at a destructive action.
2. **It is `MDB_INVALID`, not `MDB_VERSION_MISMATCH`.** A source read predicted
   the latter; the measurement refuted it. LMDB 1.0 changed the meta page
   enough that 2.6's reader never reaches a version check, so the error a user
   searches for returns corruption advice rather than version advice.
3. **It names `cn=slaptain-auth`** — ADR-027's node-local database, the first
   mdb opened — which the user never declared and will not recognise.
4. **The init container passes cleanly**, because it only writes configuration
   and never opens an LMDB data environment. A green init container currently
   says nothing about whether the volumes can be opened.

And the state is not reversible from the spec. Putting `storageFormat` back
updated the StatefulSet template and the pod **stayed on the wrong image**:
under `OrderedReady` Kubernetes will not replace a pod that is not Ready, so
the broken pod blocks its own replacement. Measured — template corrected, pod
unchanged through five restarts — until a manual `kubectl delete pod`, after
which it recovered in under 15 s with every database intact.

So a user reaches this with one CR edit and cannot leave it with another. That
is the argument for refusing before the pod is created rather than explaining
after it has failed.

Containment is good and is not the problem: the roll halts at the first bad
pod, the rest of the fleet keeps serving, and the cluster reports `Degraded`,
`Ready=False NotReady: 2/3` and `PodsResponsive=False` naming the pod. All
accurate; none of it about the image or the format.

## Options considered

### Option 1: nothing — let slapd's message stand — rejected

It was the status quo and it was measured. Rejected on finding 1: the message
is not terse, it is wrong about the remedy, and the remedy it names is
destructive.

### Option 2: an operator-side probe that reads the volume — rejected

This is ADR-040 decision 4's grave and the objection is unchanged: reading the
volume means a pod object naming the PVC, and ADR-018 says any such object —
finished pods included — is a deletion lease that blocks pod re-roll. A
diagnostic that re-creates the hazard we wrote an ADR about is not a
diagnostic worth having.

### Option 3: `slaptest` without `-u` as the gate — rejected as the mechanism

`slaptest` without `-u` runs `slap_startup(be)` (`slapcommon.c:936`), opening
every database through back-mdb. It is the same binaries and the same code
path, so its verdict is authoritative by construction, and it was the first
proposal.

It answers the wrong question. `slaptest` tells us **whether** the volume can
be opened, not **why not** — and on failure it relays back-mdb's message, which
is the misleading one. A preflight built on it either repeats "Restore from
backup!" or substitutes a claim it has no evidence for, and the second is worse:
asserting "wrong format" over a genuinely corrupt disk is the same error in the
other direction.

It also has side effects. Without `-u` it opens databases read-write, and on a
fresh volume back-mdb creates the environment — so the preflight would be
creating databases moments before slapd does. Probably harmless; unverified,
and "probably harmless side effect in the startup path" is a sentence this
project has paid for before.

Kept in reserve. If a future case needs an authoritative yes/no that the
detector cannot give, this is where it comes from.

### Option 4: ship an `mdb_stat` of each format in every image — rejected

Build `mdb_stat` from the 2.6 tree and from the 2.7 tree, carry both in both
images, and diagnose by which one opens the volume. It is attractive because it
decodes nothing: "which library can read this" is an empirical answer.

Rejected, and not only for cost:

- **It gives less information.** Three outcomes by elimination, where reading
  the meta page gives four — and its "neither could open it" conflates
  corruption with a format newer than both tools.
- **Its failure mode is the dangerous one.** If the foreign-version binary is
  missing, mislinked or silently not executed, the result is "neither can read
  it" and we report corruption on a healthy volume. A reassuring answer and an
  empty answer are indistinguishable unless something was built to tell them
  apart, and here they are the same exit code.
- **The cost is real**: cross-version binaries in four image cells, each
  needing static linking or copied libraries, built from two source trees, to
  answer a question ten bytes of file read answers better.
- OpenLDAP never builds liblmdb as a library in the first place —
  `back-mdb/Makefile.in:52-56` compiles `mdb.c` and `midl.c` straight into the
  module — so there is no artifact to reuse and the tool must be built
  specially either way.

### Option 5 (chosen): read the meta page in the init container, refuse only what is certain

## Decision

### 1. The init container is where this runs

It already mounts `/data` and `/accesslog`, so it names no PVC that is not
already named: no new pod object, no new deletion lease, ADR-018 untouched.
That is the whole reason this is buildable where decision 4 was not.

It also has to be *before* rather than *after*, and that is forced by
mechanics rather than preference. Init containers do not re-run when an app
container crashes — measured incidentally, the stuck pod reached five restarts
with its init container long completed — the slapd runtime image is distroless
with no shell to wrap (ADR-030 put exactly one client binary in it), and the
operator cannot read the volume. The one place that can read the volume runs
once, before slapd.

### 2. Read the magic and the format version, and report four distinct answers

`MDB_MAGIC` and `MDB_DATA_VERSION` sit adjacent at the head of `MDB_meta` in
both the 0.9 and 1.0 layouts, so the magic can be found in the first page and
the version read from the four bytes after it, without depending on a page
header size that differs between versions.

Four outcomes, each a different next step for the reader — the rule ADR-031
already applies to `slctl rid decode`, that a command with distinct situations
must not collapse them into one word:

| reading | meaning |
|---|---|
| known magic, known version, matches this image | the format is fine — **look elsewhere** |
| known magic, known version, does not match | this volume is `<X>`, this image reads `<Y>` |
| known magic, unknown version | a format this image does not know, named by number |
| no magic found | this file carries no LMDB magic |

### 3. Refuse only the certain case

The init container fails **only** on row 2: a magic we recognise, a version we
recognise, and a mismatch with this image. In that case slapd cannot start, so
a clear refusal is strictly better than a crashloop and costs no availability
that was not already lost.

Every other row records what was read and lets slapd proceed to succeed or fail
on its own terms. We do not refuse on a reading we cannot stand behind.

### 4. The detector must be able to EXONERATE the format

This is the load-bearing rule, not a refinement.

When the format matches and slapd still fails, the recorded reading must say
**"the format is correct; this is something else"**. Without that we have built
a machine that blames the format for every open failure — which is exactly how
`MDB_INVALID` came to carry "Restore from backup!": a generic failure wearing a
specific remedy.

Stated generally, because it outlives this ADR: **a detector that can only
accuse will eventually accuse wrongly.** Clearing the thing it checks is part
of its job, not a side effect of it.

### 5. Report the reading, never infer the condition

We may report "this file carries no LMDB magic". We may not report "corrupt".
A missing magic is equally a zero-length file from a failed restore, a
half-provisioned volume, or a wrong mount, and those want different actions
that we cannot tell apart. The message says what was read and stops.

### 6. The image's own format version is DERIVED FROM BEHAVIOUR, not from text

ADR-040 decision 3's rule applied to the half that can drift silently: the
image must not carry a hand-written claim about which format it writes.

The obvious derivation is to read `MDB_DATA_VERSION` out of `mdb.c`, and it is
the wrong one. It is `#define MDB_DATA_VERSION ((MDB_DEVEL) ? 999 : 3)` —
internal to `mdb.c`, not in `lmdb.h`, and a ternary that a naive extraction
reads as `999`. Parsing C in a shell to learn what our own binaries do is a
checker that can be wrong in the direction that looks fine.

So derive it from behaviour instead: **at image build time, create a throwaway
database with our own `slapadd`, then read its meta page with the detector we
are shipping.** The version it reports *is* this image's format, by
construction, with nothing parsed and nothing asserted.

That also makes the derivation its own gate, in the shape the openldap-deb
build already uses twice: it proves the detector can read a database this
image's tooling just wrote. If the detector and the server ever disagree about
the on-disk layout, the build fails rather than the diagnosis being quietly
wrong.

The other half — which human-facing name a given version number has — stays a
lookup, and degrades honestly to row 3 ("version 4, which this image does not
know") rather than guessing.

### 7. Per database, and absence is silent

Each database directory is read separately: "all of them" and "one of them"
distinguish a wrong image from a lost disk.

Three kinds of absence are a **silent pass**, not a warning, and all three are
ordinary:

- a directory with no `data.mdb` — every first boot;
- `/accesslog` not mounted at all — it is conditional on
  `NeedsAccesslogVolume()`, so a read-only pod and a non-replicated cluster
  legitimately have none;
- a database directory that does not exist yet — the mkdir Jobs and the init
  container create them (ADR-013 amendment), and the detector must not race
  that.

A detector that warns about ordinary states trains people to ignore it, which
costs more than the thing it was built to catch.

### 8. The operator learns the outcome without touching the volume

The init container writes its findings to `/dev/termination-log`; the operator
reads `initContainerStatuses[].lastState.terminated.message` and surfaces it on
the SlapdCluster. ADR-029's shape — a missing prerequisite is a legible state —
and the operator still never reads a PVC.

## Consequences

- **A declared format mismatch becomes a refusal with a remedy** instead of a
  crashloop with a destructive one. The message names the volume's format, the
  image's format, and the two ways out: change `storageFormat`, or migrate per
  `docs/OPENLDAP-VERSIONS.md`.
- **The exoneration path changes what a failure means.** "The format is fine"
  on a crashlooping pod is positive information and redirects the reader away
  from the migration runbook, which is where the current message sends them.
- **A format table exists and can go stale** — but only on the explaining path,
  never the deciding one. A stale table cannot break a cluster; the worst it
  produces is "unknown version N", which still beats "Restore from backup!".
  This asymmetry is why the fragile instrument is safe here and would not be as
  a gate.
- **We depend on an internal LMDB constant.** Mitigated by deriving our own
  side from source (decision 6) and by handling the unknown case honestly, but
  it is a real dependency on something upstream does not publish.
- **This does NOT fix the stuck pod.** Correcting the spec still leaves a
  crashlooping pod on the old image, because `OrderedReady` will not replace an
  unhealthy one. The refusal makes that state much harder to enter and does
  nothing to leave it; the exit is still a manual `kubectl delete pod`, now
  documented in `docs/OPENLDAP-VERSIONS.md`. Treating it properly is separate
  work and is filed in `docs/BACKLOG.md`.
- **The reverse direction is unmeasured.** Everything above was measured with a
  2.6 image against 2.7 volumes. A 2.6 volume under a 2.7 image is the
  direction ADR-021's migration runbook actually travels, and it may report
  differently — possibly the `MDB_VERSION_MISMATCH` that the source predicted
  and this direction did not produce. The detector does not care, because it
  reads the volume rather than the error; the runbook's wording may.

## Related

- ADR-040 — decision 5, which this implements; decision 4, whose grave explains
  why the operator may not do this itself; decision 3, whose derive-don't-assert
  rule decision 6 reuses.
- ADR-018 — the deletion lease that rules out any probe with its own pod.
- ADR-029 — a missing prerequisite is a legible state, reported not inferred.
- ADR-030 — why the runtime image has no shell to wrap.
- ADR-031 — a command with distinct situations must not collapse them into
  silence or one word; decision 2's four answers follow it.
- ADR-021 — the LMDB format break this exists to make legible, and
  `docs/OPENLDAP-VERSIONS.md`, its runbook.
