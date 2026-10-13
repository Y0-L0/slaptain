# ADR-033: Three monitoring surfaces, and which metric belongs to which

**Status:** Accepted — all surfaces implemented and live on a three-site mesh (2026-10-09)
**Date:** 2026-10-06, revised through 2026-10-09 (see Revision history)

## Context

Before this ADR, slaptain exposed no monitoring data of its own. The operator served
controller-runtime's scaffolded metrics (reconcile counts, workqueue depth, Go runtime),
the data plane served nothing, and everything we knew about a running mesh was CR status,
read by a human through `kubectl` or `slctl inspect`.

OpenLDAP has no OpenMetrics endpoint. Its introspection surface is the `cn=Monitor` LDAP
tree, so any slapd exporter is an LDAP-to-OpenMetrics translator.

Three vantage points exist, and they see different things: the API server, the operator,
and the pod. Without a written boundary, each accretes whatever is easiest to add from
where its author happens to stand, and the same quantity ends up exported twice, with two
values and no stated authority.

## Options considered

### Option 1: One surface — an exporter sidecar per pod — rejected

The sidecar's vantage point is one pod. The signals that matter most here are cross-pod
and cross-site by construction: per-database CSN divergence, a peer that answers but is
only partially verifiable, a suffix entry demoted to glue on one pod (ADR-025). A
federation of sidecars reconstructing them in PromQL would reimplement
`checkLocalCSNConvergence` in a query language, with no way to express ADR-008's rule that
unreadable evidence never counts as healthy.

### Option 2: One surface — the operator exports everything — rejected

Per-pod counters (connections, operations, threads, waiters) would turn every scrape
interval into N more LDAP searches from the one shared control-plane client, putting
monitoring load on the component whose failure costs convergence. And an
operator-relayed counter is indistinguishable, in the time series, from one the operator
could not read.

### Option 3: Put the operator's observations into CR status, export via Surface 1 — rejected

The cheapest design on paper: no new Go, no registry, no second scrape target, and visible
in `kubectl get -o yaml`. Write amplification is not the objection — status already churns
once a minute per cluster through `lastChecked` (ADR-002 amendment), so extra fields cost
nothing new. We reject it on two other grounds:

- A status field is API surface with a deprecation story; a metric is renamed in one
  release and one rules file. Quantities whose shape we are still learning do not belong
  in the API.
- Per-pod detail (the suffix probe is pods × databases) bloats every `kubectl get` and
  every controller fetch, for data whose only consumer is a time series.

### Option 4: Three surfaces with an ownership rule — chosen

Keep all three vantage points and make the boundary explicit, so that each metric has
exactly one home.

## Decision

### 1. The boundary: a metric belongs to the vantage point that observes it directly

1. The API server — what slaptain has declared and recorded.
2. The operator — what it saw by querying across pods and sites.
3. The pod — what slapd knows about itself.

A metric belongs to the vantage point from which it is a **direct observation**, never to
one that would relay it: the API server holds CR status because the operator wrote it
there; the operator holds a cross-pod comparison because only it performed it; the pod
holds its own counters because they are its process state. The test for Surface 2 is not
whether the operator knows a thing, but whether it is the **only** one that knows it.

→ No quantity is exported from two surfaces. The single sanctioned overlap is `up`: the
three surfaces fail independently, and a dashboard must be able to tell which one went
quiet.

### 2. Surface 1 — CR state, via kube-state-metrics, no code

kube-state-metrics' `--custom-resource-state-config` exports CR status: `status.phase`,
replica counts, `observedGeneration`, per-peer `replicationState` and `lagSeconds`, and
every `status.conditions` entry of `SlapdCluster` and `SlapdDatabase`, generically. We ship
the config (`monitoring/kube-state-metrics-values.yaml`); we write no Go.

- A condition's `reason` is a label, not a separate series, so alert rules can treat
  `DataMissingOnPods`, `DataMissingOnReadOnlyPods` and `GlueSuffix` differently.
- A condition's `lastTransitionTime` is a timestamp gauge: for `PodsResponsive` the age of
  the transition is the signal (ADR-030).
