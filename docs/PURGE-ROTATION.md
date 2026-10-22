# The Accesslog Purge Rotation

How to stop a provider's own accesslog purge from stalling its own writes, and
what your clients have to do before it helps you.

The design record is
[ADR-037](adrs/adr-037-purge-off-the-write-path.md); this is the user-facing
half. Where a tunable *lives* — which field is on which resource, and why — is
[`TUNING.md`](TUNING.md).

> **Experimental (ADR-039 tier 2). Read this before turning it on.**
>
> This is new, it is off by default, and the project does not yet claim it is
> fit for production. What *is* claimed: it is implemented, it is measured, and
> the operator reports honestly what it did. What is not claimed: that anybody
> has run it long enough to know how it behaves in your deployment.
>
> Three things make it unlike a conventional slapd deployment, all deliberate:
>
> - **It needs a patched slapd.** The per-pod purge *phase* is a third value of
>   `olcAccessLogPurge` that upstream OpenLDAP does not have; it is our patch
>   (`images/openldap-deb/`). A stock build refuses the three-value form, the
>   operator falls back to the two-value one, and
>   `AccesslogPurgeConverged=False/PurgePhaseUnsupported` names the version you
>   need. Nothing breaks — but nothing rotates either.
> - **It couples your load balancer to the database's internal schedule.**
>   Rotating the write target of a multi-provider cluster is ordinary, supported
>   OpenLDAP; doing it *in step with each provider's purge* is not something a
>   conventional on-VM deployment does, and there is little field experience to
>   draw on anywhere.
> - **Its benefit is conditional on your clients**, as the next section
>   explains. A deployment that turns this on without short-lived write
>   connections measures WORSE than one that leaves it off.
>
> Known use cases are narrow: migrations, and bulk provisioning where write
> clients are short-lived. **Feedback is wanted, positive or negative** — open
> an issue with what you ran and what you measured. The arms nobody has run are
> listed in ADR-037's seventeenth amendment.

---

## Read this first: the feature only pays for clients that re-dial

A Kubernetes Service steers **new** connections; it never re-wires an
established one. A pool that holds a connection for the lifetime of the process
keeps writing to the pod it first reached, whatever the endpoint now says.

That is not a caveat. It is the **precondition**. A deployment that turns the
rotation on without meeting it gets a measurably *worse* result than leaving it
off: with the writes following the endpoint and no lead time, a two-provider
cluster lost 2.8 % of throughput and doubled its dropped iterations against the
same rotation with the writes pinned — 99.9 % of that loss inside 30 s of a slot
boundary, 69 % of it inside the client's own connection lifetime.

So the client must bound its maximum connection lifetime `τ`, and the deployment
must satisfy

```
τ  ≤  leadTime  ≤  (replicas − 1) × slotLength − P

                   P = write_rate × replicas × slotLength / delete_rate
```

At **two** read-write pods — the only shape anyone has measured, and the one
every figure in this document comes from — that upper bound reduces to
`slotLength − P`, which is the form used from here on. It is the *reduction*,
not a different rule.

The lower bound is **the client's**: a lead shorter than the connection lifetime
leaves connections pointing at a pod that is about to purge. The upper bound is
**the server's**: a lead longer than `slotLength − P` hands writes to a pod that
has not finished purging its own log.

