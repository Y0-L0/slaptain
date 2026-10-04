# A delta-syncrepl provider stops serving every consumer after an unclean stop

Working notes on **ITS#9398, "Stale accesslog cookie due to unclean shutdown"**
— open since 2020 — as it manifests on OpenLDAP 2.7.1. Every claim below is
either demonstrated by a script in `docs/its9398/`, backed by a source line, or
explicitly marked as inferred-but-unobserved.

**Build:** `@(#) $OpenLDAP: slapd 2.7.1-0+slaptain1 (Sep 11 2026 10:00:00) $`,
built from a vendored Debian packaging fork of the 2.7.1 release (ADR-021).
Source line numbers are from that tree.

## How to read this document

The next section states **the mechanism as we currently understand it**, start
to end, in the order that makes it make sense. Everything after
"Upstream status" is the **findings in discovery order** — including the
hypotheses that turned out to be wrong and the three reproduction attempts that
came back clean.

That ordering is deliberate. The wrong turns are what establish which
explanations have been *ruled out*, and more than one of them is a plausible
first guess that costs a day. If you only want to act, read the next section
and `docs/ITS-9398-HANDLING.md`. If you are about to re-investigate, read the
rest first.

## The mechanism, end to end

### The cast

Each logical database is **two** databases on every pod:

- the **data DB** (e.g. `dc=second,dc=example,dc=net`) — the directory itself;
- the **accesslog DB** (`cn=accesslog-<database>`) — a journal of recent
  *changes* to that data DB, one entry per write.

Both are **node-local**: every pod has its own `/accesslog` PVC and its own
copy. Pod A's journal is not pod B's journal.

Delta-syncrepl exists so a consumer that has been away does not re-read the
whole directory. It presents a cookie — "I last saw state X" — and the provider
answers with the changes since X, read out of the **journal**, not the data DB.
If it cannot answer from the journal, it tells the consumer to re-read
everything instead.

In a mesh every pod is both provider and consumer; "the broken provider" means
a pod in its provider role, serving others.

### The two numbers on the journal, and their different durability

The accesslog DB's root entry carries two multi-valued attributes, one value
per serverID:

- **`contextCSN`** — the newest change held, per serverID. High-water mark.
- **`minCSN`** — the *oldest* change still held, per serverID. Low-water mark,
  i.e. how far back the journal can still answer.

Together they bound the window the journal can serve. The bug lives in the fact
that the two are **not persisted the same way**:

