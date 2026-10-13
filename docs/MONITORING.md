# Monitoring slaptain

What slaptain exposes to a Prometheus, where each quantity comes from, and what
it cannot tell you. The boundary between the three possible sources is
**ADR-033**; this is the user-facing half of it.

## What exists today

**Surface 1: CR state.** The operator records what it observes — phase, replica
counts, conditions with their reasons, per-peer cross-site verdicts — in
`SlapdCluster` and `SlapdDatabase` status. kube-state-metrics'
`custom-resource-state` turns that into metrics. No slaptain code is involved,
nothing runs on the data plane, and it works against any operator release whose
status carries the fields.

**Surface 2: what the operator measures but does not record.** A collector in
the operator exports the quantities a reconcile pass computes and then throws
away — the CSN spread behind the `ReplicationConverged` boolean, how many
(pod, database) pairs actually yielded a reading, the per-pod suffix probe, the
discovered provider count per cross-site link. Before it existed the operator
knew *"could not read contextCSN for 6 of 6 (pod, database) pairs"* and
published that only as English inside a condition message: unalertable,
ungraphable. It is now two numbers.

**Surface 3: pod-local slapd internals**, opt-in — a sidecar exporter next to
every slapd pod, switched on by `spec.monitoring.enabled` and scraped through
the operator chart's `slapdPodMonitor`. See
[Surface 3: the exporter sidecar](#surface-3-the-exporter-sidecar) below. With
it off, slapd's own load is invisible: connection growth, a filling LMDB and an
operation storm arrive unannounced. Everything about *replication health and mesh
topology* is covered by Surfaces 1 and 2 alone.

## Monitoring is site-local

Each site's Prometheus scrapes only what runs at that site — its own
kube-state-metrics, its own operator, its own slapd exporter sidecars — over
ClusterIP, through `ServiceMonitor`s and a `PodMonitor`. Nothing on the
monitoring layer reaches across a site boundary, and no site needs a
credential, a route or an open port into another (ADR-033 amendment
2026-10-09). No Prometheus at a site means no monitoring of that site; that is
the platform's call.

So **no series carries a `site` label**, from any surface. Which site a series
came from is which Prometheus holds it. The alert rules and the dashboards join
on `namespace`, `cluster`, `mesh` and the scope label, never on a site.

The only mesh-wide view is the one the operator already holds: its cross-site
judgments — peer status per link, discovered providers, convergence — exported
locally at every site under the `peer` label. What that costs:

- **The two directions of one link live in two Grafanas.** Peer status is
  consumer-side (ADR-008 amendment): site A's operator reports A's view of B,
  and B's view of A is on B's dashboard. A cut in one direction shows red at
  one site and green at the other, and you have to look at both.
- **A dark site is seen through its peers' judgments.** If a site's own
  Prometheus is down or unreachable, nothing from that site is visible; its
  peers' operators still report it `Unreachable` or lagging, on their own
  dashboards.
- **Several sites side by side** is a viewing convenience: one Grafana with a
  data source per site. Both dashboards have a `datasource` selector for exactly
  that. It is not scraping and changes nothing above.

## Install

Every site gets the same thing, in this order — the monitors in steps 2 and 3
need the Prometheus Operator CRDs that step 1 brings, and a `helm install` that
renders a `ServiceMonitor` or `PodMonitor` fails outright without them:

1. **kube-prometheus-stack** (`monitoring/kube-prometheus-stack-values.yaml`),
   then the alert rules and the dashboards;
2. **kube-state-metrics** (`monitoring/kube-state-metrics-values.yaml`) with
   its `ServiceMonitor` on;
3. **the operator** with `metrics.enabled`, `metrics.serviceMonitor.enabled`
   and `slapdPodMonitor.enabled`;
4. the `SlapdCluster`s (with `spec.monitoring.enabled` for Surface 3).

**Where the files come from.** The values files, the alert rules and the
dashboards ship only in this repository, under `monitoring/` — no chart and no
image carries them. So both sequences below run from a checkout, with
repo-relative paths, and **assume that checkout's `monitoring/` matches the
operator release you run**: the rules and dashboards reference the metric names
and labels of that operator and its exporter, and a mismatch shows up as panels
and rules that silently match nothing. The
release name `monitoring` and the namespace `monitoring` are not arbitrary: the
stack values point Grafana's dashboard sidecar at namespace `monitoring`, and
`monitoring/gateway.yaml` names the `monitoring-*` Services.

Per site, with plain `helm` and `kubectl`:

```bash
ctx=<site-context>      # run from the repository root

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update prometheus-community

# 1. The stack (brings the Prometheus Operator CRDs), then rules and dashboards.
helm --kube-context "$ctx" upgrade --install monitoring \
  prometheus-community/kube-prometheus-stack --version 92.2.0 \
  -n monitoring --create-namespace \
  -f monitoring/kube-prometheus-stack-values.yaml --wait --timeout 10m

kubectl --context "$ctx" apply -f monitoring/prometheus-rules.yaml

kubectl --context "$ctx" create configmap slaptain-dashboards -n monitoring \
  --from-file=monitoring/dashboards/ --dry-run=client -o yaml |
  kubectl label --local -f - grafana_dashboard=1 -o yaml --dry-run=client |
  kubectl --context "$ctx" apply -f -

# 2. kube-state-metrics with the custom-resource-state config (Surface 1).
helm --kube-context "$ctx" upgrade --install ksm \
  prometheus-community/kube-state-metrics --version 8.6.0 \
  -n monitoring -f monitoring/kube-state-metrics-values.yaml --wait
# Only when upgrading an existing ksm release — see "needs a pod roll" below:
# kubectl --context "$ctx" -n monitoring rollout restart deployment/ksm-kube-state-metrics

# 3. The operator already installed at this site: switch its monitors on.
#    --version is the chart version it runs NOW (`helm -n slaptain-system list`);
#    without it helm pulls the newest chart, which is an operator upgrade.
helm --kube-context "$ctx" upgrade slaptain \
  oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain --version <installed-version> \
  -n slaptain-system --reuse-values \
  --set metrics.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --set slapdPodMonitor.enabled=true

# 4. Surface 3, per SlapdCluster. Rolls that cluster's pods once.
#    Through the charts: monitoring.enabled=true (slapd) or
#    cluster.monitoring.enabled=true (slapd-mesh — in the shared values file,
#    so every site at once). Or on the CR directly:
kubectl --context "$ctx" -n <namespace> patch slapdcluster <name> --type=merge \
  -p '{"spec":{"monitoring":{"enabled":true}}}'
```

A `SlapdCluster` managed by Helm should be switched in its values, not patched:
the next `helm upgrade` would revert a patch that its values do not carry. The
sidecar is harmless at a site without a Prometheus — nothing scrapes it — so the
mesh chart's one shared value is not a problem for a site that has none.

**The same in make targets** — steps 1 and 2 in one, with
the chart versions pinned in the Makefile:

```bash
make monitoring-repo

for ctx in <site-1> <site-2> <site-3>; do
  make monitoring-install CONTEXT="$ctx"          # steps 1 and 2
  # step 3, on the operator already installed at that site:
  helm --kube-context "$ctx" upgrade slaptain ./charts/operator \
    -n slaptain-system --reuse-values \
    --set metrics.enabled=true \
    --set metrics.serviceMonitor.enabled=true \
    --set slapdPodMonitor.enabled=true
done

# optional: a URL for each site's Grafana
make monitoring-gateway-apply CONTEXT=<site> \
  MONITORING_GRAFANA_IP=<free-ip> MONITORING_PROMETHEUS_IP=<free-ip>
```

`make monitoring-dashboard-apply CONTEXT=<site>` alone reloads the dashboards:
it puts every `monitoring/dashboards/*.json` into the `slaptain-dashboards`
ConfigMap and replaces it. The gateway is a plain `kubectl apply -f
monitoring/gateway.yaml` without the make target; the target only adds the
optional address pinning.

**Step 3 on an existing operator is `helm upgrade --reuse-values`, not
`make operator-helm-install`.** The make target sets `image.repository` and
`image.tag` from the checkout it runs in. On an operator already installed from
a release, that swaps the operator image — and with it every slapd and init
image left unpinned, so those pods roll — or leaves the operator in
`ImagePullBackOff` if the checkout's tag was never pushed. (The exporter sidecar
is not affected: it has its own version line, see below.) Use the make target
(with the same three `--set`s in `HELM_ARGS`) only for an install that runs this
checkout's own images.