- `Unknown` is absent, never `0` (decision 5).

`SlapdBackup` conditions (`SourceConverged`, `SourceSuffixHealthy`) and
`status.restore.phase` belong here too, but are not yet in the config (`docs/BACKLOG.md`).
Both are config-only additions.

### 3. Surface 2 — the operator exports what only it knows

Namespace `slaptain_`, served from the operator's existing metrics endpoint, refreshed on
its 60 s tick. Eight families:

| Family | What it is |
|---|---|
| `csn_spread_seconds` | per database: newest minus oldest `contextCSN` over the RW pods that answered — the magnitude `ReplicationConverged` reduces to a boolean |
| `csn_pairs_read`, `csn_pairs_expected` | per database: the evidence behind that judgment, as two absolute counts; `read < expected` is the alertable condition |
| `peer_providers_discovered` | per external peer: how many remote providers discovery resolved — a link that resolved to nothing is how a cross-site outage begins |
| `suffix_probe_ok` | per pod per database: the ADR-025 suffix probe, labelled `role` |
| `suffix_glue` | per pod per database: a glue suffix entry, value always 1 — presence is the signal |
| `cluster_observation_timestamp_seconds`, `database_observation_timestamp_seconds` | the time of the pass that produced the samples |

The operator exports **no pod-local counter**; anything that would need a new LDAP query
per pod per scrape is Surface 3's. There is no "expected providers" series: `replicasPerPeer`
is fan-out, not a prediction of what discovery should find, and any expectation derived
from discovery would be circular. A non-zero floor is local policy and belongs in an alert
rule.

Design rules the families follow:

- **Two freshness anchors**, because one metric name must carry one label shape: a single
  anchor emitted with and without `database` would double-alert one stalled site.
- **One recording point per controller.** The cluster controller records just before its
  end-of-pass status apply, the database controller where the per-pod probes are final.
  Early and partial paths (restore, failed mesh resolution, `spec.suspend`, early
  `setStatus`) record nothing and leave the previous observation ageing, so one staleness
  rule over the anchors covers all of them. Eviction happens on NotFound and, for
  databases, on deletion intent (ADR-005 retries the finalizer). Cost: the SlapdDatabase
  reconcile returns early while its cluster is not `Running`, so a `Degraded` cluster
  freezes its databases' observations. The staleness rule is therefore load-bearing.
- **Glue is its own family**, because ADR-025 treats it differently from a missing
  entry: it never heals and has a manual runbook, so its alert needs no `for:`.
- **`role` on the pod-scoped families**: a read-only pod mid-initial-sync legitimately
  lacks its suffix entry; a read-write pod in that state is an incident.
- **`mesh` on every series**, empty for a cluster with no `meshRef`. `SlapdDatabase` has
  no `meshRef`, so Surface 1's database series carry no `mesh`: a Surface 1 ↔ Surface 2
  join at database scope keys on `(namespace, cluster, database)`.

### 4. Surface 3 — our own slapd exporter, as an opt-in sidecar over `ldapi`

Pod-local slapd internals: connections, operations by type, threads, waiters, traffic,
and per-database LMDB statistics, including fill against `olcDbMaxSize` (the quantity
`docs/TUNING.md` says to alert on).

#### Why our own exporter

The community exporter is `tomcz/openldap_exporter` (MIT, 52 stars). It has been
archived since 2024-08 and never published an image. Of its 65 forks, two have more than
one star, and one is maintained: `hm-edu/openldap-exporter` (6 stars), whose activity
since May 2026 is all dependency bumps by renovate. Upstream and that fork share the same
functional gaps for our use:

- Simple bind or anonymous only. Neither binds SASL EXTERNAL, so in a sidecar they need a
  password from a Secret; without one they read only the anonymous view of `cn=Monitor`,
  which the constant monitor ACL (decision 6) denies.
- They poll on their own interval and serve the cached result, so a failed read leaves the
  last value standing — the failure decision 5 forbids.
- No per-database LMDB statistics, so no fill metric, which is the one quantity
  `docs/TUNING.md` says to alert on.