| | written to disk |
|---|---|
| `contextCSN` | only at a syncprov checkpoint or a clean shutdown — and nobody configures a checkpoint on a log DB (not upstream's guide, not its own test fixtures, not Symas, not us — see `ITS-9398-HANDLING.md` §2) |
| `minCSN` | eagerly, during normal operation |

So on a running pod the on-disk `contextCSN` is routinely hours stale while the
served value is current. Normally invisible: a clean shutdown writes it on the
way out.

### What purging is, and what it triggers

The journal would grow without bound, so `logpurge` sweeps it — delete entries
older than *maxage*, every *interval*. slaptain defaults to `7+00:00 1+00:00`
(keep 7 days, sweep daily). It runs independently on **every pod**, against
**that pod's own** journal.

Deleting the oldest entries moves the journal's floor forward, so a sweep must
update `minCSN` — and writes it to disk immediately. It does **not** write
`contextCSN`. Measured:

```
before sweep   contextCSN …225946#001    minCSN …225946#001
after  sweep   contextCSN …225946#001    minCSN …094304#001
                        ↑ unchanged               ↑ advanced
```

Each sweep therefore ratchets the durable low-water mark forward while the
durable high-water mark sits still. **That is the sense in which purging is the
trigger** — not a cause of corruption, but the thing that drives on-disk
`minCSN` past on-disk `contextCSN`.

The resulting inversion is why nothing detects this: **in memory `contextCSN`
leads `minCSN`; on disk `minCSN` leads `contextCSN`.** Every health check reads
over LDAP, which is the in-memory view, which looks perfect.

### The unclean stop, and which serverID matters

A clean shutdown writes `contextCSN` and all of the above evaporates. A
`kill -9`, an OOM kill, a node crash or a non-graceful node shutdown does not,
so slapd restarts with `minCSN` ahead of `contextCSN`. Then:

- a serverID **present** in `contextCSN` but lagging — **survivable**
  (measured: one refresh, then quiet);
- a serverID in `minCSN` and **absent from `contextCSN` altogether** —
  **fatal**.

Fatal because the provider decides "can I answer from the journal?" by looking
`minCSN` up against its own vector. A serverID it holds *no high-water mark for
at all* is not a stale window but an incoherent one, so it gives up and answers
`e-syncRefreshRequired` (4096) — "your cookie is stale, re-read everything".

The serverIDs that end up missing are the ones **new to that journal since the
last `contextCSN` persist**: a remote site writing to a database for the first
time, a rebuilt pod, a freshly created database.

### Why it is a storm rather than a hiccup

The consumer obeys and re-reads the whole directory. That **succeeds** — no
data is lost; this is not a correctness bug. Then it tries delta-sync again,
is refused again, re-reads again. There is **no backoff** on this path.
Measured on a live mesh: 6968 stale answers per minute, five consumers in
permanent full refresh, the provider pinned at 2380m CPU serving all of them.

### Diagnosis and cure, and why the cure is addressed to a serverID

**Diagnosis** is a set difference on the journal's root entry: the serverIDs in
`minCSN` minus the serverIDs in `contextCSN`. Non-empty means broken. Nothing
in the ecosystem computes this; `docs/its9398/its9398.sh diagnose` does.

**Cure** is one trivial write — we set `description` on the data DB's suffix
entry; the content is irrelevant — issued **on the pod that owns the missing
serverID**. A write is stamped with the CSN of the server where it
*originates*, so a write at serverID 201 produces a change `…#0c9#…`. That
change replicates to the broken pod, lands in its data DB and its journal, and
its syncprov advances `contextCSN` to include serverID 201 — which it
previously had no entry for at all. `minCSN ⊆ contextCSN` holds again, the
lookup succeeds, delta-sync resumes.

Only a write *from that serverID* works. A write from any other server carries
that other server's stamp and does nothing for the gap. This is the part that
is easy to get wrong: on a mesh the missing serverID usually belongs to a
**different site** than the broken pod.

### Two consequences

The exposure is **not continuous**. It is tied to purge sweeps, which at the
slaptain default happen once a day. That moves the "how do we avoid this"
question away from "checkpoint the log DB" — which no deployment anywhere does
— and toward the purge interval, which every deployment already configures.

And the whole condition is invisible to every monitoring tool in existence,
including upstream's own, because all of them read the in-memory view of the
*data* suffix. See `ITS-9398-HANDLING.md` §1.

## Upstream status: already filed, by a maintainer, open six years

**ITS#9398** — *"Stale accesslog cookie due to unclean shutdown"*, Quanah
Gibson-Mount, 2020-11-19, assigned to Ondřej Kuzník, **UNCONFIRMED**, target
milestone 2.7.2, depends on ITS#9225. Verified by direct fetch. Its description
is this entire failure mode in one paragraph:

> "If slapd terminates uncleanly, a checkpoint will be lost on the accesslog db.
> Depending on the syncprov overlay checkpoint settings (usually no
> checkpointing is enabled on the accesslog db) this can cause the system to
> refuse engage in replication at startup."

The contemplated fix (Kuzník, 2023-11-07) is to have syncprov defer the backend
commit and send the contextCSN update from a result callback — which depends on
**ITS#9225** (back-mdb PREPARE/2-phase commit), and that is **VERIFIED
SUSPENDED**: *"I suggest we postpone this."* (hyc, 2026-04-07). So the intended
remedy is blocked behind an explicitly postponed enhancement.

**We previously cited ITS#9580 throughout. That was wrong.** ITS#9580 is
*"Refresh vs. accesslog in delta-MPR"* (Kuzník, 2021-06-14, IN_PROGRESS) and its
description mentions neither `minCSN`, nor "sync cookie is stale", nor unclean
shutdown. The `ITS#9580 FIXME` comments we were reading sit inside code that
*that* ticket added; one of them happens to remark that it depends on syncprov's
checkpoint. We mistook the remark for the ticket.

### The requirement is real and undocumented

Nothing upstream tells you to put a `syncprov-checkpoint` on the **log** DB:

- `slapo-accesslog(5)` never mentions checkpoints, CSNs, syncprov, or shutdown
  at all — zero matches across the whole page.
- `slapo-syncprov(5)` documents `syncprov-checkpoint`, `syncprov-nopresent`
  ("should only be set TRUE for a syncprov instance on top of a log database")
  and `syncprov-reloadhint` without ever connecting them.
- The Admin Guide's delta-syncrepl worked example gives the log DB
  `nopresent`/`reloadhint` only, and puts `syncprov-checkpoint 1000 60` on the
  **primary** DB twenty lines later. Identical in the 2.4, 2.5, 2.6 and 2.7
  guides.
- **Upstream's own regression fixtures agree**: of every config in the 2.7.1
  tree that puts a syncprov on a log DB, not one carries a checkpoint there.

The only adjacent advice is generic and framed as a performance concern, not a
correctness one — `overlays.sdf`: a checkpoint is recommended "to **minimize
recovery time** after an unclean shutdown".

The invariant itself appears nowhere in any document. The closest is a
maintainer's mailing-list remark (Kuzník, 2023-02-28): *"accesslog's contextCSN
should always be in sync with its main DB"*, where he also calls this class
*"the most common reason to see e-syncRefreshRequired - 4096"*.

There is **no documented recovery procedure**. The "one write from the absent
serverID" cure appears once, in a 2006 user post, never endorsed by a
maintainer; our measurement is better evidence than that citation.

## Summary

> On a delta-syncrepl provider, the accesslog DB's **`minCSN` is written to disk
> eagerly** while its **`contextCSN` is only written by a syncprov checkpoint**.
> With no `syncprov-checkpoint` configured on the log DB — which no
> delta-syncrepl setup guide tells you to configure — the on-disk invariant
> `minCSN ⊆ contextCSN` is violated **from the first write onward, permanently,
> on a perfectly healthy node.**
>
> It is invisible while the process lives, because searches read the in-memory
> copy. **Any unclean termination makes it fatal:** after the restart, that
> provider answers `err=4096 sync cookie is stale` to every consumer on every
> connection, forever, and the consumers retry with no backoff.

Reproduced in four steps with two servers and no fault injection:
**write, write, `SIGKILL`, start.**

**The `SIGKILL` is a stand-in, not the claim.** No administrator action is
required anywhere in this bug. Two independent facts make that so:

- the broken on-disk state exists on **every** delta-syncrepl provider from its
  first write — it is not caused by the stop, only revealed by it;
- **slapd crashes on its own during ordinary replication.** A field node exited
  139 on `back-mdb/id2entry.c:828: mdb_opinfo_get: Assertion '!rc' failed` while
  ingesting replicated entries — a failed `mdb_txn_renew` that the code asserts
  on rather than handles (`docs/its9398/field-crash/`). **Correction, 2026-10-04:**
  that pod was a *read-only replica*, which carries no accesslog at all, so that
  particular crash **cannot** have produced this bug. It is a separate upstream
  defect and it shows slapd does abort itself in this code path — but it is not
  evidence of the unclean stop *this* bug needs, and was wrongly presented as
  such. What stopped the field providers uncleanly remains unidentified.

So the chain requires nobody to do anything wrong: a node joins by replication,
is born with the invariant already broken on disk, and slapd then crashes on its
own during ordinary replication.

`SIGTERM` is safe, which is why routine restarts never show this and why it
looks random in the field: the trigger is node loss, an OOM kill, a lost
machine, or a container runtime's grace period expiring.

## Reproducing it

`docs/its9398/repro.sh` — two plain slapd servers, podman only, no Kubernetes,
no fault injection, no race. The container plumbing is one marked block; delete
it and the script runs on any host with OpenLDAP 2.7.1 installed.
`docs/its9398/controls.sh` runs the variants in the table below.
Captured output: `sample-run.log` and the three `control-*.log` files.

```
A, B — OpenLDAP 2.7.1, serverIDs 1 and 2, loopback.

Each node:
  data DB  dc=example,dc=org
    overlay syncprov
    overlay accesslog   logdb cn=accesslog   logops writes   logsuccess TRUE
  log DB   cn=accesslog
    overlay syncprov    syncprov-nopresent TRUE   syncprov-reloadhint TRUE
  multiprovider TRUE
  syncrepl to the other with  syncdata=accesslog  logbase="cn=accesslog"
```

1. Start both. Write one entry at **A** and one at **B**. Let them converge.
2. `kill -9` node **A**.
3. Start **A**.
4. Keep **B** idle.

Measured: **1389 `err=4096` per second**, flat (cumulative 78652 → 120500 over
30 s), with `retry="5 +"` configured and never consulted. One write at **B**
takes it to **0/s** immediately.

Step 4 matters. The serverID missing from A's on-disk `contextCSN` is restored
the moment that server writes anything, so a cluster with one busy writer
self-heals and an idle peer keeps the storm running indefinitely. That is the
"dormant serverID" earlier field reports described.

### The disk-versus-served observation

Taken on a node that is **running normally and has never been restarted**:

```
--- SERVED (ldapsearch against the live slapd — syncprov's in-memory si_ctxcsn)
minCSN:     …#001…   minCSN:     …#002…
contextCSN: …#001…   contextCSN: …#002…

--- ON DISK (slapcat of a byte copy of the live LMDB directory)
minCSN:     …#001…   minCSN:     …#002…
                                        ← contextCSN absent entirely
```

The log DB's `contextCSN` is not merely short — it has never been written at
all. `syncprov_db_open` loads that nothing at startup while `accesslog_db_root`
loads the full `minCSN`.

**Confirmed in production shape**, not only in the two-node fixture. Read off a
healthy, running, three-site Kubernetes node with `slapcat` against a copy of
its live LMDB file — the pod was serving normally and nothing had gone wrong:

```
cn=accesslog-example-db          (log DB; no syncprov-checkpoint configured)
  minCSN:     …#001…  …#003…  …#002…  …#065…  …#066…     ← five serverIDs
  contextCSN: …#001…  …#003…                              ← two
```

`minCSN` names three serverIDs that `contextCSN` does not. That node was one
unclean stop away from refusing every consumer, while every health check it has
reported green. Here the divergence is partial rather than total, so the
exposure is not limited to a log DB that has never been checkpointed — a
partially-written `contextCSN` is equally fatal.

## What it looks like on a cluster: one sick node, five loud ones

Measured on a live three-site mesh, 2026-10-04, while the condition had been
running for hours. Topology: 3 sites × (3 read-write + 1 read-only), pod-routed
cross-site replication, `replicasPerPeer: 1`, two databases.

The headline: **the broken node is not the one that looks broken.** It is the
quietest in its own logs about anything being wrong, and the loudest on CPU.

| Site | Pod | CPU | `cookie is stale` /60 s | `lost sync` /60 s | Role |
|---|---|---:|---:|---:|---|
| s1 | **slapd-0** | **2380m** | **6968** | 0 | **the broken provider** |
| s1 | slapd-1 | 688m | 0 | 4413 | consumer, local |
| s1 | slapd-2 | 690m | 0 | 3442 | consumer, local |
| s1 | readonly-0 | 612m | 0 | 3938 | consumer, local RO |
| s2 | slapd-2 | 581m | 0 | 7608 | consumer, cross-site |
| s3 | slapd-2 | 575m | 0 | 5077 | consumer, cross-site |
| s2 | slapd-0, slapd-1, readonly-0 | 5-6m | 0 | 0 | unaffected |
| s3 | slapd-0, slapd-1, readonly-0 | 6m | 0 | 0 | unaffected |

**The two log signatures are disjoint, and that is the diagnostic.**
`sync cookie is stale` is emitted **only** by the broken provider — it is what
it answers. `delta-sync lost sync` is emitted **only** by its consumers. No pod
produces both. So one grep identifies the culprit uniquely, across any number
of sites:

```bash
# the one pod that answers stale cookies is the broken one
kubectl logs -n <ns> <pod> -c slapd --since=60s | grep -c 'cookie is stale'
```

A pod at 147k log lines per minute where every other pod sits near zero is the
same signal without parsing anything.

### Why exactly three CPU levels

The quantisation is not noise; it is the replication graph.

- **~6-11m — not a consumer of the broken provider.** Six of twelve pods. They
  are entirely healthy and their own replication is fine. Nothing about the
  incident is visible here.
- **~575-690m — one permanent refresh loop.** Each of these pods has exactly
  one syncrepl stanza pointing at the broken provider, retries without backoff,
  is told the cookie is stale, falls back to a **full DIT refresh**, succeeds,
  and starts over. The cost is one refresh stream, so they all land on the same
  number regardless of site. Note they are *not* broken: their data is correct
  and their other stanzas work normally.
- **~2380m — the provider serving all of them at once.** 2380 ÷ 5 ≈ 476m per
  stream, against ~600m spent on the consumer side of the same stream. The
  provider's load is the *sum* over its consumers, which is why it is the
  outlier and why the number grows with mesh size rather than staying fixed.

### The blast radius is a formula, not a mystery

Exactly five consumers, and which five is fully determined by the topology. For
a broken provider at a site with `N` read-write pods and `R` read-only
replicas, in a mesh of `M` sites at `replicasPerPeer: 1`:

```
affected consumers = (N-1)      local read-write peers
                   +  R         local read-only replicas (each consumes from ALL local masters)
                   + (M-1)      exactly one pod per remote site
```

Here (3−1) + 1 + (3−1) = **5**, matching the measurement exactly.

The cross-site term is the non-obvious one. At `replicasPerPeer: 1` with equal
replica counts the cross-site wiring is a **bijection** — verified on this mesh,
where site-2's pods map to site-1's by a rotation:

| from | to |
|---|---|
| s2 `slapd-0` | s1 `slapd-1` |
| s2 `slapd-1` | s1 `slapd-2` |
| s2 `slapd-2` | s1 `slapd-0` ← the broken one |

so each remote site contributes exactly **one** victim, never zero and never
all three. Site-3 maps identically (`s3 slapd-2 → s1 slapd-0`). Raising
`replicasPerPeer` multiplies this term, trading link redundancy for a wider
blast radius when a provider goes bad.

**Consequence for operators:** a sick pod at one site makes pods at *every other
site* burn CPU, which is exactly the shape that gets misdiagnosed as a network
or cross-site problem. The victims are spread across the mesh; the cause is one
pod's local disk state.

### The diagnosis is a set difference, and the cure is addressed to a serverID

Read the broken pod's **accesslog** DB (`cn=accesslog-<database>`, rootDN
`cn=admin,cn=config` per ADR-020 — the data rootDN is denied):

