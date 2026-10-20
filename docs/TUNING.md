# Tuning and sizing a slaptain cluster

An unset field means slaptain's opinion, not slapd's: where upstream's default is
sized for a demo directory and we have a defensible value, we write ours
([ADR-024](adrs/adr-024-tunable-placement.md) R5). Every tunable has a placement
class that decides whether changing it converges on a running cluster, waits for the
pod's next restart, or needs the database rebuilt — and a handful of them can only
be *reported*, because slapd crashes when the operator tries to write them live. The way back to bare OpenLDAP
behaviour is asking for it (`0`, `""`, an explicit number), never leaving the field
unset.

Read this before you size anything larger than a fixture. The values that break a
large directory are structurally invisible in a small one.

## What changed in the defaults

The two tunables batches of 2026-09-12 and 2026-09-13 moved a set of values that
earlier slaptain releases left at slapd's own. Upgrading the operator changes the
behaviour of an existing cluster, in every case towards "works at scale":

| Attribute | Was (slapd's own) | Now (slaptain's default) | Why |
|---|---|---|---|
| `olcSizeLimit` | 500 entries | `unlimited` | A client enumerating a real subtree got a silently truncated answer with a result code most libraries never surface |
| `olcTimeLimit` | 3600 s | `unlimited` | The cap aborted exactly the bulk sweeps a large directory exists to serve |
| `olcLimits` on the replication identity | none (so 500) | `size`/`time` unlimited, soft and hard | A consumer's syncrepl search is an ordinary search: without the exemption a directory stopped replicating past its 500th entry while reporting itself `Synced` ([ADR-020 amendment](adrs/adr-020-accesslog-access-control.md)) |
| `olcDbMaxSize`, data DB | ~10 MB (back-mdb's) | 32Gi | A database that outgrows its map stops accepting writes with `MDB_MAP_FULL` |
| `olcDbMaxSize`, accesslog DB | ~10 MB | 8Gi | A journal that fills stops advancing — and it fills on write *rate*, not data volume |
| `olcDbIndex`, data DB | whatever `spec.indices` said | `objectClass`, `entryCSN`, `entryUUID` always present | `entryCSN` and `entryUUID` are on syncrepl's hot path; unindexed, each is a full scan |
| `olcDbCheckpoint` | unset | `1024 5` data, `2048 15` accesslog | Turning `noSync` on becomes a one-field change that is already safe |
| `olcDbRtxnSize` | 10000 | 10000, written explicitly | Same value; visible in `cn=config` and pinned against a base-image bump |
| `olcSpSessionlog` | unset (off) | 5000 operations, data DB only | A reconnect inside the window is answered from memory instead of a walk of the whole database ([ADR-022](adrs/adr-022-syncprov-sessionlog.md)) |
| syncrepl stanza | no keepalive, no timeouts | `keepalive=240:3:30`, `network-timeout=10` | An idle `refreshAndPersist` connection dropped by a stateful firewall used to look healthy indefinitely |
| `olcLogLevel` | 256 (stats) | 16640 (stats + consumer-side sync) | A replication incident is diagnosed from what slapd logged while it was going wrong |
| `olcTLSProtocolMin` | unset | `3.3` (TLS 1.2 floor) | Unset means "whatever this image's OpenSSL permits", a policy that moves on a base-image bump |
| `olcPasswordHash` | `{SSHA}` compiled in | `{SSHA}`, written | Same value, stated and converged rather than inherited from the build |
| `olcToolThreads` | 1 | 2 | `slapadd` runs during a restore with the cluster scaled to zero, so its runtime is downtime |

`olcDbNoSync` did not change value — it is still `false` — but it changed class:
editing it on a live `SlapdDatabase` used to be a silent no-op and now converges.

## The knobs

### Placement classes

Every field below is one of four ([ADR-024](adrs/adr-024-tunable-placement.md)):

- **converged** — read, compared and written on every pod on every reconcile.
  Edit the CR and it applies. This is the default class.
- **bootstrap-time** — written into a pod's generated config when its `/config`
  volume is first bootstrapped, and never again. Editing the field changes nothing
  on existing pods, and a pod bootstrapped later picks up the new value while its
  peers keep the old one.
- **startup-only** — written into a pod's config by the init container at *every*
  pod start, offline, while nothing holds the configuration open. Editing the field
  changes nothing until a pod restarts; the fleet converges one restart at a time,
  and the operator reports which pods are still waiting
  ([ADR-034](adrs/adr-034-startup-only-configuration.md)).
- **recreate-required** — written when the database is created, compared
  afterwards, and a divergence is *reported*, never applied.

The last class exists because slapd dies on the live write. An `ldapmodify` of
`olcDbMaxSize` against a running back-mdb database segfaults the process (OpenLDAP
2.7.1, exit 139, captured mid-MOD); LMDB's `mdb_env_set_mapsize` may not run with
transactions active, and a live slapd always has some. `olcDbEnvFlags` fails the
same way once `writemap` enters the list. Editing either field sets
`TunablesConverged=False` on the `SlapdDatabase`, reason `RecreateRequired`, with
the current value, the desired value and the change path in the message. The change
path is always the same: back up, delete the database, restore into a fresh one
([ADR-014](adrs/adr-014-s3-backup-restore.md)).

Bootstrap-time and startup-only read alike from the CR — in both, an edit does
nothing right now — and cost very differently. A bootstrap-time value is written
once, into a config volume being created for the first time, so two pods on either
side of an edit disagree forever and the way back is a recreate. A startup-only
value is rewritten at every pod start, so the way back is a restart.

### Per database — `SlapdDatabase`

| Field | Default | Class |
|---|---|---|
| `spec.maxSize` | 32Gi | **recreate-required** |
| `spec.envFlags` | none | **recreate-required** |
| `spec.sizeLimit` | `unlimited` | converged |
| `spec.timeLimit` | `unlimited` | converged |
| `spec.limits` | none (the replication exemption is prepended and is not yours to set) | converged |
| `spec.noSync` | inherits `SlapdCluster.spec.tuning.noSync`, itself `false` | converged |
| `spec.checkpoint` | `1024 5` | converged |
| `spec.rtxnSize` | 10000 | converged |
| `spec.indices` | none beyond the operator's baseline | converged, **additive only** |
| `spec.replication.syncprovSessionlog` | 5000 | converged |
| `spec.replication.syncprovSessionlogSource` | `InMemory` | **startup-only** |
| `spec.replication.syncprovCheckpoint` | none | converged |
| `spec.replication.accesslogPurge.enabled` | `true` | converged |
| `spec.replication.accesslogPurge.retention` | `168h` (7 days) | converged |

`spec.maxSize` takes a Kubernetes quantity (`32Gi`) or a bare byte count; the
operator converts. A value it cannot parse is an error, not a fallback.

`syncprovCheckpoint` and `accesslogPurge` are converged on every reconcile: editing
either reaches pods whose overlays already exist, and clearing the field removes the
attribute. Both were verified live-modifiable on a running slapd before being
converged at all — replace and delete, no crash, no hang — which is the precondition
ADR-024 now puts on this class. (They used to be written only at overlay creation,
so edits were silently ignored; that debt is paid.)

### `olcAccessLogPurge` has three owners, and you set two of them

`logpurge <age> <interval> [<phase>]` is one slapd directive and three quantities
with three different owners (ADR-024 R6). slaptain splits them accordingly, and the
value you read back in `cn=config` is one nobody typed:

```
olcAccessLogPurge: 7+00:00:00 18:00:00 06:00:00
                   ^retention ^interval ^phase
```

| quantity | what it decides | where you set it |
|---|---|---|
| retention (`age`) | how far behind a consumer of **this** database may fall | `SlapdDatabase.spec.replication.accesslogPurge.retention` |
| interval | how often the purge cost is paid — and the cost is the **pod's**, since all of a pod's databases purge in one window | `SlapdCluster.spec.accesslogPurge.interval`, or derived from `spec.rotation` |
| phase | which pod pays it when | derived per pod; nobody declares it |

So: **what to keep is the database's, when to purge is the cluster's.** A database
cannot declare an interval, which is what makes a mode-dependent field — and the
outage it generated — impossible rather than merely discouraged.

`enabled` sits beside the *action*, not beside the duration, on purpose:
`accesslogRetention: {enabled: false}` would read as "no retention — purge
everything", and here it has to mean the opposite. `enabled: false` is "never purge,
keep forever", and you take responsibility for the growth. It replaced the old
`"none"` sentinel.

The cluster declares the schedule in exactly one of two forms:

```yaml
# form A — one schedule, every pod purging together
spec:
  accesslogPurge:
    interval: 24h

# form B — staggered phases, plus the rotating write endpoint (ADR-037)
spec:
  rotation:
    enabled: true
    slotLength: 6h          # interval becomes replicas × slotLength
```

**Setting both is refused**, never silently resolved by precedence — a field that is
quietly never in force is the class of defect this split exists to remove. The
condition is `RotationConfigured=False/ScheduleDeclaredTwice` and it names both
fields. Meanwhile the operator's default 24h interval stays in force, so nothing goes
unpurged while you work out which one you meant.

Both durations are Go durations (`168h`, `24h`, `30m`) and must land on a whole
second: `olcAccessLogPurge`'s fields are `[ddd+]hh:mm[:ss]` and there is nowhere to
put a millisecond. The API server checks only the *grammar* — `0s` and `500ms` both
parse — so the operator reports the rest and writes its default rather than leaving a
journal unbounded.

**Migration — do this BEFORE you upgrade the operator.** The old `accesslogPurge`
was a two-token string, `"7+00:00 1+00:00"`. It is **refused, never reinterpreted**:
silently re-reading an old value as a retention would be undebuggable. Rewrite it as
`retention:`, and move the interval to the cluster if you meant to keep it.

That sentence covers a *new* apply. It says nothing about the `SlapdDatabase` objects
already **stored** in your API server, and those are the dangerous ones. A stored
string does not decode against the object schema, and the damage is not confined to
the object carrying it. Measured against a real API server (envtest, k8s 1.37, the
pre-M7 CRD upgraded in place to the current one, one legacy object and two healthy
ones):

| what | result |
|---|---|
| applying the new CRD over the old one | **accepted** — nothing warns, nothing refuses |
| typed read of the legacy object | fails: `json: cannot unmarshal string into Go struct field DatabaseReplicationConfig.spec.replication.accesslogPurge of type v1alpha1.AccesslogPurgeConfig` |
| typed read of a healthy object | fine |
| typed **list** of the namespace, and cluster-wide | **fails, with the same error** — the healthy objects are collateral |
| typed list of a namespace holding no legacy object | fine |
| the operator's **cache syncing** `SlapdDatabase` | **never syncs** (`WaitForCacheSync` still false after 20 s) |
| `kubectl get` / any unstructured read | fine, and shows the stored string verbatim |

So the blast radius is **every list whose result set contains the object**, and the
operator's informer lists `SlapdDatabase` cluster-wide. One un-rewritten CR, in any
namespace, is enough to stop the operator reconciling **every** `SlapdDatabase` in
the cluster — not just its own, and not just its namespace. Nothing about the symptom
points at the CR that caused it.

Before upgrading, find them and clear the field — the cluster now owns the interval
anyway, and an unset retention is the 7-day default:

```bash
# find every SlapdDatabase still storing the string form
kubectl get slapddatabases -A -o yaml | yq '
  .items[] | select(.spec.replication.accesslogPurge | tag == "!!str")
  | .metadata.namespace + "/" + .metadata.name + "  " + .spec.replication.accesslogPurge'

# clear it, one per line of that output
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":null}}}'
```

Clearing is the pre-upgrade move, not rewriting: under the **old** CRD the field is
still typed `string`, so patching in the object form is rejected outright
(`spec.replication.accesslogPurge in body must be of type string`). Set the retention
you want after the upgrade, when the schema can hold it.

If you upgraded first and the operator has gone quiet, you are not stuck and nothing
needs restoring. `kubectl` still reads and writes these objects — only the *typed*
decode fails — so the same `yq` sweep still finds them, and a merge patch into the
new object form is accepted and repairs the decode immediately (measured: the typed
list went back to clean on the next call):

```bash
kubectl patch slapddatabase <name> -n <namespace> --type=merge \
  -p '{"spec":{"replication":{"accesslogPurge":{"retention":"168h"}}}}'
```

`spec.replication.syncprovSessionlogSource` chooses *where* the data database's
syncprov session log lives. `InMemory` (the default, and what an unset field means)
is the in-memory log sized by `syncprovSessionlog`. `Accesslog` points syncprov at
this database's own change journal instead — one copy of the recent history, on
disk, surviving a restart, where the in-memory one starts empty after every restart.
The two are exclusive: `Accesslog` requires `syncprovSessionlog` unset or `0`, and
needs `replication.enabled` with `deltaSync`, since without a journal there is
nothing to read. It does **not** put a session log on the accesslog database —
ADR-022 still forbids that, and it is a different attribute on a different database.

Sourcing the log changes the replay window's *units*. In memory the window is an
operation count — the last 5000 writes, however long that takes. From the journal it
is the journal's retention, i.e. `spec.replication.accesslogPurge.retention`, seven
days by default. Which is larger depends entirely on your write rate; size the purge window
on purpose before relying on it (see *Journal sizing* below).

This field is **startup-only**, and that has consequences worth reading twice.
`olcSpSessionlogSource` cannot be written to a running slapd at all — doing so is a
double free in every released OpenLDAP, 2.5.20 through 2.7.1 and master, and it has
already killed live providers — so the operator never writes it over the wire. It
reaches `cn=config` only through the init container, which converges it offline at
pod start. Editing the field therefore changes nothing until each pod restarts.
`kubectl get slapddatabase <name> -o yaml` shows `StartupConfigConverged`: `False`
with reason `RestartRequired` names the pods still running the old configuration,
`True` means every pod that could be read carries the declared one. The operator
does **not** restart them for you: restarting a provider fleet is a topology event
at every peer — 130–268 s of address rediscovery each, measured
([ADR-016](adrs/adr-016-pod-routed-cross-cluster-replication.md)) — far too much to
happen behind your back because you edited a tunable. Delete the pods, one at a
time, when it suits you.

Through all of that the database keeps exactly one session log. Until a pod actually
carries the source, its in-memory log is left alone, and the two are exchanged in a
single offline write at the restart — never dropped first and replaced later. A
cluster's first generation of pods is the same story from the other end: the operator
creates the database and its overlays on a running server, where the attribute cannot
go, so a brand-new pod runs the in-memory default and picks the source up one restart
later.

`spec.indices` is additive: the operator adds index definitions the database is
missing and never removes one, because back-mdb rejects a second definition for an
attribute that already has one. Dropping an entry from the list therefore leaves the
index in place. Index changes made through `cn=config` are rebuilt online by
back-mdb — no `slapindex` run, no downtime.

### Per cluster — `SlapdCluster`

| Field | Default | Class |
|---|---|---|
| `spec.tuning.noSync` | `false` | converged (per database) |
| `spec.tuning.toolThreads` | 2 | converged |
| `spec.ldap.tls.protocolMin` | `3.3` (TLS 1.2), written whether or not TLS is on | converged |
| `spec.ldap.tls.cipherSuite` | none — OpenSSL's own list | converged when set |
| `spec.ldap.passwordHash` | `{SSHA}` | converged |
| `spec.logLevel` | 16640 | **rolls the pods** — slapd takes `-d` at startup only |
| `spec.replication.keepalive` | `240:3:30`; `none` disables | converged into the stanzas |
| `spec.replication.retry` | `10 +` | converged into the stanzas |
| `spec.backend.idlExponent` | none — slapd's 16 | **bootstrap-time** |
| `spec.persistence.{config,data,accesslog}.size` | 1Gi / **5Gi** / 1Gi | StatefulSet `volumeClaimTemplates` — **create-only in practice**: the templates are immutable, so a changed size needs the StatefulSet recreated |
| `spec.rotation.enabled` | off | converged into every pod's `olcAccessLogPurge` |
| `spec.rotation.slotLength` | none, and **required when enabled** | converged |
| `spec.rotation.slotsFromWriteToPurge` | `1` | signed, taken mod `replicas`; `v mod replicas = 0` is refused and reported |
| `spec.rotation.leadTime` | `0` — the endpoint moves at the boundary | not written to slapd at all; it shifts only *when* the write Service is repointed |

`spec.ldap.passwordHash` governs only what slapd hashes on a client's behalf. The
root passwords the operator generates are hashed by the operator before they reach
slapd. Stronger schemes need their module in the runtime image, and slaptain's does
not ship `pw-argon2` today — asking for `{ARGON2}` produces a slapd that rejects
every password write.

### The purge rotation: what the operator checks, and what it only reports

`spec.rotation` puts every read-write pod's accesslog purge on one absolute grid:
with `n = spec.replicas` pods and a slot length `t`, every pod gets purge interval
`T = n·t` and pod `k` gets phase `k·t`, counted from the epoch. That third value is
a slaptain patch, not upstream's, so it needs a slaptain slapd image
(`2.7.1-0+slaptain2` or later — `slapd -VV`).

Opt-in, and off by default: nothing about an existing cluster's purge schedule
changes until you set it.

#### Turning it on

Both charts render `spec.rotation`, and a chart that says nothing about rotation
renders byte-identically to one from before the feature existed — an upgrade changes
nothing for a cluster that has not asked.

Single site, `charts/slapd`:

```yaml
# values.yaml
replicas: 3
replication:
  enabled: true
rotation:
  enabled: true
  slotLength: "6h"      # required when enabled; there is no default
```

A mesh, `charts/slapd-mesh` — under `cluster:`, and applied unchanged at every site
like everything else in that chart:

```yaml
cluster:
  replicas: 2
  rotation:
    enabled: true
    slotLength: "6h"
```

`charts/slapd-mesh/examples/rotation.yaml` is that block as a stackable overlay:
`-f examples/three-site.yaml -f examples/rotation.yaml`.

The values name no site, no endpoint and no pod — a schedule is a shape, and every
site derives the same grid from the same `replicas`. The grid is nonetheless per
**site**, over that site's own read-write pods; a write sent to another site lands
where the client sent it, which is a mesh accepting writes at any provider rather
than a gap in the rotation.

Do not declare `spec.accesslogPurge.interval` alongside this. The two are the two
forms of one decision — *when* the purge runs — and setting both is refused by name
(`ScheduleDeclaredTwice`). Under rotation the interval is `replicas × slotLength`
and there is nothing left to declare.

Leave `slotsFromWriteToPurge` unset unless you are measuring. Unset is `1`: "purge
the slot **after** writing", so the write endpoint names the pod that purges next —
it has just stopped taking writes and works its journal down while somebody else
takes them. `-1` is the other experiment, "purge the slot **before** writing", and
it is the same answer as `replicas − 1` because the value is read modulo `replicas`
— which is the point of the sign: `2` means "the slot before" only until somebody
adds a pod, while `-1` keeps meaning it.

The rule is `v mod replicas ≠ 0`, not a minimum. Zero *and every multiple of
`replicas`* put the write on the pod that is purging, so `3` on a three-pod cluster
is as wrong as `0` and looks far more reasonable. That rule needs `spec.replicas`,
which no CRD marker can see, so the operator reports it — naming the slot the value
**resolved to** rather than echoing what you typed, since `-1` and `replicas − 1`
are one answer and only one of them is on your screen.

Note what the chart does *not* do — it restates neither default nor any other, so
the day the operator's changes, your cluster follows.

**Before you set it, check both prerequisites.** The image must carry the
purge-phase patch (above), and the clients must be the kind this helps — see
*It benefits clients that open short-lived connections*, below. Neither is
something the operator can arrange for you, and the second decides whether the
feature does anything at all.

**Staggering alone changes nothing a client can see.** This is measured, not
cautionary: de-synchronising the providers' purges lands on the synchronised arm and
nowhere near the arm where the write target did not purge (ADR-037, third
amendment). What the stagger buys is a pod that is *not* purging for the write
endpoint to sit on — a prerequisite, not the benefit.

#### The write endpoint

Turning the rotation on publishes a second client Service, `<cluster>-write`,
selecting exactly one read-write pod: by default the one that purges in the next
slot, and in general the one `slotsFromWriteToPurge` names. It moves
at every slot boundary, in lockstep with the grid, so the pod taking writes is never
the pod purging. It answers on the same ports as `<cluster>` and honours
`spec.service.type`, so a client that dials `<cluster>:389` today writes to
`<cluster>-write:389` instead and changes nothing else.

**Send writes there and no purge runs where your writes land.** Reads stay on
`<cluster>` (or `<cluster>-readonly`): spreading them is the point of having
providers. Writes sent anywhere else remain perfectly legal — every pod is a
provider and accepts a write — they just cost you the stall the rotation exists to
remove. The endpoint is a recommendation, not an enforcement.

**It benefits clients that open short-lived connections.** A Kubernetes Service
steers *new* connections; it does not re-wire an established one, so a client holding
one connection keeps writing to the pod it first reached, however often the endpoint
moves. A connection-pooling client therefore needs a maximum connection lifetime that
is short relative to `slotLength` — the shorter the ratio, the larger the share of
writes that lands on the current target. We have not measured where that ratio stops
paying, so no number is given here; ADR-037 declines to fix numbers it has not
measured, and so does this guide. A client that holds connections for hours gets
nothing from the rotation, and no operator-side setting can change that.

#### `leadTime`: moving the endpoint *before* the boundary

The two halves of the rotation do not cost the same. Measured on a two-provider
cluster: with the rotation on and the writes **pinned** to one pod, throughput was
indistinguishable from a cluster with no rotation at all — staggering the purges is
free. With the writes **following** the endpoint, throughput fell 2.8 % and dropped
iterations doubled. Staggering costs nothing; moving the writes costs, and it costs
in one place: **99.9 % of that loss landed in the first 30 s after a slot boundary**,
69 % of it inside the client's 20 s connection lifetime.

The mechanism is the handover. With no lead the endpoint moves *at* the boundary, so
the pod that stops taking writes starts purging at the same instant — and every
connection that has not yet aged out is still pointed at it, delivering writes into
an active purge.

`spec.rotation.leadTime` is how long **before** each boundary the endpoint moves:

```yaml
spec:
  rotation:
    enabled: true
    slotLength: "6h"
    leadTime: "60s"     # unset = 0 = the endpoint moves at the boundary
```

**Set it above your clients' maximum connection lifetime.** With a lead at least
that long, every connection pointed at the outgoing writer has been closed and
redialled before that pod begins purging. That is the whole rule, and it is yours:
the lifetime is a property of your client, not of the cluster, so **the operator
does not derive it, does not default it, and no number is recommended here** —
nothing has measured one. Unset means zero, which is the behaviour of every cluster
that predates the field.

It is a lookahead and only a lookahead. The purge grid is untouched — the interval
stays `replicas × slotLength` and pod `k` still purges at `k × slotLength` from the
epoch — so setting or clearing a lead never rewrites a pod's `olcAccessLogPurge` and
never rolls a pod.

**One bound, and it is the only thing checked:** `leadTime` must be at least zero and
strictly below `(replicas − 1) × slotLength`. At or beyond that the lead reaches back
past the instant the incoming writer *started* purging, so the endpoint would hand
writes to a pod that purges during its own write slot. A value outside the bound is
reported as `RotationConfigured=False/LeadTimeOutOfRange` and the whole rotation is
withheld — the cluster keeps purging on the default interval. Nothing else is
checked: a lead too short to help your clients is a configuration you chose, and the
operator will not second-guess it.

The current writer and the next rotation are published on the SlapdCluster as
`WriteEndpointRotating`:

| Reason | What it means |
|---|---|
| `WriterSelected` | the message names the pod the endpoint selects and the instant it moves next. With a `leadTime` in force that instant is the **endpoint move**, one lead before the slot boundary, and the message says so |
| `NoEligibleWriter` | no pod could be chosen this pass — none Ready, or readiness could not be read at all. The selector is left exactly where it was and writes keep going there, because a Service selecting nothing is a total write outage. Nothing was repointed, and the message says so |

Observation only: it gates nothing and withholds nothing.

**Turning the rotation off** leaves `<cluster>-write` in place and widens it to the
whole read-write fleet, so a client still following it keeps writing. It is not
deleted — clients were told to follow that name — and it is not left pinned to one
pod, because with the rotation off nothing phases that pod's purge any more and every
write would land on a provider that purges. A cluster that never rotated never grows
the Service at all.

The API server checks only that `slotLength` parses as a Go duration. Everything
else is the operator's, and each is published on the SlapdCluster as
`RotationConfigured` — reported, never refused, so a schedule you cannot honour
never stops a running directory reconciling:

| Reason | What it means |
|---|---|
| `RotationActive` | the grid is in force; the message states the interval and the spacing |
| `RotationDisabled` | `enabled` is unset or false — the good case, not a fault |
| `SlotLengthMissing` | `enabled: true` with no `slotLength`. There is no default on purpose |
| `SlotLengthNotPositive` | `0s` passes admission and would put every pod's phase at zero — the synchronised purge the feature exists to remove |
| `SlotLengthNotWholeSeconds` | `500ms` passes admission and has no representation in `olcAccessLogPurge`, whose fields are `[ddd+]hh:mm[:ss]` |
| `WriteSlotCollidesWithPurge` | `slotsFromWriteToPurge` resolved to slot 0 — the pod that is purging. Zero and every multiple of `replicas` land there, which is why the rule is modular and not a minimum. Nothing rotates; the default interval keeps the journals bounded |
| `ScheduleDeclaredTwice` | `spec.accesslogPurge.interval` and `spec.rotation.enabled` both declare *when* the purge runs. Delete one |
| `LeadTimeOutOfRange` | `spec.rotation.leadTime` is negative, or at or beyond `(replicas − 1) × slotLength`. Beyond that bound the lead reaches back past the instant the incoming writer started purging. Nothing rotates; the default interval keeps the journals bounded |
| `PurgeIntervalNotUsable` | `spec.accesslogPurge.interval` is non-positive or finer than a second, so it has no representation on the server. The default interval is in force |

Each SlapdDatabase then publishes `AccesslogPurgeConverged`:

| Reason | What it means |
|---|---|
| `Converged` | every pod that was read carries its own phase |
| `RotationDisabled` | the cluster is not rotating; the value is this database's own age and interval |
| `RetentionNotUsable` | `accesslogPurge.retention` is non-positive or finer than a second. **Reported, and the operator's 7-day default written anyway** — a mistyped duration must not become an unbounded journal |
| `PurgeDisabled` | `accesslogPurge.enabled` is `false`. No purging on this journal at all; the growth is yours |
| `PurgePhaseUnsupported` | a named pod's slapd refused the phase. That image carries no purge-phase patch; the operator wrote the two-field value instead, so the journal stays bounded and that pod simply does not take its phase |
| `NoReachablePod` | nothing could be read. `Unknown`, never `False` — unreadable evidence is not evidence |

A pod that could not be read is named in the message and never counts against the
verdict.

Sizing is yours. The operator will not refuse a rotation whose phases are too long
for your write rate, and will not shorten one to keep a purge small. The arithmetic
is ADR-037's capacity condition — every provider logs every write, including the ones
it receives by syncrepl, while only the inactive ones purge — against your own
storage's measured delete rate.

### `cn=monitor`: always present, nothing to set

Every pod, read-write and read-only, carries slapd's monitor database. The operator
converges it on every pass (ADR-033 amendment 2026-10-08). It adds
`olcDatabase=monitor` with `olcRootDN: cn=admin,cn=config` and keeps its one ACL equal
to a constant. The monitor backend is built into slapd in every slaptain image, so
there is no module to load:

```
to * by dn.children="cn=slaptain-auth" read
     by dn.children="cn=peercred,cn=external,cn=auth" read
     by * none
```

There is no field and no switch. At rest none of this writes anything: the operator
reads, compares and moves on. It writes again only when the ACL differs, after a hand
edit or after an upgrade that changes the constant.

Who can read `cn=Monitor`:

- **any node-local replication identity**, `cn=repl-<db>,cn=slaptain-auth`, with that
  database's `replication-password`. Any database's identity works, not only the
  one for the database you are looking at;
- **`cn=admin,cn=config`**, which is the monitor database's rootDN;
- **a process inside the pod on the ldapi socket** (SASL EXTERNAL), whatever its UID
  (measured for the ADR amendment);
- **nobody else.** Anonymous and every data-tree identity get `No such object`. That
  includes the legacy `cn=replication,<suffix>`, so `slctl ldapsearch --as
  replication` cannot read it. Use `--as config`, plus `--database <db>` when the
  namespace holds more than one database.

```bash
slctl ldapsearch -n <ns> --as config --database <db> --pod 0 -b cn=Monitor -s one '(objectClass=*)'
ldapsearch -x -H ldap://<pod>:1024 -D cn=repl-<db>,cn=slaptain-auth -y <pwfile> \
  -b 'cn=Connections,cn=Monitor' '(objectClass=*)' '+'
```

**Per-database statistics depend on order.** back-mdb attaches its `olmMDB*`
attributes to `cn=Database <N>,cn=Databases,cn=Monitor` when it opens a database,
and only if the monitor database exists at that moment. A cn=config ADD opens the
database immediately. Monitoring is on by default in back-mdb, so no `olcMonitoring`
setting is involved. Measured on 2.7.1:

- an mdb database ADDed after the monitor database gets them at once, whether the
  monitor database was there at start or was itself ADDed at runtime;
- an mdb database that was open before the monitor database existed gets them only
  after slapd restarts. Setting `olcMonitoring: TRUE` live does not help.

So the operator makes sure the monitor database exists immediately before it ADDs a
data or accesslog database, on read-write and read-only pods alike. The node-local
auth database is added after the monitor database in the same pass. A pod created by
this operator version has statistics for every database from the start. A pod whose
databases predate the monitor database (an upgrade from an operator without it) gets
them at its next restart. The global counters (`cn=Operations`, `cn=Connections`, …)
answer immediately either way.

### Deliberately not tunable

- **`olcIdleTimeout` and `olcWriteTimeout`.** slapd's default for both is *never
  close*, so a pod behind a stateful firewall accumulates dead connections until it
  runs out of descriptors. The gap is real and there is no field for it, because an
  `ldapmodify` of either against a running 2.7.1 does not fail and does not crash —
  it **hangs** the process. The CSN is queued and never graduates, the pod answers
  nothing at all, not even an anonymous rootDSE, and `SIGTERM` sticks. Reproduced
  twice on a healthy three-pod cluster; recovery was restarting every pod. Both
  values are fine when they come from the boot config, so the open path is
  bootstrap-time via the init container. Until that lands there is no field: offering
  one the operator cannot honour is what ADR-024 R4 forbids. See
  [`docs/BACKLOG.md`](BACKLOG.md).
- **Thread and buffer counts** (`olcThreads`, `olcListenerThreads`,
  `olcConcurrency`, `olcSockbufMaxIncoming*`, `olcConnMaxPending`). Upstream's
  defaults are defensible and we have no measurement that asks for a different one.
  `spec.tuning` is the home when one arrives.
- **The replication identity's DN, its ACL and its limits.** One contract the
  operator owns end to end (ADR-024 R7). A user who cannot change the DN has no
  business capping its searches.

## Sizing: from lab to production

### The map size is a reservation; the volume is the bound

`olcDbMaxSize` is an address-space reservation, not an allocation — LMDB grows the
file sparsely inside it. So the number that actually stops a database is the size of
the `/data` PVC, not `spec.maxSize`. Size the map high and the volume to your data;
the volume is the limit you can see, alert on and expand, and the map size is the one
you cannot change afterwards without recreating the database.

That is why slaptain's default map is 32Gi against a default `persistence.data.size`
of 5Gi. The ratio is deliberately lopsided: a cluster runs out of PVC long before it
runs out of map, which is exactly the failure you want — it is visible, it can be
alerted on, and it is fixable by expanding the volume, whereas exhausting the map is
none of those things. In production the same relationship holds, at different
absolute numbers.

The 5Gi is the OPERATOR's default and lives in exactly one place
(`internal/controller/persistence_defaults.go`). The charts deliberately do not
restate it: a size written into a `SlapdCluster` is frozen there at creation time, so
a chart that repeated the number would pin every cluster it installed to whatever the
default was on the day of install. Unset means "this operator version decides".

Work it in this order:

1. Estimate the directory's byte volume: entries × average entry size, plus the
   indices. Index overhead depends on how many attributes you index and with which
   match types, so measure it on a representative load rather than guessing a ratio —
   an `slapadd` of a sample LDIF into a throwaway database answers it in minutes.
2. Size `persistence.data.size` to that, with headroom for growth and for LMDB's
   free-page churn under write load. This is the number you will revisit.
3. Set `spec.maxSize` comfortably above the largest volume you ever expect to give
   this database — several times over, since the reservation costs address space and
   nothing else. Getting this wrong is the expensive mistake: too low and the fix is a
   backup-delete-restore cycle.

→ Decide `spec.maxSize` once, before the data is loaded. Treat
`persistence.data.size` as the knob you tune afterwards.

### Journal sizing: write rate × purge window

The accesslog journal is not a copy of the directory. It holds change records, and it
only ever needs to hold a purge window's worth of them, so its size follows from write
*rate*, not entry count:

    journal bytes ≈ writes per day × purge window in days × bytes per change record

The operator gives every accesslog DB an 8Gi map and a `2048 15` checkpoint, neither
of them configurable — the journal is a derived, purged artefact, so losing its tail
costs a consumer a full refresh rather than data.

`spec.replication.accesslogPurge.retention` defaults to `168h` — keep seven days of
journal. Unset gets that default; `enabled: false` opts out of purging entirely.

How often the sweep runs is the **cluster's**, not this database's: the operator's
default of 24h, or `SlapdCluster.spec.accesslogPurge.interval`, or
`replicas × slotLength` under `spec.rotation` (ADR-024 R6 — see *`olcAccessLogPurge`
has three owners* above). The sizing arithmetic below is unchanged; only who writes
the interval moved.

There is a default because "no opinion" is not a neutral setting here. An unpurged
journal grows until it reaches its 8Gi map ceiling, and the accesslog overlay sits in
the **data** database's write path — so the moment the journal cannot take a write,
neither can the data. That is a slow-motion outage rather than a tuning wart, which is
why the field is defaulted rather than left to be remembered.

With `syncprovSessionlogSource: Accesslog` the same window is also the session log's
replay window, so the purge policy stops being only a sizing decision and becomes
how far behind a reconnecting consumer may be and still be answered from a delta.

Seven days is chosen to cover a weekend-plus consumer outage. The window is precisely
how long a consumer may stay away and still resume from a delta: one whose cookie
predates the purge falls back to a full refresh. Lengthen it if your worst realistic
outage is longer, and size `persistence.accesslog.size` to match.

On the 2.7-default images the interaction between purging and a dormant consumer
(upstream ITS#9580 — a purge can leave a returning consumer's cookie unanswerable) is
mitigated by the cookie-flush fix. Clusters still on the `-ol26` image pair carry
elevated exposure: there, treat a consumer outage approaching the purge window as
something to verify resynced rather than assume.

Size `persistence.accesslog.size` from the same arithmetic, with the same headroom
logic as the data volume.

### idlExponent: decide before the data lands

`spec.backend.idlExponent` bounds how many entry IDs one index slot holds before
back-mdb degrades the slot to a range (valid 16–30; slapd's default 16 means 65536
IDs). On a directory where a common index slot exceeds the cap — a single
`objectClass` value shared by a million entries is the classic case — every search
using that slot reads far more candidates than it needs. Raising it a few steps is the
standard large-directory adjustment; the cost is memory per index page.

This one is **bootstrap-time**, and it is the field most likely to bite. It governs
on-disk index layout, so it deliberately carries no operator default: moving one later
would split a cluster into pods bootstrapped before and after the change, with nothing
able to repair the difference. Set it, or don't, before the cluster is created.
Changing it afterwards means recreating the config volumes pod by pod (letting each
re-bootstrap), or rebuilding the cluster and restoring.

### Index strategy

The operator's baseline — `objectClass`, `entryCSN`, `entryUUID`, equality — is not
yours to remove and implements replication, not query performance. `spec.indices` is
where your query patterns go: index the attributes your clients actually filter on,
with the match types they actually use (`uid eq,sub`, `cn eq,sub`). An index that
serves no filter costs write throughput and buys nothing.

### Durability: noSync and the checkpoint

`noSync` disables the per-write fsync and is a cluster-wide posture, not a per-database
detail — set `spec.tuning.noSync` and let databases inherit it; the per-database
`spec.noSync` is the exception, not the entry point. The argument for turning it on is
that the replication mesh plus the delta journal are the redundancy that an fsync
would otherwise provide.

`spec.checkpoint` ("`<kbyte> <min>`", default `1024 5`) is what bounds the window
`noSync` opens. Per `slapd-mdb(5)` it only takes effect while `noSync` is in force,
and the operator writes it unconditionally so that turning `noSync` on is a one-field
change that is already safe.

The one combination the operator refuses is `noSync: true` with `checkpoint: ""`:
nothing schedules a flush, so an unclean shutdown loses an unbounded window of writes.
The `SlapdDatabase` goes to `phase=Error`, reason `UnsafeDurability`, with the fix in
the message.

### Connections and timeouts

What landed is on the syncrepl side: every stanza carries `network-timeout=10` (bounds
the TCP connect and TLS handshake, so a provider whose node is gone is noticed in ten
seconds rather than at the kernel's TCP timeout), `retry=10 +` and
`keepalive=240:3:30`. The keepalive idle time sits deliberately under the five-minute
mark where stateful firewalls and cloud load balancers commonly drop an idle flow,
because a `refreshAndPersist` connection is idle by design between writes.

What is deliberately **not** in the stanzas is `timeout=`. It shipped briefly as
`timeout=300` and was withdrawn the same day on live evidence: supplying it flips
slapd's refresh-phase waiting discipline from a non-blocking peek to a blocking wait
on a threadpool thread, and a `cn=config` write pauses the whole pool — so while any
consumer was in refresh phase, every config write the operator made froze the entire
server for up to 300 seconds (three such total-silence freezes measured per
convergence pass, cascading across a three-site mesh). The expiry detects nothing —
slapd just goes back to listening — so the wait bought no protection at any value.
Failure detection belongs to the socket-level mechanisms above, which act without
occupying a worker thread. See the 2026-09-13 entry in
[`docs/reconcile-loop-fixes.md`](reconcile-loop-fixes.md) and the ADR-024 amendment
of the same date. `timelimit=` is likewise never emitted — that one is a server-side
search time limit and would abort the persistent search itself.

What did not land is the client-facing side: slapd will hold a dead client connection
forever, and there is no field for it yet. If your clients sit behind a NAT or a
firewall that drops idle flows, budget file descriptors accordingly and track the
BACKLOG entry.

## Validating a sizing

Fixture-sized coverage cannot see any of this. A single-digit-entry LDIF is how a
500-entry replication cap survived a green test suite for the whole life of the
project.

- `E2E_SCALE=1 ./tests/e2e.sh test <context>` runs the many-entries fixture
  (`tests/e2e/scale_test.go`): a generated seed of 1200 entries (`E2E_SCALE_ENTRIES`)
  and a churn loop of 700 writes (`E2E_SCALE_CHURN`) that pushes the change journal
  past the same cap. It asserts convergence across every RW pod, an unbounded client
  enumeration, the replication identity reading its own journal past 500 records, and
  the structural attributes (map sizes present on both databases, `entryCSN`/
  `entryUUID` indexed). Raise the entry count to approximate your real directory.
- `tests/e2e/tunables_test.go` runs ungated and guards the converged values.
- `slctl inspect -n <ns> <cluster>` is the health check: per-pod LDAP queries plus
  CSN convergence, syncrepl stanza counts, RID uniqueness and external-peer state.
  `--short` for CI; it exits non-zero on a failed check.
- `kubectl get slapddatabase <name> -o yaml` for the `TunablesConverged` condition.
  `False` with reason `RecreateRequired` means a value you edited cannot be applied to
  a live database, and the message carries current, desired and the change path. The
  same command's `StartupConfigConverged` condition answers the startup-only
  question: `False` / `RestartRequired` names the pods whose running configuration is
  not the declared one, and a pod that could not be read is reported as not assessed
  rather than counted as diverged.

## Related

- [ADR-024](adrs/adr-024-tunable-placement.md) — the placement doctrine and its
  amendments (the segfault, the hang, and the fourth class).
- [ADR-022](adrs/adr-022-syncprov-sessionlog.md) — the sessionlog's placement and its
  cost model, amended with the accesslog-sourced variant.
- [ADR-034](adrs/adr-034-startup-only-configuration.md) — the startup-only class:
  why the init container converges it offline, and why the restart is yours to take.
- [ADR-020](adrs/adr-020-accesslog-access-control.md) — accesslog access control and
  the limits amendment.
- [ADR-019](adrs/adr-019-per-database-accesslog.md) — one journal per database.
- [OpenLDAP versions](OPENLDAP-VERSIONS.md) — the 2.6/2.7 image pairs. Every live
  modifiability result quoted here was established on 2.7.1.
- [Backup & Restore](BACKUP.md) — the recreate path every bootstrap-time and
  recreate-required tunable points at.
- [`docs/BACKLOG.md`](BACKLOG.md) — what is deliberately not tunable yet, with the
  evidence.