Beyond functionality, nothing about them earns the trust we would extend to a data-plane
dependency: a small user base, no maintainer behind the code, and an image built on
Alpine that runs as root — none of the posture (distroless, rootless, read-only root FS)
we hold every other slaptain container to.

The decisive argument is that the work is small. An exporter of `cn=Monitor` is a
straightforward translation: one LDAP search per scrape, entries to samples. Ours is 434
lines of Go (`scc`, excluding comments and blanks, including the curated table below) in
14 functions, total cyclomatic complexity 77. No function reaches the conventional
`gocyclo` threshold of 15; the most complex are flag parsing (14) and the dial (12), and
the translation itself (`SamplesFromEntries`) is 8. For comparison, `slapddatabase_controller.go` alone has
2306 code lines. `docs/MONITORING.md` ("Check it yourself") puts a `cn=Monitor` dump
next to the scrape it produces. Owning that is cheaper than adopting, hardening and patching a fork, and
it removes a third-party dependency from every slapd pod.

#### How

- **Written against slapd, not slaptain.** It reads only `cn=Monitor`, imports nothing
  from the API package and reads no CR; slaptain context (`pod`, `namespace`, `cluster`,
  `role`) comes from the scrape target's labels. It therefore works against any slapd.
- **A curated table, correctly typed.** The exporter maps each meaningful numeric
  `cn=Monitor` entry to a named family of the right type — counters as `_total`, gauges
  as gauges — with labels that carry meaning, not a DN string. The table is data, in
  one place, like the LMDB families. See "Curated families" below.
- **Collect on scrape**, with a socket deadline; a failed read is absent from that scrape.
- **Bind over the pod-shared `ldapi` socket with SASL EXTERNAL** — no password, no Secret,
  no listener. CLAUDE.md's rejection of ldapi for the operator rests on the operator being
  a separate pod and does not carry to a sidecar. `/run/openldap` is mounted read-only
  (`connect()` needs write permission on the socket inode, not on the mount). Connection
  and bind mode are flags, so the binary also serves a remote slapd with a simple bind.
- **Opt-in: `spec.monitoring.enabled`, default off.** The sidecar changes the pod template,
  so an operator upgrade must not add it; enabling it rolls each StatefulSet once, as a
  user-initiated change (ADR-013).
- **Scraped by an opt-in PodMonitor** in `charts/operator` (`slapdPodMonitor.enabled`).

The sidecar has no probe, but like any container, a pull failure or crash loop makes the
pod NotReady.

#### Packaging: its own module, image and version line

The exporter and the operator drift, and that is expected and benign. The exporter's job
— `cn=Monitor` in, a fixed set of families out — changes far less often than the
operator, so it is released on its own schedule and nothing ties one to the other's
version.

- **Its own Go module**, top-level `exporter/` with its own `go.mod`, beside `operator/`.
  Its dependencies (go-ldap, client_golang) no longer move with controller-runtime and
  k8s bumps, and "the exporter knows slapd, never slaptain" is enforced by the compiler
  rather than by an import test.
- **Its own image, `slapd-exporter`** (`images/slapd-exporter/`): a static binary on
  distroless/static, `ENTRYPOINT ["/slapd-exporter"]`. The operator image carries no
  exporter.
- **Its own version line.** Git tags `exporter/vX.Y.Z` (the tag form Go requires for a
  module in a subdirectory); the image is tagged with the bare `X.Y.Z` (docs/VERSIONING.md).
  The metric families are the exporter's API: renaming or retyping one is a major
  version. The first release is `0.0.1`, a lab trial; `0.1.0` follows once it has run
  live at all, and `1.0.0` once the families have run on a live mesh. The main stream's tag lookup must match only `v*`, so a commit carrying both
  kinds of tag resolves unambiguously.
- **The operator defaults the image to a compiled-in exporter version**, with the
  repository derived from the operator's like `slapd` and `slapd-init` (trailing path
  segment swapped for `slapd-exporter`). The operator bumps that constant only when it
  wants a newer exporter. An operator upgrade therefore leaves the sidecar's image, and
  so the pods, unchanged unless the exporter itself moved. `spec.images.exporter`
  overrides it, as before.