```
minCSN:     …#001…  …#002…  …#066…
contextCSN: …#001…                      ← 002 and 066 missing
```

`minCSN \ contextCSN` **is** the fault, exactly. The serverIDs it yields are
the ones that must write before the storm stops. Two properties of that set
were measured the hard way on the live mesh, and both break the obvious fix:

1. **It is per database, not per pod.** This cluster had two. `example-db` was
   missing one serverID; `example-db2` was missing two and was the one actually
   driving the storm. Healing the first changed nothing — the stale answers
   kept coming from the other database's log DB. A per-pod check that stops at
   the first database reports a cured cluster that is still down.

2. **The serverID often lives at another site.** `example-db` was missing
   `#066` = SID **102** = site-2's `slapd-1`. Writes from all three *local*
   pods (SIDs 1, 2, 3) did nothing, because those were the SIDs already
   present. Only the write issued at site-2 cleared it. In CSN text the
   serverID is hex (`#066#` = 102) while `olcServerID` is decimal — convert, or
   chase the wrong pod.

The full cure for this incident was three writes: SID 102 into `example-db`,
then SIDs 2 and 102 into `example-db2`. On the last one the stale rate went
from ~2000 per 30 s to **0**, and CPU fell across all three sites.

So "issue one modification to the producer" — the vendor's documented first
rung — is right in shape and under-specified in practice: *which* database and
*from which serverID* are both load-bearing, and neither is visible from the
pod that is broken.

