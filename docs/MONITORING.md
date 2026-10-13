# Monitoring slaptain

What slaptain exposes to a Prometheus, where each quantity comes from, and what
it cannot tell you. The boundary between the three possible sources is
**ADR-033**; this is the user-facing half of it.

## What exists today

**Surface 1 only: CR state.** The operator records what it observes — phase,
replica counts, conditions with their reasons, per-peer cross-site verdicts — in
`SlapdCluster` and `SlapdDatabase` status. kube-state-metrics'
`custom-resource-state` turns that into metrics. No slaptain code is involved,
nothing runs on the data plane, and it works against any operator release whose
status carries the fields.

Not here yet, and deliberately:

- **Surface 2**, quantities the operator computes but does not record in status —
  the measured CSN spread behind the `ReplicationConverged` boolean, per-pod
  suffix-probe outcomes. Wants a collector in the operator.
- **Surface 3**, pod-local slapd internals — connections, operations by type,
  threads, LMDB fill. Needs a `cn=monitor` database, which is not yet safe to
  converge: modifying a monitor database's `olcAccess` on a running 2.7.1 pod
  hangs slapd outright (`docs/BACKLOG.md`, commit `07b89af`). Until that is
  settled there is no monitor tree for an exporter to read, and
  `openldap_exporter` pointed at a slaptain pod would read nothing.

So: saturation is invisible today. Connection exhaustion, thread starvation and
a filling LMDB will arrive unannounced. Everything about *replication health and
mesh topology* is covered.

## Install

The scrape layer is per site and carries no per-site fact, so one values file
goes to every site unchanged. The observing layer goes to exactly one site.

```bash
make monitoring-repo

# every site
for ctx in <site-1> <site-2> <site-3>; do
  make monitoring-ksm-install CONTEXT="$ctx"
done

# one observing site
cp monitoring/kube-prometheus-stack-values.yaml \
   monitoring/kube-prometheus-stack-values.local.yaml   # gitignored
$EDITOR monitoring/kube-prometheus-stack-values.local.yaml   # the scrape targets
make monitoring-stack-install CONTEXT=<site-1> \
  MONITORING_STACK_VALUES=monitoring/kube-prometheus-stack-values.local.yaml

# a URL for the dashboard
make monitoring-gateway-apply CONTEXT=<site-1> \
  MONITORING_GRAFANA_IP=<free-ip> MONITORING_PROMETHEUS_IP=<free-ip>
```

Grafana then answers on its Gateway address (`admin`, password in the stack
values) and the dashboard is at `/d/slaptain-mesh/`. Pinning the addresses is
optional — unset, LB-IPAM assigns what is free and
`kubectl -n monitoring get gateway -o wide` reads them back.

**One Gateway per app**, which is not the obvious shape. One Gateway with two
listeners and a `sectionName` per HTTPRoute does not work when both routes are
catch-all: Cilium merges the routes attached to a Gateway into one Envoy
virtualhost and requests end up split across both backends — measured on a lab,
where the Prometheus listener answered Grafana's login redirect. Hostname-based
routes would need DNS for names that do not exist yet, and path-based routes
would need Grafana's `root_url` and Prometheus' `routePrefix` reconfigured. Two
Gateways cost one extra address.

Where there is no Gateway API and no LoadBalancer, `make monitoring-grafana` and
`make monitoring-prometheus` port-forward instead.

One Prometheus holds every site, as three static scrape jobs each stamped with
a `site` label. The label is attached at the scrape target and not read from the
CR, because a mesh-scoped CR is byte-identical at every site by ADR-028 — nothing
in it says where it came from.

**Changing the exporter config needs a pod roll.** The kube-state-metrics chart
puts no checksum annotation on the `custom-resource-state` ConfigMap, so a
`helm upgrade` leaves the running pod on the old config — and the only symptom
is a label quietly missing from the output. `make monitoring-ksm-install` rolls
the deployment for this reason.