- **The interface between the two is minimal.** The operator passes no command and no
  arguments: the image's entrypoint and the exporter's defaults (`ldapi` socket in
  `/run/openldap`, SASL EXTERNAL, `:9330/metrics`) are the whole contract. The exporter
  keeps those defaults stable across versions; the operator owns only the pod-side facts
  (the port name `metrics`, the read-only socket mount, the security context).
- **Dashboards and rules follow the exporter's version**, not the operator's: they query
  its families. `monitoring/` states which exporter major it expects.

Moving to this shape changes the sidecar's image and drops its command and arguments,
so the operator release that introduces it rolls every monitoring-enabled cluster once.

#### Curated families

We do not adopt the upstream exporter's metric shape. It makes one family per monitor
objectClass, labelled by entry DN, but a Prometheus family has exactly one type, and
whether a monitor value is a counter or a gauge depends on the entry, not on its class:
`monitorCounterObject` holds both the total connection count (a counter) and the current
one (a gauge). So the shape can only declare everything a gauge. Fixing that means
splitting families and renaming counters to `_total`, which is no longer compatible with
anything. We take the break now, while nothing depends on our names, rather than keep
wrong metadata forever. Compatibility with upstream's shape — the Grafana `openldap-mixin`
and the Grafana Cloud OpenLDAP integration built from it — is withdrawn; nothing we ship
uses it.

The exporter, not the dashboard, curates. A raw dump pushes the work of knowing what each
entry means onto every dashboard and rule author; the exporter is the one place that
knowledge can live once. Curation here means naming and typing **every** meaningful entry,
not selecting a subset — on 2.7.1, 28 entries carry a numeric `monitorCounter`,
`monitoredInfo` or `monitorOp*` value, plus the start timestamp and the per-database
statistics.

| `cn=Monitor` entry | Family | Type |
|---|---|---|
| `cn=<Op>,cn=Operations` `monitorOpInitiated` | `openldap_operations_initiated_total{op}` | counter |
| `cn=<Op>,cn=Operations` `monitorOpCompleted` | `openldap_operations_completed_total{op}` | counter |
| `cn=Total,cn=Connections` | `openldap_connections_total` | counter |
| `cn=Current,cn=Connections` | `openldap_connections_current` | gauge |
| `cn=Max File Descriptors,cn=Connections` | `openldap_max_file_descriptors` | gauge |
| `cn=Bytes,cn=Statistics` | `openldap_sent_bytes_total` | counter |
| `cn=PDU,cn=Statistics` | `openldap_sent_pdus_total` | counter |
| `cn=Entries,cn=Statistics` | `openldap_sent_entries_total` | counter |
| `cn=Referrals,cn=Statistics` | `openldap_sent_referrals_total` | counter |
| `cn=Open`/`Active`/`Pending`/`Starting`/`Backload`,`cn=Threads` | `openldap_threads_{open,active,pending,starting,backload}` | gauge |
| `cn=Max`/`Max Pending`,`cn=Threads` | `openldap_threads_max`, `openldap_threads_max_pending` | gauge |
| `cn=Read`/`Write`,`cn=Waiters` | `openldap_waiters{direction}` | gauge |
| `cn=Start,cn=Time` `monitorTimestamp` | `openldap_start_time_seconds` | gauge |
| per-database `olmMDB*` | `openldap_mdb_*{suffix}` (unchanged) | gauge |

Naming rules:

- Label values are lowercase (`op="search"`, `direction="read"`). A label is used only
  where summing over it means something: operations and waiters sum to a total; threads do
  not (`backload` is `active + pending`, `open` includes `active`), so each thread
  quantity is its own gauge.