### Tooling

`docs/its9398/its9398.sh` does the three operations against a slaptain mesh.
Pass `--context` once per site, because the healing write usually has to be
issued at a different site from the broken pod:

```bash
./its9398.sh diagnose --context s1 --context s2 --context s3   # exit 2 if exposed
./its9398.sh heal     --context s1 --context s2 --context s3
./its9398.sh break    --context s1 --target 0 --yes            # SIGKILL arm
./its9398.sh break    --context s1 --target 0 --graceful       # control arm
```

`diagnose` and `heal` are validated against a live broken mesh (the incident
above: detected on both databases, healed by three writes, stale rate to zero).

**`break` has three arms, and the middle one is a trap worth naming.**

| `--kill` | what actually reaches slapd | reproduces? |
|---|---|---|
| `graceful` | `kubectl delete pod` → SIGTERM, 30 s grace | no (control) |
| `hard` | `kubectl delete pod --grace-period=0 --force` | **no — measured** |
| `brutal` | real `SIGKILL` to slapd's PID via a hostPID helper on the node | **no — measured** |

`--grace-period=0 --force` **is not a SIGKILL to the process.** It removes the
*API object* immediately without waiting for the kubelet to confirm; the
kubelet still terminates the container its own way, so slapd generally gets
SIGTERM and shuts down **cleanly** — writing `contextCSN` on the way out, which
is precisely what prevents the bug. The `hard` arm and the `graceful` arm are
therefore close to the same experiment, which is why the first attempt came
back clean: zero stale answers across all twelve pods for 60 s, and `diagnose`
clean on all 18 pod×database pairs afterwards. (This matches hard-won
experience in sibling projects: to truly crash a process under Kubernetes you
have to `kill -9` it on the node.)

Ruled out as the explanation for that negative: the cluster is **not** quietly
checkpointing its log DBs — see the measurement below.

The `brutal` arm does the real thing: a `hostPID`, privileged helper pod
scheduled onto the **same node**, which finds the host PID whose namespaced PID
is 1 inside the target container and `kill -9`s it. The kubelet then restarts
the container **in place** — same pod, same PVCs, `restartCount+1` — which is
exactly the shape of an OOM kill or a crash, and keeps the volumes whose state
the bug lives in.

It needs the namespace to permit `hostPID` + `privileged`. Clusters commonly
enforce `baseline` with **no namespace label to show for it**, so this is only
discoverable by being refused; `--allow-privileged` labels the namespace for
the duration and removes the label again.

Ruled out as the explanation: the cluster is **not** quietly checkpointing its
log DBs. Measured on the same pod —

```
olcDatabase={2}mdb (data)       olcSpCheckpoint: 500 15   olcSpSessionlog: 5000
olcDatabase={3}mdb (accesslog)  olcSpNoPresent: TRUE      ← no checkpoint
```

— which is exactly the ecosystem-standard shape §2 describes, so the exposure
should be present.

### The real SIGKILL also came back clean — which narrows the bug

Run 2026-10-04 with the `brutal` arm: `slapd-0` went to
`RESTARTS 1` at an unchanged pod age, i.e. the container was killed and
restarted **in place**, PVCs intact. Afterwards:

```
cn=accesslog-example-db    minCSN {001,002,003,065,066} ⊆ contextCSN {001,002,003,065,066}
cn=accesslog-example-db2   minCSN {001,002,066}         ⊆ contextCSN {001,002,066}
stale answers in 60 s: 0
```

The `contextCSN` values came back **byte-identical to the pre-kill ones**. So
on this cluster an uncatchable `SIGKILL` of a converged pod did not cost the
log DB its vector, and **an unclean stop is necessary but not sufficient**.

That is a real qualification of the ticket as filed: ITS#9398 is titled "Stale
accesslog cookie due to unclean shutdown", and unclean shutdown alone does not
do it. The missing ingredient is most plausibly Chu's startup behaviour — *"the
syncprov overlay always reads the largest entryCSN out of the database at
startup"* — meaning the vector is **reconstructed** from surviving log entries
when it is not on disk. If so the fatal case is a serverID whose entries are no
longer *in* the log to reconstruct from: aged out by `logpurge`, or a log that
was replaced wholesale. ITS#10173's repair speaks in exactly those terms —
minCSN values "that don't correspond to entries present in the log".