**Addressing the scrape targets.** The targets are node addresses on each site's
*cross-site* interface, and the committed values file carries RFC 5737
placeholders for them. There is no discovery path: on a dual-homed node the NIC
a workstation reaches is a different one and is not registered with Kubernetes —
the same trap `E2E_NODE_ACCESS_IP` exists for in the e2e suite. Keep the real
addresses in a gitignored `*.local.yaml` copy.

For the same reason a NodePort is a poor front door for the dashboard, while a
LoadBalancer address from the cluster's own pool is reachable without knowing
which NIC is which.

## The metrics

Every series carries `site` (from the scrape target) and `namespace`. Cluster
series additionally carry `cluster` and `mesh`; database series carry `cluster`
and `database`.

| Series | Meaning |
|---|---|
| `slaptain_cluster_info{phase,replication_mode,mesh}` | 1 per cluster; phase, ADR-010 mode and the referenced mesh as labels |
| `slaptain_cluster_replicas_ready` / `_desired` | read-write replicas |
| `slaptain_cluster_readonly_replicas_ready` / `_desired` | read-only consumers |
| `slaptain_cluster_observed_generation` | generation last reconciled |
| `slaptain_cluster_condition{condition,reason}` | 1 True, 0 False |
| `slaptain_cluster_condition_last_transition_seconds{condition,reason}` | unix time of the last transition |
| `slaptain_cluster_peer_connected{peer,replication_state}` | cross-site link reachability, with its CSN verdict as a label |
| `slaptain_cluster_peer_lag_seconds{peer}` | max per-database CSN delta — **absent unless Lagging** |
| `slaptain_cluster_peer_last_checked_seconds{peer}` | unix time the CSN check last ran |
| `slaptain_database_info{phase,suffix}` | 1 per database |
| `slaptain_database_condition{condition,reason}` | 1 True, 0 False |
| `slaptain_database_condition_last_transition_seconds{condition,reason}` | unix time of the last transition |

### Scoping: the labels follow the data model

Every reference in slaptain resolves **within one namespace** — a SlapdCluster
finds its mesh by name in its own namespace (`mesh_resolve.go`), a SlapdDatabase
finds its cluster the same way (`slapddatabase_controller.go`). So:

- a mesh is identified by **(namespace, name)**. The name alone is not unique —
  two namespaces may each hold a `slapd-mesh`;
- a mesh is referenced by **0..n** SlapdClusters, so a site can run several
  clusters of one mesh, and a mesh can have none at all;
- a cluster is referenced by **0..n** SlapdDatabases and SlapdSchemas.

The dashboard's selectors are chained in exactly that order — namespace → mesh →
cluster → site. `mesh` is single-select because a headline such as *links
synced* is a ratio over one mesh's links, and an "All" would merge two meshes
into one number that means nothing. `cluster` and `site` are multi-select with
All, and every row label names its cluster, because a mesh may hold more than
one per site.

A **database series carries no mesh label** — a SlapdDatabase knows only its
`clusterRef`. Databases are therefore scoped through the selected mesh's cluster
set rather than by a label of their own, which keeps every query join-free. A
cluster with no `meshRef` carries no `mesh` label and does not appear on the
mesh dashboard at all.

Both of these label paths were wrong on the first attempt and failed *silently*:
`meshRef` and `clusterRef` are plain strings, not objects, and a
`custom-resource-state` path that resolves to nothing drops the label without
logging anything. Check that the labels you expect are on the output.

### Three properties worth knowing before you write a rule against them

**A reason is a label, not a series.** `DataPresent` false with reason
`GlueSuffix` and with reason `DataMissingOnReadOnlyPods` are a permanent
corruption and a transient initial sync respectively. They get different fuses,
and that is only expressible because the reason rides along.

**Absence is a value.** `peer_lag_seconds` does not exist on a healthy link, and
a condition whose status is `Unknown` produces no sample rather than a `0`.
Unreadable evidence never counts toward healthy (ADR-008), and a gauge that
reports `0` for "no evidence" goes quiet exactly when the thing it watches
breaks. Alert on the *absence* of recent observation —
`time() - slaptain_cluster_peer_last_checked_seconds` — not only on bad values.