**The charts are pinned:** `MONITORING_STACK_VERSION` (kube-prometheus-stack
92.2.0) and `MONITORING_KSM_VERSION` (kube-state-metrics 8.6.0), which the values
files are written against. Helm never upgrades a chart's `crds/`, so on a
kube-prometheus-stack **major** bump apply the new Prometheus Operator CRDs
yourself before the upgrade — the chart's upgrade notes name the command.

No values file carries a per-site fact, so all of them are applied unchanged.
The shipped stack selects `ServiceMonitor`s, `PodMonitor`s and
`PrometheusRule`s regardless of release labels
(`*SelectorNilUsesHelmValues: false`). Against a Prometheus of your own that
selects by label, set `metrics.serviceMonitor.labels` and
`slapdPodMonitor.labels` to match.

**`honorLabels: true` on both ServiceMonitors, and why.** kube-state-metrics'
custom-resource series carry the CR's own `namespace`, and the operator's
series carry the `namespace` of the SlapdCluster and, per pod, the slapd
`pod`. A ServiceMonitor without `honorLabels` lets Prometheus overwrite those
with the scrape target's own namespace and pod — the monitoring namespace, the
operator pod — and moves the originals to `exported_namespace` /
`exported_pod`. Nothing errors; every rule, join and dashboard query written
against `namespace` or `pod` silently matches nothing. The operator chart's
ServiceMonitor and the shipped kube-state-metrics values both set it. The slapd
`PodMonitor` does not need it: the exporter emits no `namespace` or `pod`, so
those labels are the target's, and correct. The converse trap is handled too:
both ServiceMonitors drop the target's own `pod` label (a `labeldrop`
relabeling), because with `honorLabels` a series that carries no `pod` of its
own — every kube-state-metrics series, every cluster- or database-scope
operator series — would otherwise be stamped with the scraped pod's name (the kube-state-metrics
or operator pod).

**The PodMonitor lives in the operator chart, not the cluster charts.**
`charts/slapd-mesh` renders byte-identically at every site (ADR-028); a
`PodMonitor` there would fail the whole release at a site without the CRDs.
Whether a site runs a Prometheus is a per-site fact, like `siteName`, so the
switch sits in the per-site chart. It selects `app.kubernetes.io/name: slapd`
pods on port `metrics` in every namespace (`slapdPodMonitor.namespaceSelector`
narrows it), and labels every target `job="slapd-exporter"`, plus `cluster` and
`role` read off the pod's `app.kubernetes.io/instance`: read-only pods carry
`<cluster>-readonly`, so the suffix is stripped for `cluster` and sets
`role="readonly"`. A SlapdCluster whose own name ends in `-readonly` is
therefore misreported as the read-only fleet of the shorter name — don't name
one that way.