Note this does not fit the podman reproduction, where two freshly-created
servers storm after one `kill -9` with both SIDs' entries present. Either the
reconstruction is weaker than per-serverID there, or something else differs
between a two-node pair minutes old and a mesh hours old. **Unresolved**, and
the honest state of the question.

### The purge experiment: a third negative, and probably a cure in disguise

The hypothesis above predicted that emptying the log would make the kill
fatal, because there would be nothing left to reconstruct from. It was tested
directly on 2026-10-04: `spec.replication.accesslogPurge: "00:05 00:01"` on
`example-db2`, converged to `olcAccessLogPurge` on the pods, log allowed to
drain.

The purge behaved exactly as intended and gave one clean new fact:

```
before:  20 entries  (14 × SID 001, 3 × 002, 3 × 066)
after:    0 entries
         minCSN == contextCSN, all three SIDs, identical timestamps
```

So **`accesslog_purge` rewrites `minCSN` to match `contextCSN`** when it drains
the log — the `accesslog_purge: updating minCSN with N values` line of
ITS#10173, seen from the outside.

Then the SIGKILL, into a log with **zero** entries — reconstruction is not
merely weak, it is impossible — and the result was again **clean**:
`contextCSN` came back byte-identical, `minCSN == contextCSN`, zero stale
answers on any pod.

That is decisive about one thing: **the log DB's `contextCSN` is being
persisted to disk on 2.7.1 by something other than `syncprov-checkpoint`,**
which this cluster does not set on log DBs. With an empty log there is nothing
to rebuild from, so the vector can only have been read off the disk.

The likeliest candidate is the purge itself. `accesslog_purge` modifies the log
DB's root entry — that is how `minCSN` gets written eagerly — and if the same
write persists `contextCSN`, then **purging is a checkpoint in disguise**. On
that reading the experiment did not arm the trap; it sprang it harmlessly, and
our 5-minute purge was applying mitigation A once a minute.

If true, two things follow, and both are testable:

- the real-world exposure window is the gap **between purges**, which at the
  slaptain default (`7+00:00 1+00:00`) is up to a day; and
- shortening `logpurge`'s *interval* would be a mitigation that needs no
  unprecedented configuration — unlike option A, every deployment already sets
  `logpurge`.

### The slapcat test: purge is the TRIGGER, not the cure

Run 2026-10-04. The prediction above was **wrong, and informatively so**.

Baseline, with the log DB's served `contextCSN` for SID 001 at `…094304`
(a write made moments earlier) — read off the disk with `slapcat`:

```
ON DISK   contextCSN: 20261003225946…#001      ← stale, missing the write
          minCSN:     20261003225946…#001
```

So on-disk `contextCSN` is genuinely not written per-write. The documented
mechanism holds. Then `accesslogPurge` was set to `00:01 00:01` and a sweep
allowed to run. Reading the disk again:

```
ON DISK   contextCSN: 20261003225946…#001      ← UNCHANGED
          minCSN:     20261004094304…#001      ← ADVANCED to the new write
```

**The purge persists `minCSN` and does not persist `contextCSN`.** It is
therefore not a checkpoint in disguise — it is the mechanism that drives
`minCSN` *past* the on-disk `contextCSN`. Purging is the **trigger**.

Note the inversion this creates: in memory `contextCSN` is ahead of `minCSN`,
on disk `minCSN` is ahead of `contextCSN`. The served view is not merely
optimistic, it is the exact opposite of the durable one.

### A lagging serverID is survivable; a missing one is the bug

A SIGKILL in that state was then taken, and the broken relation **did** survive
the restart — the pod came up serving

```
contextCSN: …225946#001      minCSN: …094304#001     (minCSN ahead)
```

— and yet produced **no storm**: one stale answer from each of its two
providers and two `lost sync` lines on the restarted pod, i.e. a single
one-shot refresh, then quiet.

That sharpens the precondition, and it is the distinction the diagnosis in this
document already encodes: what is fatal is a serverID present in `minCSN` and
**absent from `contextCSN` altogether**, not one that is merely older. All
three serverIDs were present here; one was behind. `syncprov_findcsn(mincsn)`
survives a lagging value and fails an unknown one.

The original incident matches exactly: `example-db2` had `contextCSN` with
**only** `#001` while `minCSN` named `#001 #002 #066` — two serverIDs missing
outright, not lagging.

So the full recipe, and the reason a plain restart never showed it:

1. a serverID must be **new to this log DB** since the last `contextCSN`
   persist (first write from a site, a pod rejoining, a fresh database);
2. a purge sweep must then persist `minCSN` including that serverID, while
   `contextCSN` stays behind on disk;
3. an unclean stop must prevent `contextCSN` being written;
4. on restart, `minCSN` names a serverID `contextCSN` has never heard of.

All four are ordinary operation. None is fault injection.

### The fatal state, summoned on demand and repaired — red/green

The recipe was then executed deliberately, 2026-10-04, and it works. Site-3's
`SID 201` had never written to `example-db2`, so its log DB's `contextCSN` had
never heard of it:

1. write to `example-db2` from `site-3/slapd-0` (SID 201);
2. `accesslogPurge: "00:01 00:01"`, one sweep — `minCSN` persisted **with** 201;
3. `kill -9` slapd on `site-1/slapd-0`;
4. restart.

`diagnose` on the restarted pod, across all 18 pod×database pairs:

```
site-1  slapd-0  example-db2    MISSING serverIDs: 201        ← the fatal shape
(every other pair)              ok
```

`heal` then repaired it with the single write the mechanism predicts, issued at
the site that owns the serverID, and re-verified:

```
example-db2 <- write from SID 201 (site-3/slapd-0)
… all 18 pairs ok
HEALED.
```

So the state is **reproducible on demand and repairable on demand**, which is
what was needed to test any mitigation. Both directions are now exercised
against a fault that was created rather than waited for.