**Two conditions are inverted.** `ReplicationModePeerWiringRequired` and
`ReplicationModeDemoteWiringRequired` report True when a human still owes the
cluster some wiring. A healthy peer-mode cluster has one True and one False, so
a generic "condition == 0 is bad" rule is wrong for them. The shipped rules and
the shipped dashboard both exclude them.

## Alert rules

`monitoring/prometheus-rules.yaml`, applied by `make monitoring-rules-apply`.
Alertmanager is not deployed — the rules are visible as pending/firing in the
Prometheus UI, and routing them somewhere is a decision about someone's on-call
rather than about slaptain.

The fuses encode what the docs already say to treat differently: `GlueSuffix`
fires with no `for` at all, `DataMissingOnReadOnlyPods` waits 20 minutes, and
`SlaptainPeerStatusStale` exists because every other series would hold its last
value and read healthy if the operator stopped observing.

## What the dashboard shows

`monitoring/dashboards/slaptain-mesh.json`, provisioned by
`make monitoring-dashboard-apply`. The JSON in the repo is the source of truth —
a dashboard edited in the Grafana UI must be exported back over that file, or
the next run of the target discards the edit.

The centre panel is one row per **ordered** site pair. A→B and B→A are separate
rows because peer status is consumer-side and one-directional (ADR-008
amendment): a cut in one direction legitimately shows one row red and its mirror
green, and an idle-but-broken link reads `Synced` on both until writes flow.

## Exercising it

`monitoring/demo/cut-inbound.yaml` cuts inbound LDAP to one site's slapd pods
with a NetworkPolicy — reversible, no pod churn, probes unaffected (they are
exec probes on loopback, ADR-030).

```bash
make monitoring-demo-cut     CONTEXT=<site-3> CILIUM_CLUSTER=<site-3's cilium cluster-name>
# ~60-120s later: the other sites report site-3 Unreachable.
# Check site-3 itself too — see below.
make monitoring-demo-restore CONTEXT=<site-3>
```

`CILIUM_CLUSTER` is required and the manifest ships with a placeholder, so it
cannot be applied blind — which for a fault injection is a feature.

It is a `CiliumNetworkPolicy` matching `fromEndpoints` on the cluster label,
and the first version got this wrong in a way worth repeating. That version
allowed the site's own pod CIDR through an `ipBlock`, reasoning that an address
block cannot accidentally match remote ClusterMesh pods the way a selector can.
True — but **Cilium does not test `ipBlock` against in-cluster sources at all**:
it resolves them to their pod identity, and `ipBlock` only matches entities it
considers outside the cluster. So the policy was a deny-all-ingress that also
blinded the site's own operator, which timed out dialling pods inside the
allowed block.

**The cut form is unverified; the blackout form was verified and found one
operator bug.** With inbound fully blocked, site-3's own view went
`connected: false` for *both* peers with no `replicationState`, no `lastError`
and no `lastChecked` — because the operator skips the peer check when it cannot
read its own databases, and then derives `connected` from the unset verdict. A
site that cannot read itself blames its healthy peers. `docs/BACKLOG.md` carries
it; the shipped rules work around it with `SlaptainPeerUnverified`, which keys
on the *absence* of `lastChecked` and names the right site.

Expect the state change on the order of the operator's 60 s CSN tick, not
instantly. Nothing here is fresher than that pass, and a scrape interval below
it only resamples the same reading.

## Related

- ADR-033 — the three surfaces and which metric belongs to which.
- ADR-008 — what CSN comparison can and cannot observe.
- ADR-030 — readiness, `PodsResponsive`, and why there is no liveness probe.
- ADR-025 — the glue-suffix failure the `DataPresent` reasons distinguish.
- `docs/MULTI-SITE.md` — the condition reasons and their operational meaning.
- `docs/TUNING.md` — LMDB sizing, which is the saturation story Surface 3 owes.