**Read the exposure before you turn it on.** The chart passes
`--metrics-secure=false`, so the operator's endpoint is plain HTTP with **no
authentication and no authorization**, reachable from every pod in the cluster.
The exporter sidecar's `:9330` is the same. Keep the metrics Service at its
`ClusterIP` default; the site's own Prometheus is in-cluster and needs nothing
more. On a shared cluster, enable `networkPolicy.enabled` in the same change and
label the Prometheus namespace `metrics=enabled`
(`kubectl label namespace monitoring metrics=enabled`) — the policy admits only
labelled namespaces, so an unlabelled one shuts the scraper out too. It covers
the operator's port, not the exporter's. The series carry namespaces, cluster
and mesh names, database names, pod names and condition reasons — no
credentials, but a map of the deployment.

Grafana then answers on its Gateway address (`admin`, password in the stack
values) and the dashboards are at `/d/slaptain-mesh/` and `/d/slaptain-slapd/`. Pinning the addresses is
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

**Upgrading from the central observer.** An installation from before the
site-local amendment had one Prometheus scraping every site through
`additionalScrapeConfigs` and NodePorts. Upgrading the stack with the current
values drops those jobs. Two things follow, both expected:

- the old jobs' `site`-labelled series stay visible for about five minutes,
  until Prometheus marks them stale. A rule evaluated over them in that window
  can go pending — a transient `SlaptainPeerStatusStale` is the usual one — and
  clears on its own;
- the old observer keeps the other sites' history for its full retention
  (7 days in the shipped values). Dashboard ranges reaching back before the
  upgrade mix every site's series into this site's panels. For a clean slate,
  delete that Prometheus's PVC and let it start empty.

The kube-state-metrics Service changes from NodePort to ClusterIP with the
values file. The operator's does not: `--reuse-values` keeps a
`metrics.service.type=NodePort` set for the old observer, so add
`--set metrics.service.type=ClusterIP` to step 3. Nothing outside the site needs
to reach either any more.

**Changing the exporter config needs a pod roll.** The kube-state-metrics chart
puts no checksum annotation on the `custom-resource-state` ConfigMap, so a
`helm upgrade` leaves the running pod on the old config — and the only symptom
is a label quietly missing from the output. `make monitoring-ksm-install` rolls
the deployment for this reason.

## Surface 3: the exporter sidecar

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
spec:
  monitoring:
    enabled: true        # opt-in; unset means off
    resources: {}        # optional, the sidecar's own (not spec.resources); no default
```

Charts: `monitoring:` and `images.exporter:` in `charts/slapd`,
`cluster.monitoring:` and `cluster.images.exporter:` in `charts/slapd-mesh`;
passed through only when set.

What it adds, and nothing else: one container named `exporter` on every slapd
pod, read-write and read-only alike, after the slapd container. It runs the
`slapd-exporter` image with no command and no arguments, so the exporter's own
defaults apply: it binds SASL EXTERNAL over the pod's ldapi socket (`/run/openldap`, the existing emptyDir, mounted
read-only) — no password, no Secret — and serves `openldap_*` metrics on
`:9330/metrics` (port name `metrics`). It carries slapd's container hardening
and sets no UID of its own: it runs as the pod securityContext's `runAsUser`
when that is set (the default), otherwise as the image's `USER` — harmless
either way, since the monitor ACL does not depend on the UID.

**It can still make the pod NotReady.** The sidecar has no probe, so a slow or
failing scrape never reads as a false-negative readiness. But Ready is the AND
of all containers: a sidecar whose image cannot be pulled, or that
crash-loops, keeps the pod NotReady like any container would — and under
`OrderedReady` a stuck first pod halts the whole rollout. The exporter's image
must be pullable on every node that runs slapd; when its registry is private,
`spec.imagePullSecrets` must cover it too.

**The image is `spec.images.exporter`.** The exporter is its own module
(`exporter/`), image (`slapd-exporter`) and version line, with git tags
`exporter/vX.Y.Z` and image tags `X.Y.Z` (`docs/VERSIONING.md`, ADR-033
decision 4). Unset, the repository is derived from the operator's like `slapd`
and `init`, with the trailing path segment swapped for `slapd-exporter`, and the
tag is the exporter release **compiled into the operator**, not the operator's
own tag. Without `OPERATOR_IMAGE` (`make run`) the repository falls back to
`ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-exporter`. A tag alone keeps the
derived repository; repository and tag are used exactly. Pull policy: once an
`images.exporter` object exists at all, the CRD default stores `IfNotPresent`
in it, so that is what the sidecar gets unless you set another; only with the
object absent is the policy left to the API server's tag-based default
(`Always` for `:latest`, `IfNotPresent` otherwise).

**An operator upgrade leaves the sidecar alone** unless the compiled-in
exporter release moved: two operators whose images differ only in tag render an
identical sidecar. So leaving `images.exporter` empty is the normal setting, and
it is **not** pinned in lockstep with `slapd` and `init`. Pin it to run a
different exporter version, such as a newer release, a mirror or a local build.
Pinning does not stop every roll: a future operator that changes the sidecar's
pod-side shape (mounts, port, securityContext) rolls a pinned cluster too.

**The interface between the two is the exporter's defaults.** The operator
passes no command and no arguments. The image's entrypoint and the exporter's
flag defaults (`ldapi:///run/openldap/slapd.ldapi`, SASL EXTERNAL,
`:9330/metrics`) are the whole contract. The exporter keeps those defaults
stable across versions, and the operator owns only the pod-side facts: the port
name `metrics`, the read-only socket mount and the security context. The
shipped rules and dashboards follow the exporter's version, since they query its
families (`monitoring/README.md`).

**Flipping it rolls the pods once.** The sidecar is part of the pod template,
so turning it on or off changes the template and the StatefulSet replaces every
pod, one at a time. Unset and `enabled: false` produce a template identical to
an operator without the feature, so upgrading the operator rolls nothing on a
cluster with monitoring off (ADR-013 amendment 2026-10-08). `resources` while `enabled` is off configures
nothing and rolls nothing.