- Not mapped: the `cn=Operations` aggregate (the sum over `op`), `cn=Uptime` (it is
  `time() - openldap_start_time_seconds`, the `process_start_time_seconds` convention),
  `cn=Current,cn=Time`, the per-listener connection totals (`cn=Listener N`, 2.7 only —
  they sum to `openldap_connections_total`, and the listener URI is duplicated per address
  family), the writable log levels under `cn=Log`, the per-connection entries under
  `cn=Connections` (the search excludes them), and the string-valued entries (`cn=State`, `cn=Runqueue`,
  `cn=Tasklist`, a database's backend name).
- The exporter's own families follow the same rules: `openldap_up` (1 when this scrape's
  read succeeded, 0 when it did not — the exporter's own observation of its attempt, so
  0 is a reading, not missing evidence), and
  `openldap_exporter_dials_total{result}` / `openldap_exporter_binds_total{result}`.

A test runs the table against recorded `cn=Monitor` dumps of every image we ship (2.7.1
and `-ol26`) and fails on any numeric entry that is neither mapped nor listed as not
mapped. A new slapd release therefore cannot add or drop an entry silently; it turns our
build red.

Deferred until a real need is identified or requested:

- `--compat-upstream`, emitting upstream's three families alongside the curated ones.
- A `--mapping-file` flag that replaces the compiled-in table. The table is data, so
  either is cheap to add. A user-editable mapping is not offered now because our shipped
  dashboards and rules depend on fixed names, and a renamed series fails silently: under
  decision 5 it reads as a missing reading, and the alert built on it never fires.

### 5. Unreadable evidence is never exported as zero

A reading that could not be taken is absent from the scrape, or carried on a companion
series that says so. It is never `0`, and never the last known value. This is ADR-008's
rule, restated for a surface that cannot express `Unknown`: a gauge that reports `0` for
"no evidence" produces an alert that falls silent exactly when the thing it watches breaks.

Where `0` is a legitimate observation, it is defined per series:

| Series | 0 means | Absent means |
|---|---|---|
| `csn_pairs_read` | the database was attempted and every read failed | the database was not attempted |
| `csn_spread_seconds` | two or more pods answered and they agree | no measurement was taken |
| `peer_providers_discovered` | discovery ran and resolved to nothing | the peer does not discover, or discovery could not be asked |
| `suffix_probe_ok` | the pod was probed and the answer was bad | the pod could not be probed |

Two of these depend on details that are easy to undo:

- The spread is a `*time.Duration`, and the pointer is the only gate. A plain `Duration`
  renders a measurement nobody took as perfect convergence.
- A failed discovery is absent, not `0`. Discovery fails for local causes (an unparseable
  kubeconfig Secret, a remote API that does not answer), and a `0` would send the reader
  to a remote site that is fine. The failure still shows: the peer reads `Unreachable`,
  which Surface 1 exports. Check that cross-surface dependency before changing either side.

### 6. The monitor database is operator-converged (R1) and always present

On every pod, RW and RO, the operator adds the monitor database (`olcRootDN:
cn=admin,cn=config`) when absent and replaces its ACL only on mismatch. Steady state is one
read per pod per pass and zero writes.

- **The ACL is a constant**, independent of the database set:

  `to * by dn.children="cn=slaptain-auth" read by dn.children="cn=peercred,cn=external,cn=auth" read by * none`

  The first clause admits every node-local replication identity (ADR-027), which the
  operator already binds as. The second admits any local `ldapi` peer, whatever its
  UID/GID (measured, including the shape a randomised-UID platform produces); no
  `olcAuthzRegexp` is needed. The grant is no wider than pod access, since only containers
  in the pod reach the socket. The legacy `cn=replication,<suffix>` is not granted.
- **No switch.** An off-transition would mean deleting a `cn=config` database and
  renumbering every database after it (ADR-019). Only the exposure, the sidecar, is opt-in
  (decision 4). The resting cost is the monitor tree's memory.
- **Create order decides per-database statistics.** `olmMDB*` attach when an mdb database
  opens, and only if the monitor database already exists then. So the SlapdDatabase
  controller ensures the monitor database immediately before every mdb ADD, not on every
  pass. A pod whose databases predate its monitor database gets statistics at its next
  restart; enabling the sidecar provides one.
- `back_monitor` is a static backend in every slaptain image, 2.7 and `-ol26`; nothing
  loads it. `olcMonitoring` is on by default for mdb and is not converged.