**What is still not on demand is the storm itself.** This arming produced the
broken precondition without the 4096 flood inside a 60 s window — the flood
needs consumers to actually re-present a cookie the provider will reject, and
on an idle mesh with sessions already established that does not happen
immediately. The incident earlier the same day had both, at 6968 stale answers
per minute. So: precondition reproducible, consequence still opportunistic.

### Experiments (2026-10-04): the harm is a contextCSN REGRESSION of the pod's OWN serverID

Two experiments were run to size the operator design — how long after a broken
restart does the storm start, and can a heal beat it. Both were overtaken by
what they found.

**Setup.** Armed as in the recipe above (write from `SID 202`, a serverID the
journal had never seen; 1-minute purge; `kill -9`), then driven with continuous
writes during observation, on the theory that the storm needs consumers to keep
requesting deltas.

**Result 1 — the state was armed, harder than intended.** After restart,
`cn=accesslog-example-db2` on the killed pod was missing **two** serverIDs from
`contextCSN` that `minCSN` named (`0c9`=201, `0ca`=202).

**Result 2 — no storm. A different error entirely.** Sampled every 20 s for
**420 s** (21 samples, T+0 to T+404) with live write traffic throughout:
**every sample 0 stale-cookie answers**, 0 `err=4096`, CPU flat at 4-6m. What
appeared instead, at a steady ~30/min:

```
err=53  text=consumer state is newer than provider!
```

**Result 3 — and it is not about the missing serverIDs at all.** The log says
exactly what it objects to:

```
SRCH base="cn=accesslog-example-db2" …
syncprov_op_search: consumer 1 state 20261004094304.906679Z#000000#001#000000
                 is newer than provider 1 state 20261003225946.303873Z#000000#001#000000
```

The disagreement is on **`#001` — the killed pod's own serverID** — and
`…225946` is precisely the stale *on-disk* `contextCSN` measured before the
kill. On restart the pod loaded a `contextCSN` for **its own** serverID that was
older than what it had already served, and its consumers remember the newer
value. Healing `201`/`202` left `err=53` running unchanged, which is the proof
that the missing-serverID set difference was not what was hurting.

**Result 4 — the cure is a purely LOCAL write.** One write issued **on the
broken pod itself**, from its own serverID, moved `contextCSN[001]` from
`…225946` to `…153801`, past everything its consumers hold.

### What this changes

There are **two distinct failure modes** from the same stale-on-disk
`contextCSN`, and only one of them was previously described here:

| | observed as | what disagrees | cure must come from |
|---|---|---|---|
| **missing serverID** | `err=4096 sync cookie is stale` — the storm | a SID in `minCSN`, absent from `contextCSN` | the pod owning that SID, usually **another site** |
| **own-SID regression** | `err=53 consumer state is newer than provider` | the pod's **own** `contextCSN` went backwards across a restart | the pod **itself** — local |

The second has a much better shape for an operator:

- **Detectable without reading disks.** Only pod P ever generates CSNs stamped
  with P's serverID, so P's `contextCSN[P]` must never be below any peer's view
  of it. A peer holding a newer value for P's own SID is pathological by
  construction — and is exactly what the operator's existing cross-pod CSN
  monitoring already fetches.
- **Curable locally.** No cross-site write, no operator fan-out, no ADR-026 R2
  question. The pod that is wrong is the pod that must write, and its own
  site's operator owns it.
- **Cheap while broken.** ~30 errors/min and no measurable CPU, versus the
  storm's 6968/min and 2380m. So the budget for detecting and healing it is
  minutes, not seconds.

**Still unexplained:** what additionally distinguishes the morning's `err=4096`
storm. The armed state reproduced the broken CSN relation and the `err=53` face
of it, with live write traffic, and never produced a single stale-cookie answer.
So the set difference is necessary for the storm but not sufficient, and the
missing ingredient is still unidentified. The negative is a flat line across
the whole window, not a short look: 21 consecutive samples at zero while the
broken relation was verified present and writes were flowing.

### Status of the Kubernetes reproduction, plainly

Three attempts, increasingly targeted, **all clean**:

| attempt | what was done | result |
|---|---|---|
| 1 | `--grace-period=0 --force` | clean — never a SIGKILL to slapd |
| 2 | real `kill -9` on a converged pod | clean — `contextCSN` survived intact |
| 3 | real `kill -9` with the log purged empty | clean — and probably self-cured |

Yet the condition is real and was captured on this very cluster hours earlier:
`minCSN ⊄ contextCSN` on two databases, 6968 stale answers per minute, five
consumers in permanent refresh. So it *arises* — just not from an unclean stop
on 2.7.1.

What has never been tested is the path that actually produced it: that cluster
had been through an e2e run, which wipes a pod's PVCs and lets it rejoin
(ADR-012 case 2, `dataloss_recovery_test.go`) and scales the cluster to zero
for a restore. **That is the next trigger to try**, and it is the one with
circumstantial evidence behind it.

Leading hypothesis, now secondary: an unclean stop may be **necessary but not
sufficient**. The violation needs a serverID that is in `minCSN` and *not
recoverable* when `contextCSN` is rebuilt at startup — which is consistent with
Chu's "the syncprov overlay always reads the largest entryCSN out of the
database at startup". If startup can reconstruct the vector from surviving log
entries, then a plain kill of a converged node is harmless, and the fatal case
is one where the evidence is gone: entries aged out by `logpurge`, or a pod
that came back with **no** accesslog at all. Note the real incident arose from
an e2e run that replaces pods and wipes volumes (ADR-012 case 2), not from a
plain restart — and ITS#10173 speaks of minCSN values "that don't correspond
to entries present in the log".

**Next experiment**, in order of cost: (1) `slapcat` the log DB on a freshly-killed pod via the `slctl debug` recipe
in CLAUDE.md, to establish whether on-disk `contextCSN` was reconstructed or
persisted; (2) set an aggressive `accesslogPurge` (e.g. minutes), let a serverID's entries
age out of the log, and only then SIGKILL — this is the hypothesis above, stated
as an experiment; (3) the dataloss path, which is what produced the incident in the first
place.

Until one of those lands, the honest statement is: **the bug is reproducible in
four operations on two plain servers (podman), and has been observed repeatedly
on Kubernetes, but we cannot yet summon it on Kubernetes on demand.**

