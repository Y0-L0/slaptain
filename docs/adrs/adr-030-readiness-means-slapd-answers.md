# ADR-030: Ready means slapd answers LDAP; nothing restarts a frozen pod

**Status:** Accepted
**Date:** 2026-09-28

## Context

The slapd container carried **no probes at all**, so "Ready" meant only that the
process existed. On 2026-09-16 a slapd froze for 874 s — accepting TCP
connections, answering nothing, logging nothing — and held `readyReplicas: 3`,
`phase=Running` and its place in the Service endpoints for the entire freeze.
Every wait built on `readyReplicas`, in the demo and throughout the e2e suite,
inherited that blindness.

ADR-013 (2026-05-14) asked the *hard* question in passing — should readiness
mean "slapd has converged data"? — and deferred it for reasons that still hold:
what "converged" means is genuinely unclear (base-scope on the suffix?
`contextCSN` within some delta? entry count?), and the implementation surface
was the same one that made its option C' expensive.

The easy question — *does slapd answer LDAP at all?* — has none of those
problems and was never asked separately. It rode along in the deferral of the
ambitious one. That is how a trivial guard goes missing for four months: not by
being rejected, but by being bundled with something hard.

## Options considered

### TCPSocket on port 1024 — rejected, disproven

The cheapest probe, and the incident that motivates this ADR is precisely a pod
that **accepted TCP throughout** while answering nothing. It would have reported
the frozen pod healthy for all 874 s.

### `spec.readinessGates` driven by the operator — rejected

Attractive on paper: no image change, and it reuses an observation the operator
already makes every 60 s (it logged 106 consecutive bind timeouts against the
frozen pod, at INFO, reaching nobody).

Rejected because an unset gate condition defaults to **False**. Under the
default `OrderedReady` policy a pod-0 that never reaches Ready means pod-1 is
never *created*, so a fresh cluster would depend on the operator being up,
reconciling, and able to reach a pod that is not yet in DNS, before it can have
a second pod at all. It moves a kubelet-local question into a control-plane
round trip and makes cluster formation depend on the answer. It would also be
exotic to a reader: someone auditing the pod spec looks for `readinessProbe`.

### A purpose-built Go probe binary in the slapd image — rejected as unnecessary

