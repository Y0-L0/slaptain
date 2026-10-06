# ITS#9398: the accesslog refresh storm, and how to see it

A delta-syncrepl consumer is told `sync cookie is stale`, full-refreshes,
reconnects with the same cookie, and is told the same thing again — forever. The
pods burn CPU at a few hundred percent and every health signal slaptain has
reads green.

This document records the **mechanism**, traced to slapd 2.7.1 source, and a
**local detector** found on a live reproduction on 2026-10-06. The detector is
the new part: the storm was previously believed to be invisible to every shipped
tool, diagnosable only by counting stale-cookie lines in a pod log.

---

## The signature

One entry, on one pod, contradicts itself. On the accesslog database:

```
# site-1 slapd-2, cn=accesslog-example-db
minCSN:     …172852…#001  …173048…#002  …173939…#003  …174857…#066  …210850…#011  …211026…#013
contextCSN: …211428…#001  …211428…#002  …211629…#003                              …211026…#013
```

`minCSN` names six serverIDs. `contextCSN` tracks four. **`minCSN` claims the log
can replay for sids `011` and `066`, which `contextCSN` says this pod does not
track at all.**

That is the whole trigger. The predicate is one-directional:

> **A `minCSN` sid that is absent from `contextCSN` on an accesslog DB ⟹
> permanent refresh storm.**

The reverse (`minCSN` ⊂ `contextCSN`, i.e. the log knows about fewer sids than
the DB tracks) is benign and common — it was observed on healthy, idle pods in
the same mesh.

---

## Why it is permanent — the two passes that disagree

`servers/slapd/overlays/syncprov.c`, OpenLDAP 2.7.1, inside
`syncprov_op_search`:

**Pass 1 — line 3320**, comment *"If there are SIDs we don't recognize in the
cookie, drop them"*:

```c
for (i=0; i<srs->sr_state.numcsns; ) {
    for (j=i; j<numcsns; j++)
        if ( srs->sr_state.sids[i] <= sids[j] ) break;
    if ( j == numcsns || srs->sr_state.sids[i] != sids[j] ) {
        /* drop this sid from the consumer's cookie */
```

`sids[]`/`numcsns` are the **provider's own** contextCSN (set at 3289–3294 from
`si->si_sids`). So every sid in the consumer's cookie that the provider does not
track is deleted from the consumer's state. In the capture above that removes
`011` and `066`.

**Pass 2 — line 3479**, the `ad_minCSN` branch:

```c
for ( i=0, j=0; i < a->a_numvals; i++ ) {        /* a = the log DB's minCSN */
    while ( j < numcsns && minsids[i] > sids[j] ) j++;
    if ( j < numcsns && minsids[i] == sids[j] &&
            ber_bvcmp( &a->a_vals[i], &srs->sr_state.ctxcsn[j] ) <= 0 )
        continue;
    /* else → LDAP_SYNC_REFRESH_REQUIRED, "sync cookie is stale" */
```

This iterates the log's `minCSN` sids and demands each still be present in the
cookie — the cookie pass 1 just pruned. `011` is in `minCSN`, was deleted by
pass 1, the `j`-walk cannot match it, and the consumer is refused:

```
syncprov_op_search: consumer not within recorded mincsn for DB's mincsn=20261005210850.530520Z#000000#011#000000
SEARCH RESULT tag=101 err=4096 … text=sync cookie is stale
```

**The second pass indicts a sid the first pass removed microseconds earlier.**

Nothing converges: the consumer full-refreshes, reconnects, sends the identical
cookie, and both passes run identically. There is no window and no backoff — it
is a steady state, which is why the CPU cost is flat rather than bursty.

---

## Why we reach that branch at all

Line 3457 is the **third** arm of a chain:

```c
if      ( !BER_BVISNULL( &si->si_logbase ) )  syncprov_play_accesslog(…)
else if ( si->si_logs )                       syncprov_play_sessionlog(…)
else if ( ad_minCSN != NULL && si->si_nopres && si->si_usehint )   /* ← here */
```

The storm is on the syncprov overlay of the **accesslog database itself** — the
DB a delta-syncrepl consumer searches with `base="cn=accesslog-<db>"`. That
database has no `logbase` of its own, and per **ADR-022** it deliberately
carries no sessionlog. So both earlier arms are skipped and adjudication falls
to the minCSN hint, with the prune/indict contradiction above.

ADR-022's reasoning is unchanged and still correct: a successful sessionlog
replay on an accesslog DB would displace the minCSN guard and under-replicate
silently. What was never recorded is the **consequence** — that the choice
routes all accesslog-DB syncrepl into this one branch. Whether a sessionlog on
the accesslog DB would avoid the storm, and at what cost to the guard ADR-022
protects, is an open question (see *Open questions*).

---

## Measuring it: the live reproduction of 2026-10-06