**Resources: measured, not guessed.** On a three-site lab (OpenLDAP 2.7.1,
five mdb databases per read-write pod, 64 `openldap_*` series per scrape
with the earlier, upstream-shaped families (the curated ones are 75 on the same
pod),
`metrics-server` readings), the sidecar used 0–1m CPU and 3–7 MiB of memory
while nothing scraped it. While every pod was scraped once a second for 60 s,
far above any usual Prometheus interval, it peaked at 18m CPU and 9 MiB. If a
quota or `LimitRange` makes `resources` necessary at all, start from
`requests: {cpu: 10m, memory: 16Mi}` and `limits: {memory: 32Mi}`. Leave out a
CPU limit, or set it no lower than `100m`: a throttled scrape that runs past
`--timeout` reads as an absent reading, not as a slow one. A scrape's size
follows the number of databases, threads and operation types, not the
directory's entry count, so re-measure on a server with many more databases
rather than a bigger one.

Without `OPERATOR_IMAGE` (the operator run locally with `make run`), an
unpinned exporter image falls back to the canonical upstream operator
repository at the operator's tag, the way the slapd and slapd-init images do.

### What the sidecar exports

Each scrape is one subtree search of `cn=Monitor` (user and operational
attributes), taken when Prometheus asks — there is no background poll. The
exporter knows slapd, not slaptain: no `cluster`, `namespace` or `pod` label
comes from it. Scraped through the operator chart's `slapdPodMonitor`, every
series gets `namespace`, `pod` and `container` from the target, plus `job`
(`slapd-exporter`), `cluster` and `role` (`readwrite`|`readonly`) from the
PodMonitor's relabeling — so a fill ratio reads per
`(namespace, cluster, pod, suffix)`. No `site`, like every other surface.

| Family | Type | Labels | Source |
|---|---|---|---|
| `openldap_up` | gauge | — | the exporter: `1` when this scrape read `cn=Monitor`, `0` when the dial, bind or search failed |
| `openldap_exporter_dials_total` | counter | `result` = `ok`\|`fail` | the exporter's own connection attempts |
| `openldap_exporter_binds_total` | counter | `result` = `ok`\|`fail` | the exporter's own bind attempts |
| `openldap_operations_initiated_total` | counter | `op` | `monitorOpInitiated` of `cn=<Op>,cn=Operations` |
| `openldap_operations_completed_total` | counter | `op` | `monitorOpCompleted` of `cn=<Op>,cn=Operations` |
| `openldap_connections_total` | counter | — | `monitorCounter` of `cn=Total,cn=Connections` |
| `openldap_connections_current` | gauge | — | `monitorCounter` of `cn=Current,cn=Connections` |
| `openldap_max_file_descriptors` | gauge | — | `monitorCounter` of `cn=Max File Descriptors,cn=Connections` |
| `openldap_sent_bytes_total`, `_pdus_total`, `_entries_total`, `_referrals_total` | counter | — | `monitorCounter` of `cn=Bytes`, `cn=PDU`, `cn=Entries`, `cn=Referrals` under `cn=Statistics` |
| `openldap_threads_open`, `_active`, `_pending`, `_starting`, `_backload` | gauge | — | `monitoredInfo` of `cn=Open`, `cn=Active`, `cn=Pending`, `cn=Starting`, `cn=Backload` under `cn=Threads` |
| `openldap_threads_max`, `openldap_threads_max_pending` | gauge | — | `monitoredInfo` of `cn=Max`, `cn=Max Pending` under `cn=Threads` |
| `openldap_waiters` | gauge | `direction` = `read`\|`write` | `monitorCounter` of `cn=Read`, `cn=Write` under `cn=Waiters` |
| `openldap_start_time_seconds` | gauge | — | `monitorTimestamp` of `cn=Start,cn=Time`, as Unix seconds |
| `openldap_mdb_pages_max` | gauge | `suffix` | `olmMDBPagesMax` |
| `openldap_mdb_pages_used` | gauge | `suffix` | `olmMDBPagesUsed` |
| `openldap_mdb_pages_free` | gauge | `suffix` | `olmMDBPagesFree` |
| `openldap_mdb_readers_max` | gauge | `suffix` | `olmMDBReadersMax` |
| `openldap_mdb_readers_used` | gauge | `suffix` | `olmMDBReadersUsed` |
| `openldap_mdb_entries` | gauge | `suffix` | `olmMDBEntries` |
| `openldap_mdb_page_size_bytes` | gauge | `suffix` | `olmMDBPageSize` (2.7 only) |