Fully code-controlled exit semantics ("any LDAP result means the server is up,
only timeout and connection failure are failures"). It costs a Go build stage in
the slapd image and a repo-root build context — ADR-013's option C' resurfacing,
and the second entry in the class `BUG-ANALYSIS-database-dirs-rolling-restart.md`
opened. Not worth it once `ldapwhoami` removes the problem the exit semantics
were for (below).

### Exec `ldapsearch` against the rootDSE — rejected

The obvious "is it answering" query, and what the e2e suite's `BeforeSuite`
already does. But `ldapsearch` exits with the LDAP result code, and there is no
shell in a distroless image to post-process it — so a user whose ACLs deny the
anonymous read would have a cluster whose pods are all NotReady. The probe must
not be able to fail because of what an identity may *read*.

## Decision

**1. An exec probe running `ldapwhoami -x -H ldap://localhost:1024`.**

WhoAmI is an extended operation that reads no entry, so no ACL can turn it into
a failure: "does the server answer" stays independent of "may this identity read
anything" — the same distinction ADR-025 makes when it treats uniform
invisibility as normal rather than as divergence. Anonymous, so the probe
carries no credential. Over the plain LDAP listener on loopback, so it exercises
the same path the ClusterIP Service routes to rather than a side channel.

The binary comes from the same vendored package build as slapd itself
(ADR-021), so client and server can never drift. The runtime image carries
exactly this one client and stays distroless, rootless and shell-free;
debugging still belongs to `slapd-toolkit` (`slctl debug`).

**2. A startup probe and a readiness probe; no liveness probe.**

| | period | timeout | failures | budget |
|---|---|---|---|---|
| `startupProbe` | 5 s | 5 s | 60 | 300 s |
| `readinessProbe` | 10 s | 5 s | 6 | 60 s |

The startup probe exists so the readiness threshold does not have to be
generous. It is deliberately over-provisioned: under `OrderedReady`, a pod-0
that never reaches Ready means pod-1 is never created, so a probe that is too
tight does not degrade a cluster, it prevents one — and the operator cannot
help, because there is no pod to converge.

The readiness threshold is the real design question. It must sit **well above**
a normal `cn=config` write, which pauses slapd for sub-second periods and which
the operator performs routinely (ACLs, schemas, syncrepl stanzas, per-pod
identity convergence); a threshold near those pauses would have the operator
evicting healthy pods as a side effect of configuring them. It must sit **far
below** the 874 s freeze this exists to catch. A minute is comfortably between,
chosen on that evidence rather than for symmetry.

**No liveness probe**, and that is a decision rather than an omission.
Restarting a pod mid-freeze invalidates every peer's syncrepl addresses under
ADR-016 — 130-268 s measured, per pod, per site — turning a recoverable stall
into a topology event across the mesh. **A frozen pod should leave the Service,
not the cluster.**

**3. Headless Services publish NotReady addresses** (landed separately, and
first, while it was still a no-op).

Everything the operator does per pod resolves
`<name>-<ordinal>.<name>-headless.<ns>.svc`, and a NotReady pod drops out of a
headless Service's DNS by default. Without this, the first unhealthy pod becomes
an *unreachable* pod: the operator cannot converge its node-local `cn=config`
(ADR-002), and ADR-027's identity gate then withholds syncrepl for the whole
site. The cluster's answer to one sick pod must not be to stop configuring all
of them. The client-facing ClusterIP Services deliberately do **not** set it —
stopping traffic at an unready pod is the entire point of the probe.

## Consequences

- **`readyReplicas` becomes meaningful.** Waits in the demo, the e2e suite and
  `tests/e2e.sh` that were passing spuriously will start genuinely waiting. That
  is the point, and it will surface latent timing assumptions.
- **Both slapd images must carry `ldapwhoami`.** The `-ol26` interop image
  (ADR-021, hot migration under ADR-011) gets the same binary, because the probe
  is in the pod template unconditionally and a 2.6 pod without it would simply
  never go Ready.
- **Version skew is a hard failure, not a degradation.** A new operator against
  an old `slapd` image pins pods that can never pass their probe. Both images
  default to the operator's own tag, so this only reaches someone who pinned
  `spec.images.slapd` independently — the same hazard, and the same mitigation,
  as the `LDAP_REPLICAS` removal recorded in `docs/reconcile-loop-fixes.md`:
  **ship the images together.** A `spec.probes.enabled` escape hatch is designed
  and deferred; it would be the right answer if this ever bites someone.
- **The startup budget is a guess above fixture size, and is labelled as one.**
  Measured on the three-site lab, 2026-09-28: every pod reaches Ready **3-5 s**
  after its container starts — one or two startup-probe periods — across all
  twelve pods of three sites, read-only replicas included. So the 300 s budget
  carries roughly 60-100x headroom *on fixture-sized data*, which is the only
  data anybody has: every fixture opens a few-dozen-entry LMDB, and `E2E_SCALE`
  seeds into a LIVE mesh rather than opening a big one cold. How that scales to
  a production LMDB is unmeasured; the big-DIT e2e lane that would produce the
  number (`docs/BACKLOG.md`) is still unbuilt, and this is now its fourth
  waiting consumer. 300 s is chosen to fail safe; raising it later is a
  pod-template change and therefore a roll.
- **The convergence question stays deferred**, exactly as ADR-013 left it.
  Ready means "answers LDAP", never "has the data". A pod that answers while
  still filling from its peers is Ready and will serve incomplete reads — which
  is what `DataPresent` (ADR-025) and `ReplicationConverged` (ADR-008) are for,
  and they remain the place that question is answered.
- **A frozen pod is still silent in the CR.** The probe removes it from Service
  endpoints; nothing yet surfaces *why*. Promoting the operator's own
  bind-timeout observation to a status condition remains open and is independent
  of this.

## Related

- ADR-013 — deferred the convergence probe and named `publishNotReadyAddresses`;
  this ADR splits the easy question out of that deferral and answers it
- ADR-016 — why there is no liveness probe: a replaced pod costs every peer an
  address rediscovery window
- ADR-002 — `cn=config` is node-local and converged per pod over headless DNS,
  which is why decision 3 is a prerequisite rather than a nicety
- ADR-027 — the identity gate that would turn one unreachable pod into a
  site-wide syncrepl withholding
- ADR-021 — the vendored package build that makes `ldapwhoami` version-matched
- ADR-025 / ADR-008 — where "has the data" is actually answered
