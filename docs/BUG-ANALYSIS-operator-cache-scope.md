# Bug analysis: the operator caches every Secret, Pod and Service in the cluster

**Status:** SUPERSEDED IN PART, 2026-10-29, by PR #29 and ADR-042. The analysis below
is kept **as written**, because its §8 rejections still hold and its §4 hazard is real —
but §6's first piece is **withdrawn** and §5 is **dissolved rather than deferred**. Read
the Amendment at the end before acting on §6 or §10.
**Reported:** 2026-10-28, [issue #28](https://github.com/chuck-chuck-chuck-net/slaptain/issues/28), by an external contributor
**Severity:** The operator is OOMKilled at the chart's own default limit on any
sufficiently large Kubernetes cluster, while managing a single `SlapdCluster`.

This document is self-contained: a reader can come to it cold. It records the
analysis, the two findings that change the obvious fix, and the alternatives
rejected — those last are the part worth keeping, because they are what stops the
next person re-proposing them.

Every claim below is labelled **measured**, **observed once**, **source-confirmed**
or **argued**. They are not the same thing and the fix depends on which is which.

---

## TL;DR

The operator's memory is a function of the size of the Kubernetes cluster it runs
in, not of the number of slapd deployments it manages. `ctrl.NewManager` is built
with no `Cache` option, so every `Owns()` and every cached `Get`/`List` starts a
**cluster-wide** informer for that kind. Five kinds are cached: `Secret`, `Pod`,
`Service`, `StatefulSet`, `Job`.

The obvious fix — label what the operator creates and filter the cache on the label
— **does not survive an upgrade**, for a reason that is not obvious and is set out
in §4. And narrowing the event stream is the subject of an existing ledger entry
whose stated prerequisite is only half satisfied (§5).

The recommendation is therefore narrower than the report's and is staged.

---

## 1. Symptom

*Observed once, by the reporter; not reproduced here.*

- OOMKilled at the chart default of 128Mi (`charts/operator/values.yaml:42`).
- 553Mi with the limit raised, while managing **one** 2-replica `SlapdCluster`.
- A shared cluster with 84 namespaces.

## 2. What the cache is for, and what it actually does

**Intended** — controller-runtime's shared cache exists so repeated reads inside one
reconcile do not hit the API server, and so `Owns()` can deliver events. Its intended
scope is *the objects this operator manages*.

**Actual** *(source-confirmed)* — `operator/cmd/main.go:166` passes `Scheme`,
`Metrics`, `WebhookServer`, `HealthProbeBindAddress` and leader-election settings,
and **no `Cache` option**. `cache.Options{}` is therefore the zero value: no
namespace restriction, no label selector, no field selector. Each `Owns()` forces an
informer for its GVK; each cached `Get`/`List` starts one lazily.

| kind | where the informer comes from |
|---|---|
| `Secret` | `Owns()` at `slapdcluster_controller.go:1719`, plus ~16 cached `Get` sites |
| `Service` | `Owns()` at `slapdcluster_controller.go:1718` |
| `StatefulSet` | `Owns()` at `slapdcluster_controller.go:1717` |
| `Job` | `Owns()` at `slapdbackup_controller.go:480` |
| `Pod` | `List` at `job_reap.go:140`, `writerotation.go:156`, **and `slapdcluster_controller.go:492`** |

The report named two Pod `List` sites; there are three.

All three Pod lists already restrict by namespace and labels **in the query**. That
is the detail that misleads: a filtered cached `List` filters the *cache contents*.
It does not narrow the informer. The watch stays cluster-wide.

## 3. Root cause

The proximate fact is "no `Cache` options". That is not the root cause.

**The operator has no declared boundary of what it observes, so the boundary is the
union of its read sites.** Each `Owns()` and each cached `Get` is a local decision
with a global, invisible cost, and nothing in the source states the result.

The proof is in the tree. `database_resync.go:47-53` reasons the cost through
explicitly, and gets it backwards:

> A Secret watch would make it immediate, and is a fair alternative rather than an
> unworkable one: the obvious objection (caching every Secret in every watched
> namespace to observe one key per database) applies to a naive watch, **not to a
> label- or field-scoped cache**. That buys a selector the operator then has to own
> and keep correct on every Secret it cares about, against four lines here.

The author identified the right fix shape and declined it as not worth the cost —
three lines away from an `Owns(&corev1.Secret{})` that had already bought the entire
cost. The five-minute resync interval was chosen to avoid an expense that was
already being paid.

### The class

This project already names the neighbouring class: *"an event stream can carry a
second, undocumented job"* (`reconcile-loop-fixes.md`, 2026-08-26). This one is
different and needs its own name:

> **The scope is a declaration nobody made.** The default is everything, widening it
> is implicit and local, and the absence of a statement is the defect.

It belongs to the family this repository keeps re-meeting: *no scope declared* and
*cluster scope chosen deliberately* are indistinguishable in the source, exactly as
"nothing found" and "nothing looked for" are indistinguishable in a check.

### A second, independent cause

128Mi was sized against a cluster where that union happened to be small. **The
default encodes an assumption about somebody else's cluster**, and remains wrong by
luck after any fix to the first cause.

## 4. Why the obvious fix does not survive an upgrade

The report proposes labelling what the operator creates and filtering the cache on
the label. The discriminator has to be built first — and today the operator labels
almost nothing it creates. Across `slapdcluster_controller.go` there is exactly
**one** `Labels:` in a built `ObjectMeta`, the pod template's. The Secrets, the
Services, the StatefulSet and the ConfigMaps carry none.

That is survivable. This is not:

**A label-scoped cache over create-only state deadlocks on upgrade.**

`reconcileSecret` (`slapdcluster_controller.go:625-647`) is read-then-create-if-absent,
ending in `r.Create`. `<dbname>-credentials` has the same shape
(`slapddatabase_controller.go:651ff`). Point a label-scoped operator at an existing
cluster whose Secret predates the label:

1. the cached `Get` returns `NewNotFound` — *source-confirmed*,
   `sigs.k8s.io/controller-runtime@v0.25.2/pkg/cache/internal/cache_reader.go:70-77`,
   byte-identical to genuine absence;
2. the operator generates a fresh password and calls `r.Create`;
3. the API server answers `AlreadyExists`.

Every pass. Forever. **The operator cannot label an object it cannot see.**

It fails loudly rather than silently, which is the better direction by this
project's own precedent — but it is not a direction anyone chose, and it breaks
every pre-existing cluster at the moment of upgrade.

*Status: argued from source, not run. The experiment that settles it is in §7.*

## 5. Sequencing: an open defect makes this more dangerous than it looks

`requeueAfterFor` (`writerotation.go:108-117`): a cluster that is `Running` with
`replication.enabled: false` gets base `0` — **no periodic requeue at all**. For
such a cluster, watch events are the only thing that ever reconciles it.

This is the 2026-08-26 ledger entry in a new costume. That entry closes:

> **Fix, when someone takes it:** make the resync explicit — give the
> `SlapdDatabase` and `SlapdSchema` controllers their own periodic requeue sized for
> drift correction — *then* filter the watch. **Prerequisite, not alternative.**

That prerequisite is **half satisfied**:

- `SlapdDatabase` has `databaseResyncInterval` (5 min) — landed since.
- `SlapdSchema` has **no** periodic requeue; only 10s on error paths.
- `SlapdCluster` has **none** when Running and non-replicated — recorded as open,
  unintended and uncovered by e2e at `BACKLOG.md:1885`.

Cache scoping is not watch filtering, but it narrows the same event stream. **The
ledger has already ruled that this ordering is a prerequisite**, and the ruling
stands until someone overturns it deliberately.

## 6. Proposed fix

Four pieces, each standing alone.

1. **Stop caching Secrets entirely.**
   `client.CacheOptions.DisableFor: []client.Object{&corev1.Secret{}}`
   (`controller-runtime@v0.25.2/pkg/client/client.go:89-91`) **and** drop
   `Owns(&corev1.Secret{})`. Both are required: `DisableFor` routes the client's
   reads to the API server, while the `Owns` would otherwise keep the informer alive
   for its watch.

   *Why this beats labelling:* it covers create-only operator Secrets too, so §4's
   deadlock never arises, and there is no selector for anyone to keep correct.
   *Cost:* one API `GET` per Secret read. With a five-minute resync and a handful of
   databases, negligible.
   *What it loses:* the "someone deleted `<name>-config-password`" event — material
   only on a Running non-replicated cluster, which is §5.

2. **Label-scope the Pod cache.** Safe here and nowhere else: Pods come from a
   template the operator owns, already carry `app.kubernetes.io/name=slapd`, all
   three `List` sites already filter on exactly those labels, and there is no
   create-only read on a Pod, so §4 does not apply.

3. **Raise the chart's memory default, and say what it scales with.** Independent of
   1 and 2, and wrong today regardless of them.

4. **Leave `Service`, `StatefulSet` and `Job` alone until measured.** They are
   owner-referenced and operator-created, but labelling them reintroduces §4's
   bootstrap problem on a different object. Do not pay that on an argued cost.

## 7. What is argued rather than measured

**The 553Mi was not reproduced here, and the attribution across the five kinds is
argued.** The reporter's reasoning is sound from first principles and "Helm release
Secrets run to megabytes each" is very likely the dominant term — but *likely* is
not *measured*, and which informer dominates decides whether piece 1 alone closes
the issue.

The cheapest second route, needing no profiler and no operator change:

```bash
kubectl get secrets -A -o json | wc -c    # lower bound on the Secret informer
kubectl get pods    -A -o json | wc -c    # and on the Pod informer
```

A `/debug/pprof/heap` would be better; the operator does not expose pprof today.

§4's deadlock is likewise argued from source. One envtest — create an unlabelled
Secret, start a label-scoped cache, assert the `AlreadyExists` loop — settles it,
and is the red-first test if that route is ever taken.

## 8. Alternatives rejected

| option | why rejected |
|---|---|
| `Cache.ByObject` label selectors on Secrets (as reported) | §4: deadlocks on upgrade over create-only Secrets. Would need every one of ~16 Secret reads audited *and* a guard stopping the next cached `Get` from silently reading absence |
| `Cache.DefaultNamespaces` | Not viable as the end state: slaptain cannot know its namespaces — a mesh operator serves whatever namespaces users create. Reasonable as an opt-in knob, not as the fix |
| Read only *user-supplied* Secrets through the API reader | Half the problem. The create-only operator Secrets are exactly the ones §4 breaks, and they are not user-supplied |
| Raise the memory limit and stop there | A workaround presented as a fix. Memory still scales with the cluster; the next user with 200 namespaces reopens this |
| Label everything, all five kinds, in one change | Maximum blast radius on an argued cost, and §4 applies to every create-only object it touches |

## 9. Blast radius

**Changes:** ~16 cached Secret `Get` sites become API reads. The Pod informer
narrows. The chart's default limit moves.

**Does not change:** SSA writes never consult the cache, so every `Apply` of a
Service, StatefulSet or ConfigMap is untouched. RBAC is unchanged — the operator
already has the verbs. Persistence, reconcile cadences and requeue intervals are
unchanged unless §5 is taken deliberately.

**No event we act on today is lost by dropping the Secret watch.** `Owns()` installs
`EnqueueRequestForOwner(..., OnlyControllerOwner())` — *source-confirmed*,
`controller-runtime@v0.25.2/pkg/builder/controller.go:340,344` — so a user-supplied
TLS, S3 or kubeconfig Secret never enqueued a reconcile in the first place. Only the
owner-referenced ones did.

## 10. Open questions for the decision

1. **Rule on the no-resync gap (`BACKLOG.md:1885`) before narrowing anything?**
   Recommended: yes. It is the ledger's own stated prerequisite, it is half
   satisfied, and piece 1 removes an event stream a standalone cluster depends on.
2. **Secrets: `DisableFor` or label-scope?** Recommended: `DisableFor` — it removes
   the class rather than managing it, and it is the only one that survives an
   upgrade.
3. **Scope: Secrets and Pods only, or all five kinds?** Recommended: Secrets and
   Pods, then measure.
4. **An ADR?** Recommended: yes. *What the operator caches, and what it must
   therefore never read from the cache* constrains every future call site, which is
   this project's stated test for an ADR. Without it the next contributor adds a
   cached Secret `Get` and nothing goes red.

## Related

- `docs/reconcile-loop-fixes.md`, 2026-08-26 — filtering the `SlapdCluster` watch
  stopped per-pod convergence; the prerequisite this analysis inherits.
- `docs/BACKLOG.md:1883-1896` — the resync gap, open.
- ADR-002 and its 2026-08-26 amendment — `cn=config` convergence is event-driven,
  and what that implies for anything that narrows an event stream.
- ADR-025 — unreadable evidence never counts as absence. §4 is that rule meeting a
  cache.

---

## Amendment, 2026-10-29: what the implementation changed about this analysis

PR #29 implements the fix as **ADR-042**, and it is a better design than §6. Recorded
here because a document that keeps recommending what the merged code deliberately does
not do is the same drift between two records that caused the original defect (§3).

### §6 piece 1 is withdrawn

§6 recommended dropping `Owns(&corev1.Secret{})` and reading every Secret uncached.
That is ADR-042's **option 2**, and the ADR rejects it on a case this document raised
and then failed to apply to its own proposal:

> a deleted `<cluster>-config-password` would be recreated only on the next reconcile,
> and a Running standalone cluster has no periodic requeue (reconcile step 7), so it
> would wait for an unrelated event. The watch is worth keeping; it only has to be
> narrow.

That is §5's own finding, turned around. §5 treated the missing periodic resync as a
**prerequisite to fix before narrowing anything**; ADR-042 keeps the watch and labels
the objects instead, so no event stream is lost and **the prerequisite never arises**.
`BACKLOG.md:1885` stays open on its own merits and is no longer sequencing-critical to
this work.

The lesson against this document: §5 correctly identified that narrowing an event
stream is the hazard, then §6 proposed narrowing one anyway. Finding a hazard and
routing the proposal around it are two steps, and only the first was taken.

### §4 was right, and is answered rather than avoided

The upgrade deadlock over create-only state is real, and ADR-042 does not sidestep it:

- **Secrets** are read through the API reader at all 15 call sites, so `reconcileSecret`
  finds the existing Secret and §4's `AlreadyExists` loop cannot start. An owned but
  unlabelled Secret is then patched by `labelOwnedSecret`, which refuses to label
  anything whose controller reference is not this cluster.
- **StatefulSets and Services** use `managedGet` — cache, then one uncached GET on
  NotFound. ADR-042 names the two concrete failures this document had only described
  in general: `volumeClaimTemplates` re-asserted against an existing StatefulSet so the
  labelling apply never lands, and a `<cluster>-write` Service frozen on one pod never
  widening.

### §3's root cause stands, and the implementation closes it

"The scope is a declaration nobody made" is answered directly: `CacheOptions()` is that
declaration, in one place, and `managedLabels()` is the single definition of what the
operator claims. `database_resync.go`'s comment — this document's evidence for the root
cause — was itself amended in the PR rather than left to contradict the new behaviour.

### What is still open after the implementation

Raised in review on PR #29; none is a redesign:

1. **The guard is a convention, not a mechanism.** `client.Client` satisfies
   `client.Reader`, so a future `r.Get` of a Secret compiles, passes the unit suite and
   finds nothing in production — the exact failure the PR fixes. Adding
   `client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}` to the manager's
   **client** options makes it impossible rather than discouraged, and does **not**
   disturb the `Owns()` watch: `DisableFor` governs the client's reads, the informer
   belongs to the cache. This is §6 piece 1 returning in the only form that still has a
   job — as a structural guard beside the label filter, not as a replacement for it.
2. **Jobs get no `managedGet` fallback** (`slapdbackup_controller.go:181`,
   `dirjob.go:112`, `slapdcluster_restore.go:462`). Benign — Jobs are TTL'd under
   ADR-018 and the failure is loud and self-healing — but the asymmetry with Services
   and StatefulSets is undocumented.
3. **The e2e has not run**, by the author's disclosure. Not a replication-path change,
   but it changes how the ADR-027 replication credential is read on every per-pod
   convergence.

### What §7 said and what the implementation measured

§7 recorded that the 553Mi was not reproduced here and the attribution across the five
kinds was **argued**. ADR-042 measured it: 96 Secrets of 768 KiB (72 MiB, Helm release
records) in a namespace the operator has no business in cost **+97.5 MiB of heap**
before and **+1.6 MiB** after. The attribution to Secrets was correct, and is now
measured rather than argued.