## Mechanism, as it was worked out

From here on the document is in **discovery order**, wrong turns included. The
settled version is "The mechanism, end to end" at the top; this is how it was
established, and which explanations are therefore ruled out.

### Why the disk state is wrong on a healthy node

- `minCSN` is written eagerly: `accesslog.c:2141` does a real `LDAP_MOD_ADD`
  through `be_modify` for every new serverID it journals.
- `contextCSN` on that DB lives only in `si_ctxcsn` and reaches disk solely
  through `syncprov_checkpoint` (`syncprov.c:1612`), whose **only two callers**
  are the configured checkpoint (`:2638`) and `syncprov_db_close` (`:4204`,
  guarded by `if (si->si_numops)`).

So with no checkpoint configured, the only thing that ever persists
`contextCSN` is a clean shutdown. That is precisely why `SIGTERM` is safe and
`SIGKILL` is not.

Upstream's own `accesslog.c:2069` `ITS#9580 FIXME` says *"we depend on
syncprov's checkpoint to clean up after"* — a dependency on configuration that
is not on by default and is not mentioned in the delta-syncrepl instructions.

### Why the bad state is fatal rather than merely degraded

`servers/slapd/overlays/syncprov.c`, `syncprov_op_search()`:

1. **`:3320`** — *"If there are SIDs we don't recognize in the cookie, drop
   them"*: the consumer's cookie is trimmed to the provider's `contextCSN` set.
2. **`:3342`** — a count mismatch after that trim discards the cookie outright,
   so past this point the trimmed cookie and `contextCSN` are index-aligned.
3. **`:3457`** — the branch is entered only when
   `ad_minCSN != NULL && si->si_nopres && si->si_usehint`, i.e. on a log DB.
4. **`:3480`** — the trimmed cookie is then validated against **`minCSN`**:

   ```c
   for ( i=0, j=0; i < a->a_numvals; i++ ) {
       while ( j < numcsns && minsids[i] > sids[j] ) j++;
       if ( j < numcsns && minsids[i] == sids[j] &&
               ber_bvcmp( &a->a_vals[i], &srs->sr_state.ctxcsn[j] ) <= 0 )
           continue;
       /* … LDAP_SYNC_REFRESH_REQUIRED, "sync cookie is stale" … */
   ```

   A serverID in `minCSN` but absent from `contextCSN` drives `j` to `numcsns`,
   so the guard fails and control falls through to stale — **unconditionally,
   for every possible cookie**. The cookie is normalised against one attribute
   and judged against another, and nothing requires the two to agree.

A pre-existing `ITS#9580 FIXME` sits four lines above that lookup at **`:3462`**.

### Why the consumer makes it a denial of service

5. `do_syncrep2()` turns `REFRESH_REQUIRED` into `SYNCLOG_FALLBACK`; the
   fallback full refresh against the data suffix succeeds and returns an
   **empty** cookie, so the stored cookie never changes.