A live `olcAccess` replace on the monitor database is safe on 2.7.1: 71 replaces at ~0.3 s
each, the pod serving throughout. Concurrent `cn=Monitor` reads may see `Server is busy
(51)` while a write runs.

### 7. Monitoring is site-local

Each site's Prometheus scrapes only what runs at that site, all three surfaces, over
ClusterIP (ServiceMonitor/PodMonitor). Nothing on the monitoring layer crosses a site
boundary. No Prometheus at a site means no monitoring of that site; that is the platform's
call.

Every central-collection design (API-server pod proxy, remote-write agents, per-pod
NodePorts, direct pod-IP scraping) costs a new cross-site dependency, credential or
exposure. The global view we need already exists locally at every site: the operator's
cross-site judgments (decision 3) and CR status.

- No surface emits a `site` label, and rules and dashboards need no `on(site)` join. Peer
  sites appear where the operator reports on them, under its peer labels.
- ServiceMonitors use `honorLabels: true` and drop the target's `pod` label, so the
  `namespace`/`pod` labels from kube-state-metrics and the operator stay as they are
  instead of becoming `exported_*`.
- Viewing several sites side by side (one Grafana, one data source per site) is a viewing
  convenience, not scraping.

## Consequences

- **A dashboard spans three scrape targets**, with three independent reachability stories;
  `up` is what makes a quiet panel diagnosable.
- **Surface 2 is no fresher than the operator's 60 s tick**, and lag is observable only
  while writes flow (ADR-008 amendment), so an idle but broken link can read `Synced`. The
  discovered-provider count covers part of that gap.
- **Cardinality is bounded by pods × databases**, both small by design
  (`ridLayout.podsPerSite` ≤ 15, ADR-028). Nothing per entry or per connection is exported.
- **Surface 1 needs no slaptain release**: condition alerting works against any operator
  version that writes the conditions.
- **Only Surface 3 sees saturation** — connection exhaustion, thread starvation, LMDB fill.
  It is opt-in, so a cluster without it gets no warning before these hit.
- **The surfaces fail differently.** Losing the operator costs cross-site judgment and
  leaves pod counters flowing; losing a pod costs its counters and leaves the operator
  reporting it unreachable.
- **Every new metric is checked against decision 1**, including those that look like
  operator knowledge: a CR status field is Surface 1's even though the operator wrote it.
  `operator/internal/obsmetrics/boundary_test.go` makes every Surface 2 family declare
  its provenance and fails one whose provenance is a CR status condition.

## Related

- ADR-002 — status churn; why Option 3's write cost is not the objection.
- ADR-005 — finalizer retry; why database eviction also happens on deletion intent.
- ADR-008 — CSN monitoring; decision 5 carries its rule onto a surface without `Unknown`.
- ADR-013 — roll triggers; why the sidecar is opt-in and has its own image entry.
- ADR-019 — `olcDatabase={N}` renumbering; why the monitor database has no switch.
- ADR-024 — placement classes; the monitor database is R1.
- ADR-025 — suffix health; the per-pod probe and glue families.
- ADR-027 — node-local replication identity; the monitor ACL's first clause.
- ADR-028 — `SITE_NAME`; why `site` has no role in monitoring.
- ADR-030 — readiness; `PodsResponsive` and its transition age.
- ADR-031, ADR-032 — cross-site links; the discovered-provider count.

## Revision history

Earlier amendments are folded into the decisions above; other files cite them by date.

| Date | Change |
|---|---|
| 2026-10-06 | Decisions 1–5; Surface 1 live |
| 2026-10-07 | Surface 2's family list narrowed to decision 3's eight; CR-status quantities stay on Surface 1; Option 3 costed and rejected |
| 2026-10-08 | Decision 6 (monitor database R1, constant ACL, create order); decision 4 becomes our own exporter, its own binary, `spec.images.exporter`, opt-in |
| 2026-10-09 | Decision 7 (site-local) |
| 2026-10-11 | Decision 4: curated, correctly typed families replace upstream-compatible names (breaking) |
| 2026-10-11 | Decision 4: the exporter gets its own module, image and version line; the operator defaults to a compiled-in exporter version and passes no arguments |
