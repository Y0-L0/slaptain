# Upgrading slaptain

What an operator upgrade does to a **running** mesh, per release. Read the entry
for every version you cross, not only the one you land on. A fresh install never
needs this file.

---

## ADR-028 amendment: the serverID stride becomes hex (100 → 16)

**There is no upgrade path, and this is the one entry in this file that says
so.** A site's `serverIDBase` is derived as `serverIDIndex × 16` instead of
`× 100`, so every pod of every site but index 0 would change its `olcServerID`.

`olcServerID` is stamped into every CSN a pod has ever written, and `contextCSN`
is a vector keyed by it. Renumbering a running pod leaves its entire history
filed under the old sid — which then sits in every peer's vector forever without
advancing — and starts a fresh lineage at zero. Nothing detects this and nothing
repairs it.

And the operator **will** write it. `ensureServerIDs` converges `olcServerID`
per pod on every reconcile (ADR-017), comparing the derived value against the
live one and `Replace`-ing when they differ — which until now was a no-op for
any correctly-booted pod, because the derivation never moved. It moves here. So
on an existing mesh the new operator renumbers the pods, once, silently, and
there is no undo.

Do not upgrade a live multi-site mesh across this release:

- **a single-site cluster at index 0** is unaffected — base 0 either way;
- **a standalone cluster** with a hand-set `spec.replication.serverIDBase` is
  unaffected: that field is a bare integer, is not derived, and any value in
  `0..4094` remains legal (ADR-011's 101-104 / 201-204 layout included);
- **a multi-site mesh at indices ≥ 1** must be rebuilt, not upgraded. The change
  was taken deliberately while every deployment was a recreatable lab.

Also new, and worth checking before you apply a mesh: `spec.ridLayout.podsPerSite`
is now capped at **15**. A site's pod ordinal 15 would take the serverID that is
the next site's base. The chart refuses it at render time and the API server at
admission.

Why it was done at all: a serverID is read in exactly one place, the `#sid#`
field of a CSN, and slapd renders that field as three hex digits. Under the
decimal stride the numbers everyone reasoned in were not the numbers anyone
could read — site 1 pod 0 was 101, shown as `065`. At stride 16 it is `011`.
See ADR-028's amendment of 2026-10-05.

---

## ADR-031: RIDs become coordinates (`ridBase` → `ridSegment`)

`spec.replication.ridBase` is gone. Each `SlapdDatabase` now declares
`spec.replication.ridSegment`, and every syncrepl RID on every pod is renumbered
once.

**Convert by dividing by 100:** `0` stays `0`, `100` becomes `1`, `200`
becomes `2`.

That conversion preserves your existing numbering, and because the `ridBase`
convention started at `100`, a converted mesh usually begins at segment 1 and
leaves **segment 0 unused**. Segment 0 is perfectly legal — it owns rids 1..20
at the default layout; it is `rid` 0 that is reserved, not the segment. Losing
it costs one segment of 49 and nothing else, so reclaim it only if you want to,
and only by renumbering deliberately.

Everything below was measured on a three-site, 12-pod, 2-database lab mesh,
upgrading `v0.2.2` to the ADR-031 build.

### What the renumber costs

- **one `cn=config` write per pod per database** — 8 per site, counted in the
  operator logs, no repeats;
- **no pod restart** — all 12 pod UIDs, `restartCount` and `startTime`
  unchanged;
- **no full refresh** — 0 empty-cookie refreshes across 9 RW pods; every
  consumer resumed incrementally with its full CSN vector. Sync position is
  keyed on SID and a RID is only a local handle, so changing one loses nothing;
- **no interruption to writes** — 24 canaries written at rotating sites
  throughout the window, all present everywhere afterwards;
- **never a short stanza set** — polled every 12 s, every sample held either
  the old four RIDs or the new four, never fewer.

Three sites converged **~20 s** after the new operator started. The whole
procedure took **64 s**.

### The order — follow it exactly

```bash
# 1. Scale every operator to 0, at EVERY site, before touching anything else.
kubectl --context=<ctx> -n slaptain-system scale deploy/slaptain --replicas=0

# 2. Apply the new CRDs.
make operator-crd-apply CONTEXT=<ctx>

# 3. Convert your manifests (ridBase → ridSegment, ÷100) and apply them.

# 4. Upgrade the operator — this scales it back up.
make operator-helm-install CONTEXT=<ctx> GIT_TAG=<tag> SITE_NAME=<site>
```

**Upgrade `slctl` too.** A pre-ADR-031 `slctl inspect` reports two confident
failures against a *correctly* upgraded cluster —
`syncrepl-stanza-count: 4 stanzas (want 2)` and
`external-syncrepl: missing stanza for site-N` — because it reads a status
field this release removed. The stanzas are fine; the tool is stale. Rebuild it
with `make build-slctl` before you use it to judge the upgrade.

### Why step 1 is not optional

Step 4 applies the CRDs itself, so it is tempting to skip step 1. Do not. If
the **old** operator is running against the **new** CRD — which it is, from the
moment the CRDs land until the new pod is ready — two things happen, both
silent, and they compound:

- **Colliding RIDs.** The API server prunes `ridBase` **on read**; no write is
  needed. The old operator sees it absent, falls back to base `0` for *every*
  database, and writes the same RIDs for all of them. slapd accepts a duplicate
  RID without a word.
- **Severed cross-site replication.** `status.externalPeerStatuses[].discoveredAddresses`
  no longer exists either, so the old operator's cluster controller fails its
  status patch on every pass. Status freezes, the database controller reads no
  peer addresses from it, and it writes stanza sets **with no cross-site links
  at all** — one site dropped from four stanzas per database to two.

Measured on a live site: **complete within 24 s** of the CRD apply, with
controller errors starting at 3 s. Throughout, the `SlapdCluster` reported
`phase: Running` and every peer reported `Synced`, while a write at one site no
longer reached the severed one. **The only signal that catches it is
`slctl inspect`**, where `rid-uniqueness` and `external-syncrepl` both fail.

**If you hit it, finish the documented procedure.** Recovery is automatic and
complete: the stanzas are rewritten correctly and the writes missed during the
severance back-fill. No manual repair.

### If you forget to convert

Every write path refuses. Nothing is silently pruned on the way in:

```
# kubectl apply
strict decoding error: unknown field "spec.replication.ridBase"

# kubectl apply --server-side, and helm upgrade
.spec.replication.ridBase: field not declared in schema
```

The charts refuse a duplicate `ridSegment` before rendering anything. The
operator refuses one again at reconcile, and also refuses a segment outside the
layout's range (`RIDSegmentOutOfRange`) — withholding that database's stanzas
rather than writing a short set, which under `olcSyncRepl`'s atomic replace
would be a deletion.

### Reading the new numbers

With the default layout (`podsPerSite: 8`, `maxSites: 12`, so `M = 20`):

```
rid = 1 + segment·M + offset
offset <  8   →  a pod at your own site, at that ordinal
offset >= 8   →  your link to the site whose serverIDIndex is offset − 8
```

Segment 1 is therefore `21, 22, 23` for local pods and `29, 30, 31` for links
to sites 0, 1, 2. **These mean the same thing at every site in the mesh**,
which the old scheme's `151` — "the first peer in *my* list" — did not.

`slctl rid decode <rid>` does this for you, offline or against a cluster.

### Changing `ridLayout` later

`SlapdMesh.spec.ridLayout` sets `podsPerSite` and `maxSites`. Changing it
renumbers the mesh and costs exactly what this upgrade costs — measured at
**8 s** to renumber and ~1 s to revert, zero restarts, zero full refreshes.

Set it in one values file and apply it unchanged everywhere: it is mesh-scoped,
and two sites running different layouts would decode the same RID differently
with nothing to detect the disagreement.

### Not yet verified

- **Upgrading sites one at a time.** All three were upgraded in lockstep. A RID
  is pod-local and peers need no agreement about each other's numbers, so a
  mixed-version mesh should be safe — but that is reasoned, not measured.
  **Until it is, upgrade all sites together.**
- **A large directory.** The renumber has only been measured at fixture size.
  "No full refresh" is the claim most worth re-checking where a refresh would
  actually hurt.