Both bounds move with the write rate, and the upper one closes as it rises.
**Neither is checked by the operator**, because both need numbers only the
deployment has — see [Sizing](#sizing-is-arithmetic-you-have-to-do-yourself).

Writes sent anywhere else stay perfectly legal: every read-write pod is a
provider and accepts a write. The endpoint is a **recommendation, not an
enforcement** — ignoring it costs you exactly the stall this feature removes,
and nothing else.

---

## The problem: a purge blocks the writes of the pod that runs it

`accesslog_purge()` collects every expired journal entry in one search and
deletes them in an unpaced loop. Each delete takes the accesslog environment's
single LMDB writer — and every client write appends its own log entry to that
same environment. The writer is exclusive, so **a provider's purge blocks that
provider's writes** while its reads keep flowing at around 75 %.

Measured on a 2+2 cluster at 100k ops/s, one hour per arm, with `cn=config` set
asymmetrically:

| | purge on the write target | purge on the peer only |
|---|---|---|
| write stall per cycle | 30 s | **0 s** |
| iterations dropped per cycle | 969,786 | **0** |
| peak p99 | 3,384 ms | **1.3 ms** |

Two things follow, both measured rather than reasoned:

- **Scheduling alone cannot help.** The fraction of wall clock a provider spends
  purging is `write rate / delete rate`, and the interval does not appear in it:
  halving the interval halves each batch and doubles the frequency. Shortening
  it was tried — 15 s against a 5-minute baseline cost 18× the disk reads, took
  p99 from 2.1 ms to 143–230 ms, and dropped 2.2 % of the load for no gain in
  throughput.
- **Staggering alone changes nothing a client can see.** De-synchronising the
  providers' purges lands on the synchronised arm and nowhere near the arm where
  the write target did not purge. The stagger buys a pod that is *not* purging
  for the write endpoint to sit on — a prerequisite, not the benefit.

The only lever that works is moving the purge off the write path, which is what
this feature does.

---

## Prerequisites

| | Why |
|---|---|
| A slapd image carrying the purge-phase patch — `2.7.1-0+slaptain2` or later (`slapd -VV`) | The per-pod phase is a **third field** of `olcAccessLogPurge` and is ours, not upstream's. A stock slapd rejects the three-field value outright |
| `spec.replication.enabled` with `deltaSync`, and `spec.replicas ≥ 2` | A pod with no change journal has nothing to purge, and with one pod there is no pod to rotate to |
| Clients that re-dial — see [above](#read-this-first-the-feature-only-pays-for-clients-that-re-dial) | A Service does not move an established connection |

A pod whose slapd refuses the phase keeps a **bounded** journal: the operator
writes the two-field `<retention> <interval>` value instead and reports it per
database as `AccesslogPurgeConverged=False/PurgePhaseUnsupported`. The feature
degrades to "no rotation", never to "no purge".

---

## Turning it on

The schedule is the **cluster's**, declared in exactly one of two mutually
exclusive forms. What each database **keeps** is still the database's.

```yaml
# SlapdCluster — WHEN to purge, in exactly one of two forms

spec:
  accesslogPurge:
    interval: 24h                 # form A: one schedule, every pod together

spec:
  rotation:                       # form B: staggered, plus the write endpoint
    enabled: true
    slotLength: 6h                # required when enabled; NO default
    leadTime: 180s                # how early the endpoint moves; unset = 0
    slotsFromWriteToPurge: 1      # unset = 1; leave it alone unless measuring
```

```yaml
# SlapdDatabase — WHAT to keep
spec:
  replication:
    accesslogPurge:
      enabled: true
      retention: 24h
```

**Setting both cluster forms is refused**, never silently resolved by
precedence: `RotationConfigured=False/ScheduleDeclaredTwice`, naming both
fields. The operator's default 24h interval stays in force meanwhile, so nothing
goes unpurged while you work out which one you meant.

Through the charts — `charts/slapd`:

```yaml
replicas: 3
replication:
  enabled: true
rotation:
  enabled: true
  slotLength: "6h"
  leadTime: "180s"
```

A mesh — `charts/slapd-mesh`, under `cluster:`, applied unchanged at every site:

```yaml
cluster:
  replicas: 2
  rotation:
    enabled: true
    slotLength: "6h"
    leadTime: "180s"
```

`charts/slapd-mesh/examples/rotation.yaml` is that block as a stackable overlay
(`-f examples/three-site.yaml -f examples/rotation.yaml`).

The values name no site, no endpoint and no pod — a schedule is a *shape*, and
every site derives the same grid from the same `replicas`. The grid is
nonetheless per **site**, over that site's own read-write pods: a write sent to
another site lands where the client sent it, which is a mesh accepting writes at
any provider rather than a gap in the rotation.

Opt-in and off by default. Nothing about an installed cluster changes until you
set it, and a chart that says nothing about rotation renders byte-identically to
one from before the feature existed.

---

## The grid, and the parameters

With `n = spec.replicas` read-write pods and a slot length `t`:

```
purge interval   T        = n × t          derived, never declared
pod k purges at  phase    = k × t          derived, nobody declares it
the writer moves every    t,  one leadTime early
```

Both the interval and the phase are counted from the **Unix epoch**, so nothing
drifts and a restarted pod rejoins its own slot at the next boundary.

| Parameter | Where | Default | Who owns the number |
|---|---|---|---|
| `slotLength` (`t`) | `SlapdCluster.spec.rotation` | **none — required when enabled** | You. ADR-037 fixes no value on purpose; a default would be a schedule every silent cluster had accepted without choosing |
| `leadTime` (`d`) | `SlapdCluster.spec.rotation` | `0` — the endpoint moves *at* the boundary | You. It must beat your clients' connection lifetime, which the operator cannot see |
| `slotsFromWriteToPurge` (`v`) | `SlapdCluster.spec.rotation` | `1` | Leave it unset unless you are measuring |
| `retention` | `SlapdDatabase.spec.replication.accesslogPurge` | `168h` (7 days) | You, per database |
| `interval` (`T`) | **derived** as `n × t` | — | Nobody. Under rotation there is nothing left to declare |

`n` is `spec.replicas` — **declared, never observed**. It fixes `T`, and `T` is
persisted into every pod's `cn=config`: deriving it from readiness would rewrite
the whole fleet's configuration on one transient restart. Readiness belongs in
writer *selection*, which persists nothing.

`t` is fixed and `T = n·t` varies, not the reverse. Scaling then leaves every
existing pod's phase where it was and gives the new pod `n·t`; only the interval
moves, and changing an interval does not move a phase.

### `slotsFromWriteToPurge`

A **signed** displacement in slots from a pod's write slot to its own purge
slot, taken mod `n`. `+1` is "purge the slot after writing" — the default, and
the configuration ADR-037 prefers: the writer hands off and then immediately
works its journal down. `-1` is the other experiment, "purge the slot before
writing", and is the same answer as `n−1` — which is the point of the sign.
`2` means "the slot before" only until somebody adds a pod; `-1` keeps meaning
it.

The rule is `v mod n ≠ 0`, not a minimum: zero **and every multiple of `n`** put
the write on the pod that is purging, so `3` on a three-pod cluster is as wrong
as `0` and looks far more reasonable. The rule needs `spec.replicas`, which no
CRD marker can see, so the operator reports it
(`WriteSlotCollidesWithPurge`) — naming the slot the value *resolved to* rather
than echoing what you typed.

When the preferred pod is not eligible, the fallback **degrades in the direction
of the sign**, away from the purge: `-1` degrades to `-2`, not to `+1`, so a
choice that cannot be honoured stays near what its author meant.

### `leadTime`

How long **before** each slot boundary the endpoint moves to the pod that will
hold the next slot. It exists because the handover is where the entire cost is.

With no lead the endpoint moves *at* the boundary, so the pod that stops taking
writes begins purging at the same instant and every connection that has not aged
out delivers its writes into an active purge — every slot, by construction.

Measured, `n = 2`, `slotLength` 600 s, retention 15 m, client connection
lifetime `τ` 60 s, 100k ops/s offered, 75-minute arms, same binary the same
afternoon, one variable:

| arm | achieved | dropped | worst write latency |
|---|---|---|---|
| `leadTime: 0` | 93,686/s | 16,044,943 | 137,626 ms |
| `leadTime: 180s` | **99,245/s** | **57,167** | **2,314 ms** |

281× fewer dropped iterations, 59× lower worst-case write latency, and modify
p99 flat at 1.9–2.9 ms for the hour. The per-boundary stall is **gone, not
smaller**: folded onto the slot, the unleaded arm loses 100 % of its drops in one
contiguous 150 s block after each boundary, and the leaded arm has no boundary
structure at all.

It is a **lookahead and only a lookahead**. The purge grid is untouched — the
interval stays `n × t` and pod `k` still purges at `k × t` from the epoch — so
setting or clearing a lead never rewrites a pod's `olcAccessLogPurge` and never
rolls a pod. `leadTime: 0` is byte-identical to the behaviour of every cluster
that predates the field.

---

## A lead does not remove the cost — it moves it

Say this to yourself before you size anything. The purge still happens. It
happens on the pod that is **not** taking writes, and that pod's syncrepl replay
stalls while it deletes.

What the rotation removes is the stall **from the write path**. The residue is
**replication lag**. In the measured arm above, the `contextCSN` spread
alternated between 0.01 s and 96–150 s on the slot rhythm, and recovered
unaided every cycle.

The sizing consequence is direct: **retention must exceed the purge duration by
a comfortable margin**, or a consumer falls out of the retention window while
the provider it reads from is busy deleting. At 15 m retention against ~140 s of
lag, it did.

---

## Sizing is arithmetic you have to do yourself

The operator will not refuse a rotation whose phases are too long for your write
rate, and will not shorten one to keep a purge small. Three numbers are yours:

| symbol | what it is | how you get it |
|---|---|---|
| `τ` | your clients' maximum connection lifetime | your client's pool configuration |
| `w` | the write rate reaching one provider's journal | your own load, measured |
| `r` | the **delete** rate of one provider's storage | measured, at *your* purge size — see below |

### Do not take a delete rate from this document

`r` measured **~2,400–2,600/s** under load by the server's own timing line,
across purges from 4,400 to 187,000 entries — a 42× range with no decline in
it. A single coarser reading on a 353,000-entry purge suggested a fall, but it
came from a different instrument whose duration is bounded rather than
measured, and its range overlaps the others. **Whether `r` falls at production
purge sizes is unmeasured**, and the two instruments have never been run
against the same purge.
If `r` degrades as purges grow, then `P` grows **faster than linearly** in the
interval, so an `r` taken from a smaller purge flatters a larger one.

Measure `r` at your own purge size. The arithmetic below is transferable; the
number is not.

### The capacity condition

Every provider logs every write, including the ones it receives by syncrepl, so
every journal grows at the full write rate while only the inactive providers may
purge. With an even rotation each is inactive `(n−1)/n` of the time:

```
w  ≤  r × (n−1)/n
```

Above that bound the logs grow without limit no matter how the purges are
scheduled, and the scheme does not apply.

### Purge duration

```
P  =  w × n × t / r        = w × T / r
```

### The lead window, and that it closes as the write rate rises

**This is the most important number in this document.** The feasible band for
`leadTime` is

```
τ  ≤  d  ≤  (n−1)·t − P          and at n = 2:          τ  ≤  d  ≤  t − P
```

The general form is the left one; the right is its `n = 2` reduction, and
`n = 2` is what has been measured. ADR-037 derives the slot count once, under
**Notation** at the top of that document: with `v = +1` the incoming writer's
previous purge began `n` slots back, so the span the lead reaches into is
`(n−1)·t` and what is left of it must still cover `P`. The table and the floor
below are worked at `n = 2`.

A lead is feasible exactly when the cluster is inside its purge capacity — it
adds no constraint a deployment did not already have. What it adds is a **floor
on the slot length**:

```
t  ≥  τ / ( (n−1) − n·w/r )
```

At the bottom of a measured delete rate that floor can approach the slot length
itself, which is where the margin actually lives.

The window closes as the write rate rises. Two columns below, at `t = 600 s`:
one computed at the **assumed** `r ≈ 2,000/s` this document carried before the
rate was measured, one at the **measured** `r ≈ 2,500/s`. The gap is the point.
A 25 % error in a rate you have to measure yourself is the difference between a
workable lead and an impossible one — at 300k the assumed rate says no lead
exists and the measured rate says 168 s is comfortable.

The table below is indexed by *offered* rate because that is what the arms were
driven at, but **`w` is the write rate, not the offered one.** The two move
together only while the mix is fixed. A write-heavier mix at an unchanged
offered rate raises `w` by the same arithmetic and closes the window just as
surely — so a cluster left sitting with a lead sized for one workload can lose
the protection when the workload changes shape, with the offered rate never
moving.

| offered | `w` | max `d` at assumed `r = 2,000` | max `d` at measured `r = 2,500` |
|---|---|---|---|
| 100k | 300 | 420 s | 456 s |
| 200k | 600 | 240 s | 312 s |
| 250k | 750 | 150 s | 240 s |
| 280k | 840 | 96 s | 197 s |
| 300k | 900 | 60 s — equals `τ`; the window is **empty** |

So a lead of 180 s is correct at 100k and **infeasible above about 220k**.

**The failure mode to recognise:** a deployment that sets a lead once and then
grows its write rate loses the protection *silently*, and the symptom is the
unleaded failure returning — the per-boundary stall, the dropped writes, the
multi-second worst-case latency. That reads as *"the feature does not work"*
when the truth is *"the lead is now mis-sized"*. Re-derive `d` whenever your
write rate moves.

### A note on the inequality's upper bound

`t − P` above is the `n = 2` reduction of `(n−1)·t − P`, as ADR-037's Notation
section derives. **At three providers or more they differ**, and nothing has
been measured there: size against the general form, and read `t − P` as the
conservative one.

The operator checks neither bound. Its one enforced bound is
`d < (n−1) × slotLength`, reported as `LeadTimeOutOfRange`: at or beyond that
the lead reaches back past the instant the incoming writer *started* purging, so
the endpoint would hand writes to a pod that purges during its own write slot —
nonsense independent of any measurement. Everything else about the value is
yours.

---

## What to observe

### The write endpoint

Turning the rotation on publishes a second client Service, `<cluster>-write`,
selecting exactly one read-write pod by `statefulset.kubernetes.io/pod-name`. It
answers on the same ports as `<cluster>` and honours `spec.service.type`, so a
client that dials `<cluster>:389` today writes to `<cluster>-write:389` instead
and changes nothing else. Reads stay on `<cluster>` (or `<cluster>-readonly`):
spreading them is the point of having providers.

```bash
kubectl get svc -n <ns> <cluster>-write \
  -o jsonpath='{.spec.selector.statefulset\.kubernetes\.io/pod-name}{"\n"}'
```

**Turning the rotation off** leaves the Service in place and **widens** it to
the whole read-write fleet, so a client still following it keeps writing. It is
not deleted — clients were told to follow that name, and deleting it would turn
a configuration change into a write outage at every client that took the advice.
It is not left pinned to one pod either, because with the rotation off nothing
phases that pod's purge any more and every write would land on a provider that
purges. A cluster that never rotated never grows the Service at all.

### The conditions

```bash
slctl status -n <ns> <cluster>
```

`RotationConfigured` on the **SlapdCluster** — reported, never refused, so a
schedule you cannot honour never stops a running directory reconciling. A faulty
rotation block withholds the **rotation and nothing else**: the cluster keeps
purging on the default 24h interval.

| Reason | What it means |
|---|---|
| `RotationActive` | the grid is in force; the message states the interval and the spacing |
| `RotationDisabled` | `enabled` is unset or false — the good case, not a fault |
| `SlotLengthMissing` | `enabled: true` with no `slotLength`. There is no default on purpose |
| `SlotLengthNotPositive` | `0s` passes admission and would put every pod's phase at zero — the synchronised purge the feature exists to remove |
| `SlotLengthNotWholeSeconds` | `500ms` passes admission and has no representation in `olcAccessLogPurge`, whose fields are `[ddd+]hh:mm[:ss]` |
| `WriteSlotCollidesWithPurge` | `slotsFromWriteToPurge` resolved to slot 0 — the pod that is purging |
| `LeadTimeOutOfRange` | `leadTime` is negative, or at or beyond `(replicas − 1) × slotLength` |
| `ScheduleDeclaredTwice` | `spec.accesslogPurge.interval` and `spec.rotation.enabled` both declare *when* the purge runs. Delete one |
| `PurgeIntervalNotUsable` | the form-A `interval` is non-positive or finer than a second |

`WriteEndpointRotating` on the **SlapdCluster** — observation only; it gates
nothing and withholds nothing.

| Reason | What it means |
|---|---|
| `WriterSelected` | names the pod the endpoint selects and the instant it moves next. With a `leadTime` in force that instant is the **endpoint move**, one lead before the slot boundary, and the message says so |
| `NoEligibleWriter` | no pod could be chosen this pass — none Ready, or readiness could not be read at all. The selector is left exactly where it was and writes keep going there, because a Service selecting nothing is a total write outage. Nothing was repointed, and the message says so |

`AccesslogPurgeConverged` on each **SlapdDatabase**:

```bash
kubectl get slapddatabase -n <ns> <db> \
  -o jsonpath='{range .status.conditions[?(@.type=="AccesslogPurgeConverged")]}{.status}{" "}{.reason}{"\n"}{.message}{"\n"}{end}'
```

| Reason | What it means |
|---|---|
| `Converged` | every pod that was read carries its own phase on the cluster's grid |
| `RotationDisabled` | the cluster is not rotating; every pod carries the same interval |
| `RotationNotInForce` | the cluster **declares** `spec.rotation` and the gate refused it. Journals are bounded, nothing is staggered; go and read `RotationConfigured` |
| `RetentionNotUsable` | `retention` is non-positive or finer than a second. **Reported, and the operator's 7-day default written anyway** — a mistyped duration must not become an unbounded journal |
| `PurgeDisabled` | `accesslogPurge.enabled` is `false`. No purging on this journal at all; the growth is yours |
| `PurgePhaseUnsupported` | a named pod's slapd refused the phase. That image carries no purge-phase patch; the operator wrote the two-field value instead, so the journal stays bounded and that pod simply does not take its phase |
| `NoReachablePod` | nothing could be read. `Unknown`, never `False` — unreadable evidence is not evidence |

A pod that could not be read is named in the message and never counts against
the verdict.

### The value slapd actually carries

`olcAccessLogPurge` lives on the accesslog overlay in `cn=config`, so it needs
`--as config` — the data rootDN cannot read it:

```bash
slctl ldapsearch -n <ns> --as config --pod 0 \
  -b cn=config '(objectClass=olcAccessLogConfig)' olcAccessLogPurge
```

Under rotation each pod carries a value nobody typed, with a **distinct third
field per pod** and every database on one pod sharing that pod's phase:

```
slapd-0   olcAccessLogPurge: 7+00:00:00 18:00:00 00:00:00
slapd-1   olcAccessLogPurge: 7+00:00:00 18:00:00 06:00:00
slapd-2   olcAccessLogPurge: 7+00:00:00 18:00:00 12:00:00
          retention ^        interval ^ phase ^
```

Databases **align**, they do not spread: a pod purges all of its journals at its
own phase, concentrating its disturbance into the one window where it is not
taking writes.

---

## Measuring it yourself

- **Lower `spec.logLevel` to `16384` for any run where purge timing matters.**
  At the default `16640` the stats logging buries the purge line at high rates
  and `kubectl logs` reaches back only seconds. `16384` is `LDAP_DEBUG_SYNC`
  without `LDAP_DEBUG_STATS`. Note this **rolls the pods** — slapd takes `-d` at
  startup only.
- **The `contextCSN` spread peak is a usable proxy for `P`** when the log line
  is unreadable: observed 139 s against 136 s predicted, 2 % apart.
- Purge timing, where the line is readable, reads as
  `etime=5.402305 nentries=803` — elapsed and entries removed, which is `r`
  directly.
- **Wait for the endpoint to converge before you dial.** A fresh connection
  opened 16 ms after the selector was patched still reached the **old** pod:
  that is kube-proxy's propagation delay, not a client failing to follow. A
  measurement that does not wait for it reads as a failure of the whole design
  when it is nothing of the kind.

---

## The limits, stated rather than buried

- **`n = 2` is the sharpest case, and it is the one that has been measured.**
  With `v = +1` the incoming writer *is* the currently-purging pod, so a lead
  necessarily overlaps the tail of its purge by `d`. That is correct and
  bounded, and tighter than anything with a third provider — but it also means
  the only shape anyone has numbers for is the hardest one.
- **The phase needs our patch.** A stock slapd cannot express one. On an
  unpatched image the rotation degrades to a synchronised purge with a bounded
  journal and says so per database.
- **Short slots are measurement geometry, not a production shape.** OpenLDAP's
  own default purges roughly daily; ADR-037 expects production-shaped
  deployments at 6 or 12 hours. The 20 s slot the e2e uses and the 600 s slot
  the load runs use exist to make a boundary reachable inside a test.
- **No `slotLength` floor is enforced.** The operator reconciles on its own
  cadence and does not chase boundaries.
- **Operator downtime degrades to today.** Purges keep firing from `cn=config`
  because the grid is absolute, but the writer stops moving, and eventually the
  writer is the pod purging. No worse than not having the feature.
- **The read endpoint is not built.** ADR-037 sketches a read Service of the
  read-only replicas plus the current writer, on a freshness argument. It has no
  measurement behind it and costs read capacity in inverse proportion to
  `readReplicas`, so it is deliberately out of scope.
- **Pacing the purge inside slapd** (`olcAccessLogPurgeBatch`) is the
  alternative this feature does not use. It is 2.7-only, never set by the
  operator, and measuring it is blocked upstream — see ADR-037 option A.

---

## Related

- [ADR-037](adrs/adr-037-purge-off-the-write-path.md) — the design record, the
  measurements and every rejected alternative.
- [`TUNING.md`](TUNING.md) — where each half of `olcAccessLogPurge` lives, and
  the migration off the old two-token string field.
- [ADR-019](adrs/adr-019-per-database-accesslog.md) — one accesslog DB per
  replicated data DB, which is why a purge disturbs one database's consumers and
  not every database's.
- [ADR-024](adrs/adr-024-tunable-placement.md) — R6: the placement argument for
  splitting the directive across two resources.
- [ADR-013](adrs/adr-013-defer-hot-database-management.md) — why `n` is declared
  and never observed.
- [`MULTI-SITE.md`](MULTI-SITE.md) — the mesh install this is applied inside.