Three-site mesh, `slaptain-testing`, after a full-gate e2e run. All 18
`slctl inspect` checks passed throughout.

| pod | sid | `minCSN` sids | `contextCSN` sids | verdict |
|---|---|---|---|---|
| s1 slapd-2 | 3 | 001,002,003,011,013,**066** | 001,002,003,013 | **storm source** |
| s1 slapd-0 | 1 | 001,002,003,011,013,066 | = same set | ok |
| s1 slapd-1 | 2 | 001,002,003,011,013 | ⊃ (has 066 too) | ok (benign direction) |
| s2 slapd-0/1/2 | 17/18/19 | 001,002,003,011,013,066 | = same set | ok |
| s3 slapd-0/1/2 | 33/34/35 | 001,002,003,011,013,066 | = same set | ok |

Only `example-db` is affected; `example-db2`'s log is clean on all nine pods.

CPU at the same moment:

```
site-1  slapd-0 410m   slapd-1 408m   slapd-2 1681m
site-2  slapd-0 438m   slapd-1 439m   slapd-2 1685m
site-3  slapd-0   4m   slapd-1   7m   slapd-2  727m
```

**One broken pod, many hot pods.** This is the most useful thing in the capture:
`s2 slapd-2` burns 1685m with a perfectly consistent `minCSN`/`contextCSN` pair.
The CPU is the cost of *being refused* — every consumer that pulls `example-db`'s
log from `s1 slapd-2` full-refreshes in a loop, and pays for it at its own end.

So **CPU does not localise the fault, and the storming pod is usually not the
broken one.** Look for the self-contradictory entry, not for the busiest
process.

### Why ordinal 2 everywhere — hypothesis, not a finding

Every site's `slapd-2` is hot while ordinals 0 and 1 sit at a uniform baseline.
ADR-031 maps cross-site links as local pod `i` → remote pod `(i+k) % N`, so
cross-site peering is ordinal-correlated; the pods whose link happens to land on
`s1 slapd-2` would be hot together. This was not verified before the lab was
wiped, and no stanza-level evidence was captured for it.

---

## The detector

Per accesslog DB, per pod, read as `cn=admin,cn=config` (ADR-020 denies the data
rootDN):

```bash
slctl ldapsearch -n <ns> --database <db> --pod <ord> --as config \
      -b cn=accesslog-<db> -s base '*' +
```

and assert **every `minCSN` sid appears in `contextCSN`**. It needs no
cross-pod comparison, no log scraping and no timing window: the contradiction is
local to one entry, and it is a steady state, so a single sample is conclusive.

Not yet implemented. Two places it belongs:

1. an `slctl inspect` check — the natural home, since the storm is invisible to
   all 18 existing ones (they compare *data* DB contextCSNs across pods, which
   stayed identical here, and correctly reported convergence);
2. `dataloss_recovery_test.go`, replacing the current stale-cookie **log-line
   count** with this assertion. That spec's gating claim is separately stale —
   it reads as if the storm were `-ol26`-only, which the ITS#9398 investigation
   disproved.

---

## What this is *not*

`066` is decimal **102** — site-2 pod 1's serverID under the pre-2026-10-05
decimal stride, frozen at 17:54:21Z. It is renumber residue (ADR-028 amendment),
and it is a **passenger here, not the cause**: the indicted sid in the log is
`011` (17), which is live, and every healthy pod in the table carries `066` in
both lists while idling at single-digit CPU.

It does make recovery strictly worse. Sid 102 is dead, so once a pod's
`contextCSN` loses `066` while its `minCSN` retains it, that half of the
contradiction can never be repaired by replication — no entry will ever be
written under 102 again. A live sid can heal; an orphaned one cannot.

---

## Open questions

1. **Should the accesslog DB carry a sessionlog after all?** This is the third
   time a problem has turned out to be specific to its absence. A sessionlog
   would take arm 2 of the chain instead of the minCSN hint, which is the arm
   that contains the contradiction. Against it stands ADR-022's reason, which is
   real and was proven: a successful replay there displaces the minCSN guard and
   under-replicates *silently* — a worse failure than a loud storm. The
   question is whether a sessionlog sized so that it reliably *misses* (and thus
   falls through to a correct full refresh) buys anything over the current
   behaviour, or whether arm 2 and arm 3 fail the same way for the same reason.
   Needs reading `syncprov_play_sessionlog` with the same care applied to
   `syncprov_op_search` here.
2. **How did `s1 slapd-2` lose `011`/`066` from its accesslog `contextCSN`?**
   The run included the dataloss-recovery spec, which deliberately destroys a
   pod's PVCs. Unverified.
3. **Is there a repair short of rebuilding the pod?** `minCSN` is maintained by
   slapd, not by the operator, and ADR-026 R2 forbids the operator from
   correcting state it does not own on evidence like this.
