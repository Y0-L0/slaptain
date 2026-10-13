# monitoring/

Deployment inputs for slaptain's monitoring, built from off-the-shelf
components — kube-state-metrics and kube-prometheus-stack. No slaptain code is
involved, so everything here works against any operator release whose CR status
carries the fields.

| File | Applied where |
|---|---|
| `kube-state-metrics-values.yaml` | every site, unchanged — it carries no per-site fact |
| `kube-prometheus-stack-values.yaml` | the one observing site; edit the three scrape targets for your lab |
| `prometheus-rules.yaml` | the observing site, after the stack's CRDs exist |
| `dashboards/slaptain-mesh.json` | the observing site, as a labelled ConfigMap |
| `demo/cut-inbound-site-3.yaml` | reversible fault injection, to see the signals move |

Targets: `make monitoring-repo`, `monitoring-ksm-install`,
`monitoring-stack-install`, `monitoring-rules-apply`,
`monitoring-dashboard-apply`, `monitoring-grafana`, `monitoring-prometheus`.

The user guide is `docs/MONITORING.md`; the decision behind the split is
ADR-033.
