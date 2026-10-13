# monitoring/

Deployment inputs for slaptain's monitoring, built from off-the-shelf
components — kube-state-metrics and kube-prometheus-stack.

Surface 1 (CR state) involves no slaptain code at all and works against any
operator release whose status carries the fields. Surfaces 2 (the operator's
own judgments) and 3 (the slapd exporter sidecar) are scraped through the
operator chart's `ServiceMonitor` and `PodMonitor` — see `docs/MONITORING.md`.

The Surface 3 rules and dashboards follow the **exporter's** version line, not
the operator's: they expect the `openldap_*` families of slapd-exporter `0.x`
(`docs/VERSIONING.md` §7).

Monitoring is **site-local**: every site runs the same stack and scrapes only
itself, over ClusterIP. Nothing here names a site and nothing reaches across
one (ADR-033 amendment 2026-10-09).

| File | Applied where |
|---|---|
| `kube-prometheus-stack-values.yaml` | every site, unchanged, **first** — it brings the Prometheus Operator CRDs the later releases' monitors need |
| `kube-state-metrics-values.yaml` | every site, unchanged, after the stack (its `ServiceMonitor` needs the CRD) |
| `prometheus-rules.yaml` | every site, after the stack's CRDs exist |
| `prometheus-rules-test.yaml` | nowhere — `promtool` unit tests over those rules, run by `make monitoring-rules-test` |
| `dashboards/slaptain-mesh.json` | every site — the mesh overview (Surfaces 1 and 2, plus a Surface 3 summary); one key of the labelled `slaptain-dashboards` ConfigMap |
| `dashboards/slaptain-slapd.json` | every site — slapd per pod and database (Surface 3); the other key of that ConfigMap |
| `gateway.yaml` | any site, optional — Gateway API front door so Grafana and Prometheus have URLs instead of a port-forward |
| `demo/blackout.yaml` | reversible fault injection: a site dark to inbound LDAP, its own operator included. **Verified** |
| `demo/cut-inbound.yaml` | a genuinely one-directional cut via CiliumNetworkPolicy. **Unverified** — see its header |

Targets: `make monitoring-repo`, `monitoring-install`, `monitoring-ksm-install`,
`monitoring-stack-install`, `monitoring-rules-apply`, `monitoring-rules-test`,
`monitoring-dashboard-apply`, `monitoring-gateway-apply`, `monitoring-grafana`,
`monitoring-prometheus`, `monitoring-demo-blackout`, `monitoring-demo-cut`,
`monitoring-demo-restore`.

These files ship only in the repository, so monitoring is installed from a
checkout whose `monitoring/` matches the operator release you run — the rules
and dashboards reference that operator's and exporter's metric names and labels.
`docs/MONITORING.md` "Install" has the plain `helm`/`kubectl` sequence the make
targets wrap.

The user guide is `docs/MONITORING.md`; the decision behind the split is
ADR-033.
