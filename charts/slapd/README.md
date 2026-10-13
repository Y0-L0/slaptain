# slapd

Deploys one site's `SlapdCluster`, plus optional `SlapdDatabase` and
`SlapdSchema` objects declared under `databases:` / `schemas:`. The slaptain
operator (`charts/operator`) must be installed first. For a multi-site mesh use
`charts/slapd-mesh` instead.

```bash
helm upgrade --install slapd oci://ghcr.io/chuck-chuck-chuck-net/charts/slapd \
  -n slaptain-testing --create-namespace -f my-values.yaml
```

`values.yaml` is the reference: every key is commented there. The chart renders
only the fields you set — a commented-out key is the operator's own default,
left to the operator so that a later release's changed default still reaches an
existing cluster.

## Monitoring (ADR-033 Surface 3)

The slapd exporter sidecar: one container next to every slapd pod, read-write
and read-only alike, serving slapd's `cn=Monitor` as `openldap_*` metrics on
`:9330/metrics`. Off unless you turn it on. The full story — what it exports,
how it is scraped, resources measured on a lab — is `docs/MONITORING.md`.

| Value | Default | Renders | Notes |
|---|---|---|---|
| `monitoring.enabled` | unset | `spec.monitoring.enabled` | `true` adds the sidecar. Changing it changes the pod template, so the StatefulSets roll their pods once. Unset and `false` render the same pods. |
| `monitoring.resources` | unset | `spec.monitoring.resources` | The sidecar's own requests/limits, not `spec.resources`. No default; `docs/MONITORING.md` has the measured starting point. |
| `images.exporter.repository` | `""` | `spec.images.exporter.repository` | Empty: `slapd-exporter` beside the operator's image (its trailing path segment swapped, like `slapd` and `slapd-init`). |
| `images.exporter.tag` | `""` | `spec.images.exporter.tag` | Empty: the exporter release compiled into the operator, not the operator's tag. A tag alone keeps the derived repository. |
| `images.exporter.pullPolicy` | `""` | `spec.images.exporter.pullPolicy` | Once any `images.exporter` field is set, the CRD stores `IfNotPresent` unless you set another. |

`monitoring` is passed through verbatim when set and omitted when empty;
`images.exporter` is rendered only when one of its fields is non-empty.

**The exporter is not pinned in lockstep with `slapd` and `init`.** It is its
own image with its own version line (`docs/VERSIONING.md`), and the operator
defaults it to the exporter release it was built against. An operator upgrade
therefore changes the sidecar only when that release moved; leaving
`images.exporter` empty is the normal setting. Pin it to run a different
exporter version — a newer release, a mirror, a local build. The operator passes
the sidecar no command and no arguments, so any `slapd-exporter` image works.

The sidecar is only half of it: nothing scrapes it until this site's operator
chart has `slapdPodMonitor.enabled=true` and a Prometheus Operator is installed
(`docs/MONITORING.md`, "Install").