6. **`syncrepl.c:2336`** — `/* give up but schedule an immedite retry */`
   (upstream's typo) sets `SYNC_PAUSED`.
7. **`:2383`** — `SYNC_PAUSED` sets `rtask->interval.tv_sec = 0` and reschedules
   immediately. **`retry=` and `interval=` are never consulted on this path.**

→ ~1400 stale results per second per consumer, each preceded by a full refresh
of the entire DIT.

### What the impact is — and what it is not

**Data still converges.** Measured on a live three-site mesh while a provider
was storming at ~5700 stale results per minute: an entry written at the broken
provider reached **all nine** read-write pods across all three sites, as did a
control entry written at a healthy node. We predicted one-way divergence and
were wrong; the prediction is recorded here because the negative matters.

The reason is step 5 above — the consumer's fallback full refresh against the
*data* suffix **succeeds**. In the same 45-second window the broken provider
served 2211 full searches of `dc=example,dc=org` alongside 4355 stale results.
The data gets through; it is the delta-syncrepl optimisation that is gone.

So the harm is **resource exhaustion, not data loss**: every consumer refreshes
the entire DIT, hundreds of times a second, forever. On a fixture DIT that
merely pins a CPU (observed: 2237m on the provider, 550-660m on each of five
consumers). On a production DIT it is an outage. And because nothing breaks
functionally, every replication health check keeps passing — which is why this
survived so long unnoticed.

### Why the monitoring does not catch it

That last sentence is checkable, and it holds for upstream's own tool.

Every published check we examined — LTB's `check_ldap_syncrepl_status`,
`check_syncrepl_extended`, the Prometheus exporters — compares *data-suffix*
`contextCSN` across servers. In this failure those values agree, because the
data really is converging. `slapd-monitor(5)` exposes no replication state at
all, so `cn=Monitor` is not a route in either.

The Project's own [`syncmonitor`](https://git.openldap.org/openldap/syncmonitor)
is the interesting case, because it is the one tool that necessarily *sees* the
symptom: it runs as a real syncrepl consumer. Read at `f2debe3`, it treats the
signal as unremarkable by number — `environment.py`:

```python
except ldap0.LDAPError as e:
    result = e.args[0]['result']
    if result not in (ldap0.TIMELIMIT_EXCEEDED, ldap0.SIZELIMIT_EXCEEDED, 0x1000):
        logger.warning("Search to %r finished with %r, "
                       "waiting before reconnection", self.uri, e)
...
if result == 0x1000:
    self.cookie = SyncreplCookie()
    self.cookie_updated(None)
await self.set_up()
```

`0x1000` is 4096, `e-syncRefreshRequired`. It is listed among the outcomes that
do not even emit a warning, alongside time- and size-limit exceeded; the cookie
is then discarded and the session re-established. `syncrepl_observer.py` does
the same on the in-flight path (*"Refresh required (cookie was rejected), forget
it before we propagate"*). Pointed at a provider in this state, syncmonitor
reports **In-sync** indefinitely and logs nothing; its only `BROKEN` state comes
from a 30-second no-progress watchdog that an idle directory trips anyway.

This is not a criticism of the tool — discarding a rejected cookie and
reconnecting is the correct client behaviour, and a consumer cannot distinguish
one legitimate refresh from a provider that will reject every cookie forever.
It is offered as an explanation of the silence: the condition is only visible as
a *rate*, from the provider side, and nothing in the ecosystem measures that.

## Controls

| Scenario | disk `contextCSN` at stop | after restart |
|---|---|---|
| converge, `SIGKILL`, start | *(absent)* | **1526/s storm** |
| converge, `SIGTERM`, start | written by `db_close` | **clean** |
| `syncprov-checkpoint 1 1` on log DB, `SIGKILL` | `001 002` | **clean** |
| `syncprov-checkpoint 100 5` on log DB, `SIGKILL` | `001` only | **1473/s storm** |

The last row is the important one: **a batched checkpoint is a lottery, not a
guard.** The exposure is the interval between a new serverID entering `minCSN`
(immediately) and the next checkpoint writing `contextCSN`. Since new serverIDs
appear rarely and in bursts — at topology changes — a time- or operation-batched
checkpoint will lose that race sooner or later. Only `1 1` closes it, at the
cost of a `contextCSN` write per operation on a write-heavy database.

## Proven versus inferred

**Proven by experiment**, on `2.7.1-0+slaptain1`, scripts in `docs/its9398/`:

- a log DB's on-disk `contextCSN` is absent on a healthy running node with no
  checkpoint configured;
- `SIGKILL` + restart alone produces the permanent storm — no node loss, no
  concurrent write load, no race, no fault injection;
- `SIGTERM` + restart does not;
- `syncprov-checkpoint 1 1` on the log DB prevents it; `100 5` does not;
- one write from the absent serverID cures it instantly, with `minCSN`
  untouched;
- the consumer's retry ignores `retry=`.

**Inferred from source, not observed in any run:** that `accesslog.c:2069`'s
destructive copy can *additionally* truncate a `contextCSN` that a checkpoint
had previously written. That branch never executed in any run here
(`trying to reset contextCSN`: 0 occurrences). It is a second route to the same
end state and the report does not depend on it.

**Observed in the field but not reproduced here:** a log DB whose `contextCSN`
went from five serverIDs to one *and to an older value*. An unclean stop
explains the same end state without needing that path, so the mechanism behind
that particular observation remains uncaptured.

## A note on framing

It is tempting to describe this as a failure to bootstrap a replaced node by
replication, because that is the scenario it was found in. That is not quite
right, and the accurate version is stronger.

The bootstrap **succeeds**: the replacement node recovers the complete DIT, and
cross-site replication comes back. Nothing about the wipe creates the
divergence. What happens is that the node — like every other node with an
accesslog — comes up with `minCSN` on disk and `contextCSN` only in memory, and
therefore *looks* perfectly healthy while being one unclean stop from refusing
every consumer forever.

The scope is accordingly wider than any recovery scenario: it is every
delta-syncrepl provider, from its first write, in a default configuration.

## For upstream

1. **The asymmetry is the bug.** `minCSN` is durable, `contextCSN` is not, and
   nothing enforces the invariant between them across a restart. Either both
   should be written in the same transaction, or `syncprov_op_search` should
   skip `minCSN` entries whose serverID is absent from `sids[]` — the latter
   fixes the stale-disk case and the `accesslog.c:2069` case together.
2. **Independently, the retry path has no backoff.** `syncrepl.c:2336` → `:2383`
   reschedules immediately, so any persistent provider-side `REFRESH_REQUIRED`
   becomes a self-inflicted denial of service. Worth fixing whatever happens to
   point 1.
3. **A default-configuration hazard.** The documented delta-syncrepl recipe does
   not include a checkpoint on the log DB, so every deployment following it is
   one `kill -9` away from a provider that never serves a consumer again.

## Open questions, as of 2026-10-04

Recorded so they are not rediscovered from scratch. None is answered.

**1. What uncleanly stops the providers in the field?** This is the one real
gap. The reproduction uses `SIGKILL` as a stand-in, but nobody has identified
what does it on the lab. Two candidates, neither confirmed:

- *Grace-period expiry.* The slapd pods set **no**
  `terminationGracePeriodSeconds` (so the Kubernetes default of 30 s applies)
  and have **no** `preStop` hook. If slapd needs longer than 30 s to close its
  databases, every pod deletion is an unclean stop — and a full e2e suite
  deletes pods many times over. Nobody has measured slapd's actual shutdown
  duration on a loaded pod. **This is the cheapest next measurement and it may
  be a slaptain defect rather than upstream's.**
- *A crash.* One was captured — `back-mdb/id2entry.c:828: mdb_opinfo_get:
  Assertion '!rc' failed`, exit 139, during syncrepl ingest
  (`docs/its9398/field-crash/`). But it was on a **read-only replica**, which
  carries no accesslog, so that instance cannot have caused this. It stands as
  a separate upstream defect and as proof that slapd does abort itself on this
  path.

**2. Why was the storm absent and then present on a pod that never restarted?**
On 2026-10-03 the storming pod showed `restartCount=0`, started 23:00:01, yet a
stale-cookie census at ~00:58 found **zero** across all twelve pods, and by
~01:30 it was at ~5700/minute. The model says a process loads the bad
`contextCSN` at startup and is broken from that moment, which fits
`restartCount=0` — the "restart" was the pod's creation — but not the quiet
window. Either the storm began well after startup for a reason we have not
identified, or no consumer happened to request those log DBs during the census.
Unresolved, and it matters: "broken from startup" and "goes bad later" imply
different triggers.

**3. Frequency.** Reconstructed from reports rather than measured consistently,
so treat as indicative: the e2e's stale-cookie assertion fired in roughly **4 of
8** executions across the session — but **4 of 5** on full three-site suites and
**0 of 3** on single-site or label-filtered runs. That split is consistent with
the mechanism (full suites churn pods far more, so they hit more unclean stops)
and is itself the best circumstantial evidence for question 1.

**4. Does `logpurge` reopen the window on a steady-state server?** The purge
path advances `minCSN` on disk with a real `be_modify` on its own timer
(`accesslog.c:656-684`, `:760-787`), with no corresponding `contextCSN` write.
If so, the exposure is not confined to the bootstrap window but returns on every
purge cycle. Read from source; not measured.