The families are slaptain's own, typed per entry (ADR-033 decision 4): counters
end in `_total`, everything else is a gauge, and a label is used only where
summing over it means something. Operations and waiters sum to a total, so they
carry `op` and `direction`, each the entry's `cn` lowercased (`op="search"`,
`direction="read"`). Thread quantities overlap (`backload` is active plus
pending, `open` includes `active`), so each is a family of its own. The upstream
exporter's shape (one family per monitor objectClass, labelled by entry DN,
everything a gauge) is not kept, so dashboards built for it, such as the Grafana
`openldap-mixin`, do not work against this exporter. The earlier,
upstream-compatible naming came from
[tomcz/openldap_exporter](https://github.com/tomcz/openldap_exporter) (MIT).

Not mapped: the `cn=Operations` aggregate (the sum over `op`), `cn=Uptime`
(it is `time() - openldap_start_time_seconds`, and a restart is a change of the
start time), `cn=Current,cn=Time`, the per-listener connection totals, the log
levels under `cn=Log`, the per-connection entries, and the string-valued entries
(`cn=State`, `cn=Runqueue`, `cn=Tasklist`, a database's backend name). `suffix`
is the database's `namingContexts`. Replication state (`contextCSN`) is
deliberately absent: that is the operator's (Surface 2).

**A reading that could not be taken is absent — never 0, never the last
value.** If the dial, the bind or the search fails, or the whole read exceeds
`--timeout` (default 5s, below Prometheus's 10s scrape timeout), the scrape
still answers 200 but carries only `openldap_up 0` and the exporter's two
counters, and no `cn=Monitor` family at all. `openldap_up` is the one series
where `0` is a reading: the exporter observed its own attempt fail. A sidecar
Prometheus cannot scrape has no `openldap_up` at all; that case is
`up{job="slapd-exporter"} == 0`. The same rule holds per series:

- a database opened **before** the monitor database carries no `olmMDB*`, so it
  has no `openldap_mdb_*` series — not zeros, which would read as an empty
  database. Statistics attach when a database is opened after `cn=Monitor`
  exists; the pod's next restart puts it right (ADR-033, "create order");
- `openldap_mdb_page_size_bytes` does not exist on the `-ol26` image;
- `openldap_exporter_binds_total` is counted only when a bind is attempted, and
  a counter's `result="fail"` series exists only after a first failure.

So alert on `openldap_up == 0` or on a growing
`openldap_exporter_dials_total{result="fail"}`, never on a `cn=Monitor` value
reading 0. A frozen slapd (accepts connections, never answers — ADR-030) reads
as `openldap_exporter_binds_total{result="fail"}` once `--timeout` expires; the
exporter closes that connection, so a freeze does not pile up connections across
scrapes.

### Check it yourself: from `cn=Monitor` to `/metrics`

The exporter invents no data. Every `cn=Monitor` series is one attribute of one
monitor entry, renamed and typed: the name and the type are slaptain's, the
value is slapd's. You can read both sides and compare them line by line.

**The input** is slapd's own monitor tree. Read it as the config admin (the
monitor database's rootDN; `--as admin` is the *data* rootDN and gets `No such
object`). Most values are operational attributes, so ask for `+` as well as `*`.
`cn=Monitor` is per pod, so pin one:

```bash
slctl ldapsearch --as config -n slaptain-testing --pod 0 -LLL -b "cn=Monitor" "*" "+"
```

This is the full tree. The exporter issues the same search with a filter that
leaves out the per-connection entries it never reads:

```
(|(objectClass=monitoredObject)(objectClass=monitorCounterObject)(objectClass=monitorOperation))
```

**The output** is what Prometheus scrapes, on the same pod:

```bash
kubectl port-forward -n slaptain-testing pod/slapd-0 9330:9330 &
curl -s localhost:9330/metrics
```

The binary also runs outside the pod, against a port-forward to slapd's LDAP
port, with a simple bind instead of SASL EXTERNAL:

```bash
slapd-exporter --uri ldap://localhost:<port> --bind simple \
  --bind-dn cn=admin,cn=config --bind-password-file <file> --listen 127.0.0.1:9330
```

Or the image itself, with the same arguments after the image name. Its
entrypoint is the binary. `--user 0:0` lets rootless podman read a `0600`
password file you own:

```bash
podman run --rm --network host --user 0:0 -v <file>:/pw:ro,Z \
  ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-exporter:<tag> \
  --uri ldap://localhost:<port> --bind simple --bind-dn cn=admin,cn=config \
  --bind-password-file /pw --listen 127.0.0.1:9330
```

With no arguments the image runs the sidecar's configuration, which is the
whole contract with the operator:

```
slapd-exporter: reading ldapi:///run/openldap/slapd.ldapi (bind external), serving /metrics on [::]:9330
```

**The mapping** is two tables in `exporter/internal/slapdexporter/slapdexporter.go`.
Each row names a family, its type, the entry it reads and the attribute:

```go
var monitorFamilies = []family{
	{name: "openldap_operations_initiated_total", typ: counter, dn: "cn=*,cn=Operations,cn=Monitor", label: "op",
		attr: "monitorOpInitiated"},
	{name: "openldap_operations_completed_total", typ: counter, dn: "cn=*,cn=Operations,cn=Monitor", label: "op",
		attr: "monitorOpCompleted"},
	{name: "openldap_connections_total", typ: counter, dn: "cn=Total,cn=Connections,cn=Monitor",
		attr: "monitorCounter"},
	{name: "openldap_connections_current", typ: gauge, dn: "cn=Current,cn=Connections,cn=Monitor",
		attr: "monitorCounter"},
	// … cn=Max File Descriptors, the four cn=Statistics counters …
	{name: "openldap_threads_active", typ: gauge, dn: "cn=Active,cn=Threads,cn=Monitor",
		attr: "monitoredInfo"},
	// … the other six cn=Threads gauges …
	{name: "openldap_waiters", typ: gauge, dn: "cn=*,cn=Waiters,cn=Monitor", label: "direction",
		attr: "monitorCounter"},
	{name: "openldap_start_time_seconds", typ: gauge, dn: "cn=Start,cn=Time,cn=Monitor",
		attr: attrTimestamp},
}

// any entry carrying namingContexts, labelled suffix=<namingContexts>
var mdbStatFamilies = []family{
	{name: "openldap_mdb_pages_max", typ: gauge, attr: "olmMDBPagesMax"},
	// … pages_used, pages_free, readers_max, readers_used, entries, page_size_bytes …
}
```

(`help` strings omitted.) A row's `dn` is compared without case; a
`cn=*,<parent>` row matches every direct child of `<parent>` and puts that
child's `cn`, lowercased, in the row's label. `SamplesFromEntries` walks the
search result once, applies both tables, and keeps a value only if it parses (a
number, or a generalizedTime for `monitorTimestamp`). That is the whole
translation.

Side by side, from one read-write pod of the lab (trimmed to the attributes
that matter):

```
# input                                        # output
dn: cn=Search,cn=Operations,cn=Monitor
objectClass: monitorOperation
monitorOpInitiated: 422386                     openldap_operations_initiated_total{op="search"} 422385
monitorOpCompleted: 422385                     openldap_operations_completed_total{op="search"} 422384

dn: cn=Total,cn=Connections,cn=Monitor
objectClass: monitorCounterObject
monitorCounter: 127984                         openldap_connections_total 127982

dn: cn=Current,cn=Connections,cn=Monitor
objectClass: monitorCounterObject
monitorCounter: 11                             openldap_connections_current 11

dn: cn=Active,cn=Threads,cn=Monitor
objectClass: monitoredObject
monitoredInfo: 1                               openldap_threads_active 1

dn: cn=Read,cn=Waiters,cn=Monitor
objectClass: monitorCounterObject
monitorCounter: 11                             openldap_waiters{direction="read"} 11

dn: cn=Start,cn=Time,cn=Monitor
objectClass: monitoredObject
monitorTimestamp: 20261009092724Z              openldap_start_time_seconds 1.791538044e+09

dn: cn=Database 3,cn=Databases,cn=Monitor
objectClass: monitoredObject
monitoredInfo: mdb                             (not a number: no series)
namingContexts: dc=example,dc=org              → suffix="dc=example,dc=org"
olmMDBPagesMax: 8388608                        openldap_mdb_pages_max{suffix="dc=example,dc=org"} 8.388608e+06
olmMDBPagesUsed: 71                            openldap_mdb_pages_used{suffix="dc=example,dc=org"} 71
olmMDBPagesFree: 53                            openldap_mdb_pages_free{suffix="dc=example,dc=org"} 53
olmMDBReadersMax: 126                          openldap_mdb_readers_max{suffix="dc=example,dc=org"} 126
olmMDBReadersUsed: 15                          openldap_mdb_readers_used{suffix="dc=example,dc=org"} 15
olmMDBEntries: 11                              openldap_mdb_entries{suffix="dc=example,dc=org"} 11
olmMDBPageSize: 4096                           openldap_mdb_page_size_bytes{suffix="dc=example,dc=org"} 4096
```

The scrape came first and the search six seconds later, so the three counters
that move with traffic (the search operations and the connection total) read
one or two higher on the input side. Every gauge and every database statistic
matches exactly. `20261009092724Z` is `1791538044` in Unix seconds. The type
travels with the family, in the scrape's own metadata:

```
# TYPE openldap_connections_total counter
# TYPE openldap_connections_current gauge
# TYPE openldap_operations_completed_total counter
# TYPE openldap_threads_active gauge
# TYPE openldap_waiters gauge
# TYPE openldap_start_time_seconds gauge
```

The exporter's own three families, `openldap_up`,
`openldap_exporter_dials_total` and `openldap_exporter_binds_total`, count its
own attempts and have no `cn=Monitor` counterpart.

## The metrics

Every series carries `namespace`. Cluster series additionally carry `cluster`
and `mesh`; database series carry `cluster` and `database`. None carries `site`
— see [Monitoring is site-local](#monitoring-is-site-local).

### Surface 1 — CR state

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

### Surface 2 — what only the operator knows

Eight families. Every one carries `namespace`, `cluster` and `mesh` (empty for a
standalone cluster), plus its own scope label. **None carries `site`**: the
operator knows its own site, but the Prometheus already is that site, and a
label only this surface carried would empty every join on it against the
others.

| Series | Meaning |
|---|---|
| `slaptain_cluster_observation_timestamp_seconds` | unix time of the cluster pass that produced these samples |
| `slaptain_database_observation_timestamp_seconds{database}` | the same, for the database pass |
| `slaptain_csn_spread_seconds{database}` | measured contextCSN spread across the pods that answered — **absent** below two readable pods |
| `slaptain_csn_pairs_read{database}` | pods that yielded a readable contextCSN. **0 is present** — the attempt is the observation |
| `slaptain_csn_pairs_expected{database}` | pods the pass set out to read. `read < expected` means a pod was asked and could not be read |
| `slaptain_peer_providers_discovered{peer}` | remote pods discovery found. **0 when discovery ran and found none; absent when discovery could not be asked at all** |
| `slaptain_suffix_probe_ok{database,pod,role}` | ADR-025 probe: 1 healthy, **0 probed-and-bad**, **absent for a pod that could not be probed** |
| `slaptain_suffix_glue{database,pod,role}` | presence is the signal: that pod's suffix entry is a glue entry |

The `role` label is `readwrite` or `readonly`, and it is load-bearing: ADR-025
gives a read-only pod mid-initial-sync its own reason precisely because that
case is legitimately transient, while the same `0` on a read-write pod is not.
One rule over both is the mistake the label exists to prevent.

### Scoping: the labels follow the data model

Every reference in slaptain resolves **within one namespace** — a SlapdCluster
finds its mesh by name in its own namespace (`mesh_resolve.go`), a SlapdDatabase
finds its cluster the same way (`slapddatabase_controller.go`). So:

- a mesh is identified by **(namespace, name)**. The name alone is not unique —
  two namespaces may each hold a `slapd-mesh`;
- a mesh is referenced by **0..n** SlapdClusters, so a site can run several
  clusters of one mesh, and a mesh can have none at all;
- a cluster is referenced by **0..n** SlapdDatabases and SlapdSchemas.

The dashboard's selectors are chained in exactly that order — data source →
namespace → mesh → cluster. `mesh` is single-select because a headline such as
*links synced* is a ratio over one mesh's links, and an "All" would merge two
meshes into one number that means nothing. `cluster` is multi-select with All,
and every row label names its cluster, because a site may run more than one
cluster of a mesh.

A **database series carries no mesh label** — a SlapdDatabase knows only its
`clusterRef`. Databases are therefore scoped through the selected mesh's cluster
set rather than by a label of their own, which keeps every query join-free. A
cluster with no `meshRef` carries no `mesh` label and does not appear on the
mesh dashboard at all.

Both of these label paths were wrong on the first attempt and failed *silently*:
`meshRef` and `clusterRef` are plain strings, not objects, and a
`custom-resource-state` path that resolves to nothing drops the label without
logging anything. Check that the labels you expect are on the output.

### Joining the two surfaces

The keys below are measured against live data, not derived from the label
names. Getting one wrong does not produce an error — it produces a rule that
never fires, which is indistinguishable from a rule watching a healthy system.

| scope | `on (...)` |
|---|---|
| cluster | `namespace, cluster, mesh` |
| peer | `namespace, cluster, mesh, peer` |
| **database, Surface 1 ↔ Surface 2** | `namespace, cluster, database` — **omit `mesh`** |
| database, Surface 2 ↔ Surface 2 | `namespace, cluster, mesh, database` |
| pod, Surface 2 ↔ Surface 3 | `namespace, cluster, pod` — Surface 3 is keyed by `suffix`, not `database` |

Two traps, both silent:

- **A plain `*` with no `on(...)` matches on every label and returns nothing, at
  every scope**, because `instance` and `job` always differ between the two
  scrape targets.
- **`mesh` at database scope empties the join.** `SlapdDatabase` has no
  `meshRef` field, so kube-state-metrics structurally cannot put a mesh on a
  database series, while Surface 2 gets one because the operator resolves the
  owning cluster during the pass. `(namespace, cluster, database)` is already
  unique.

When a join returns nothing, the index API tells you whether the data or the
query is wrong — a typo in a metric name, a label name or a label value all
look identical from the query side:

```
/api/v1/label/mesh/values?match[]=slaptain_database_condition   ->  []
```

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

**Surface 2 adds two groups.** `slaptain.observation` holds the two freshness
anchors; `slaptain.evidence` holds the signal rules — no readable contextCSN at
all, partial readability, a spread above threshold, a cross-site link whose
provider count collapsed, a glue suffix, and the two suffix rules split by
`role`.

**The staleness rules are load-bearing, not decoration.** Surface 2 records at
exactly one point per controller; every partial pass — a restore, a failed mesh
resolution, `spec.suspend`, a terminating database, and critically a `Degraded`
cluster — deliberately records nothing and leaves the previous observation
standing. That is only safe because the anchors age and something watches them.
A `Degraded` cluster freezes its databases' observations, which is exactly when
the probe matters.

**Their thresholds differ, and the obvious number is wrong for both.** An anchor
observed every `C` seconds exceeds a threshold `T` for `C − T` of each cycle, so
with fuse `F` and evaluation interval `E` a rule fires only when `C > T + F + E`.

- The **database** anchor's cadence is a five-minute resync floor *plus the
  pass's own per-pod LDAP sweep*. At `T = 300` the entire margin comes out of
  the fuse, and the failure mode is a false critical on a site that is merely
  slow — when a spurious page costs most. The shipped threshold is 900.
- The **cluster** anchor cannot be fixed by any threshold: a `Running` cluster
  with replication disabled gets **no periodic requeue at all** (ADR-002
  amendment), so its anchor is written once and ages without bound. That rule is
  **gated** on the cluster having a CSN readout rather than given a bigger
  number.

Each rule's comment carries its own arithmetic. `monitoring/prometheus-rules-test.yaml`
pins them with `promtool` unit tests — run `make monitoring-rules-test`, which
needs `promtool` on the host (`pacman -S prometheus` on Arch). Those tests exist
for the one failure a live induction cannot distinguish from a healthy system:
an expression that silently matches nothing.

## What the dashboards show

Two dashboards, both in `monitoring/dashboards/` and provisioned together by
`make monitoring-dashboard-apply` (one ConfigMap, one key per file). The JSON in
the repo is the source of truth — a dashboard edited in the Grafana UI must be
exported back over its file, or the next run of the target discards the edit.

| Dashboard | Grain | Answers |
|---|---|---|
| **slaptain — mesh state** (`/d/slaptain-mesh/`) | one mesh at this site | Is replication healthy, and what does the operator think of every link and pod? Surfaces 1 and 2, plus a short Surface 3 overview. |
| **slaptain — slapd** (`/d/slaptain-slapd/`) | pod × database at this site | What is slapd itself doing — load, queues, capacity? Surface 3 only. |

They are split by **question, not by surface count**. The mesh dashboard is the
one to open first; it is scoped by mesh, which the exporter's series do not
carry. The slapd dashboard is scoped by what they do carry — cluster, fleet,
pod, database — so it works with Surface 3 alone, without kube-state-metrics or
the operator's metrics. Each links to the other in its header, carrying the data
source, namespace, cluster and time range; and every per-pod line in the mesh
dashboard's Surface 3 section is a data link that opens the slapd dashboard on
that pod (and, from the fill panel, that database).

### slaptain — mesh state

Two selectors sit above everything: **data source**, so one Grafana holding a
data source per site can switch between them, and the namespace → mesh →
cluster chain. There is no site selector; the data source is the site.

Surface 2 sits below the Surface 1 block: the two oldest observation ages, the
share of CSN evidence actually gathered, pods carrying a glue suffix, the
measured spread per database, read-of-expected per database, the per-pod suffix
entry as a state timeline, and discovered providers per cross-site link.

**The spread panel draws absence as a hole, not as zero.** It runs with nulls
unspanned and points always shown, so a measured `0` is a point on the zero line
and an unmeasured spread is a gap. A graph can reintroduce visually the exact
confusion the metric's type was changed to make unexpressible.

The centre panel is one row per **outbound link from this site**: this site's
consumer view of each peer. The reverse direction is on the peer's own
dashboard, because peer status is consumer-side and one-directional (ADR-008
amendment): a cut in one direction legitimately reads red at one site and green
at the other, and an idle-but-broken link reads `Synced` on both until writes
flow.

Surface 3 has its own row at the bottom — LMDB fill per pod and database
(`(pages_used − pages_free) / pages_max`, the rules' expression: `pages_used` is
a high-water mark that never shrinks after deletes, and the free list is what
LMDB reuses; accesslog databases included, drawn against the data-database
thresholds), current connections, queued operations, whether each exporter
target is up, and operations per second per pod. The operation rate has no rule,
but it is the panel that shows a replication storm — a stale-cookie refresh loop
reads as hundreds of binds and searches per second on the affected pods while
every Surface 1 and 2 signal stays flat. It stays empty until
`spec.monitoring.enabled` and `slapdPodMonitor.enabled` are both on, and a
database opened before `cn=Monitor` shows as a missing line, not a zero.

Each surface is a row of its own, so a section can be collapsed and the
Surface 3 row is findable by its title. **"All" clusters means the selected
mesh's clusters** — the option list, not every cluster in the namespace. That
matters for Surface 3 and the database panels, whose series carry no `mesh`
label and are scoped only through the cluster selector.

### slaptain — slapd

Selectors: data source → namespace → cluster → fleet (`role`: read-write,
read-only) → pod → database (`suffix`), each multi-select with "All" except the
first two, each chained on the ones before it — pick the read-only fleet and the
pod list holds only read-only pods, and the database list loses the
`cn=accesslog-*` journals they do not keep. The `suffix` selector applies to the
database panels only; everything else is per pod.

| Section | Panels | What it cannot tell you |
|---|---|---|
| *(headline)* | share of pods with a `cn=Monitor` reading; fullest data database and fullest accesslog (the rules' thresholds, 80/90 % and 70/85 %); busiest LMDB reader table; operations per second; slapd restarts in the time range | — |
| Exporter and process health | `cn=Monitor` reading per pod as a state timeline — *reading*, *slapd unreadable* (the exporter answered, slapd did not: `SlaptainSlapdMonitorUnreadable`), *exporter down* (`SlaptainSlapdExporterDown`); scrape duration against the exporter's 5 s `--timeout`; uptime; failed dials and binds | The failure counters' `result="fail"` series only exist after a first failure, so a healthy exporter shows *no data* there — the absence of failures since the sidecar started, not a reading of zero. |
| Databases (LMDB) | data-database fill and accesslog fill on separate panels, each with its own rule thresholds; a table of every pod × database now — fill, bytes in use, map size, entries, reader slots; entries over time; reader-slot use | Bytes are `pages × olmMDBPageSize`, which the `-ol26` image does not report, so those two columns are empty there. Equal entry counts across pods are a hint, not convergence — that is `contextCSN`, on the mesh dashboard. |
| Connections and operations | current connections; new connections per second; operations per second per pod; the operation mix by type and fleet; writes (Add, Modify, Delete, Modrdn) per pod — the rate an accesslog fills at | The panels draw completed operations only; `openldap_operations_initiated_total` is exported alongside but not drawn. |
| Threads and waiters *(collapsed)* | active and open threads against `cn=Max` (`olcThreads`); pending and backload; read and write waiters | A non-zero queue is depth, not saturation: live, `cn=Pending` read above zero with only 2–9 of 16 threads active, which is why no rule watches it. Saturation is a queue that grows while Active sits at Max. |
| Traffic sent *(collapsed)* | bytes per second; PDUs, entries and referrals per second | — |

**Restarts are counted as changes of `openldap_start_time_seconds`**, per pod
over the pod's merged series — a replaced pod gets a new `instance` label, so
per-series counting would miss exactly the restarts that replace the pod. The
earlier count, from `cn=Uptime` going backwards, read 1 on the lab for the one
pod that had been replaced in the window and 0 for the others; the start-time
count has not yet been checked against a live restart.

**There is a floor of slaptain's own traffic.** Every exporter scrape is a bind
(SASL EXTERNAL), a search and an unbind; every readiness probe a bind, a WhoAmI
extended operation and an unbind (ADR-030); the operator's per-pod passes and
the syncrepl consumers add more. A quiet directory therefore never reads zero
operations — on the three-site lab, about 8 per second across a read-write
fleet of three, most of them searches.

Rates use `$__rate_interval` with a 30 s minimum interval, matching the
PodMonitor's default scrape interval; with a longer `slapdPodMonitor.interval`
raise it in the panels, or a rate over too few samples comes back empty.

Left out on purpose: `cn=Max File Descriptors` and `cn=Max Pending` (configuration,
not load); `cn=Starting` threads (transient, and read 0 throughout on the lab); per-database dashboards (the
`suffix` selector already narrows every database panel, and a dashboard per
database would have to be re-provisioned for every database added).

## Exercising it

`monitoring/demo/cut-inbound.yaml` cuts inbound LDAP to one site's slapd pods
with a NetworkPolicy — reversible, no pod churn, probes unaffected (they are
exec probes on loopback, ADR-030).

```bash
make monitoring-demo-cut     CONTEXT=<site-3> CILIUM_CLUSTER=<site-3's cilium cluster-name>
# ~60-120s later: the other sites report site-3 Unreachable — on THEIR
# dashboards, since each site's Prometheus sees only itself.
# Check site-3's own dashboard too — see below.
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
and no `lastChecked` — the operator skipped the peer check when it could not
read its own databases, and then derived `connected` from the unset verdict. A
site that could not read itself blamed its healthy peers. Fixed 2026-10-06
(`docs/reconcile-loop-fixes.md`); `SlaptainPeerUnverified`, which keys on the
*absence* of `lastChecked`, now fires only for a cluster with no databases.

The blackout also takes the site's own Surface 3 dark: Prometheus scraping the
exporter sidecar is inbound to the slapd pod like any other connection, so
expect `up{job="slapd-exporter"} == 0` at that site for the duration. That is
read from the policy's shape; the verified run predates the PodMonitor.

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
