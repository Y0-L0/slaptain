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

Nothing converges *by this path*: the consumer full-refreshes, reconnects, sends
the identical cookie, and both passes run identically. There is no window and no
backoff, which is why the CPU cost is flat rather than bursty.

It is not unconditionally permanent, though — see *It can clear by itself*. A
full refresh carries the provider's whole `contextCSN`, which can restore the
sid the consumer was missing and dissolve the contradiction. Whether that
happens appears to depend on which link is storming.

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

### Two instances in one mesh, not one

The table above samples a single moment, and it is **later** than the CPU
reading. Read together with the pod log, the mesh held **two** broken providers:
`s1 slapd-2` *and* `s2 slapd-2`, mutually refusing each other and each also
refusing its own in-cluster consumers.

600 lines off `s1 slapd-2` show it as victim and culprit at once. As **provider**
it issued 30 stale refusals to four distinct consumers:

```
14x  cookie=rid=023,sid=002     s1 slapd-1          (in-cluster)
12x  cookie=rid=023,sid=001     s1 slapd-0          (in-cluster)
10x  cookie=rid=029,sid=023     sid 0x023 = 35 = site-3 pod 2
 9x  cookie=rid=029,sid=013     sid 0x013 = 19 = site-2 pod 2
```

As **consumer** it restarted refresh ten times, all on one rid:

```
10x  ldap_sync_search: rid=030 starting refresh
```

`slctl rid decode 030` resolves to segment 1, peer site index 1 — **site-2**. At
`replicasPerPeer: 1` ADR-031 maps local pod `i` to remote pod `(i+k) % N`, so the
provider refusing it is `s2 slapd-2`.

**The CPU distribution is the clearest evidence, and the idle pods carry it:**

| | pods 0/1 | pod 2 | RO |
|---|---|---|---|
| site-1 | **409 / 411** | 1698 | 412 |
| site-2 | **437 / 443** | 1696 | 4 |
| site-3 | **11 / 13** | 735 | 6 |

Sites 1 and 2 each host a broken provider, so their in-cluster consumers burn
~400m being refused. Site-3 hosts none, so its pods 0 and 1 sit at 11-13m, and
only its pod 2 is hot — the one whose cross-site link lands on a broken
provider elsewhere.

A single-source model cannot produce that. If `s1 slapd-2` were the only broken
pod, site-2's pods 0/1 had no reason to burn 440m while site-3's identically
placed pods idled at 11m: both sites are equidistant from site-1. **Count the
sites whose ordinals 0/1 are busy — that is the number of broken providers.**

So CPU *does* localise the fault, better than first thought — but at SITE
granularity, not pod granularity. Within a site the busiest pod is as likely to
be a refused consumer as the broken provider; only the `minCSN`/`contextCSN`
predicate separates those two.

### It can clear by itself

`s2 slapd-2` reads clean (6 = 6) in the table above, yet the log shows it
refusing `s1 slapd-2` minutes earlier, and a one-off read of that same pod
during the investigation returned **7 minCSN against 6 contextCSN**. The most
likely reading is that it was broken and **healed between samples**.

That matters twice over:

- the storm is **not** an unconditional steady state, so "it never recovers" is
  too strong a claim. A plausible mechanism is the full refresh itself: it
  delivers the provider's complete `contextCSN`, which would restore the
  consumer's missing sid and dissolve the contradiction;
- it makes a single clean sample **non-conclusive for absence**. One sample
  still proves a storm when the predicate fires, but a clean read only means
  clean *now*. Sample on the CPU signal, not on a schedule.

The inverted question is then the interesting one: not why these pods storm, but
why `s1 slapd-2` did **not** heal, when its in-cluster links to pods 0 and 1
were working throughout — only `rid=030`, the cross-site link, was restarting.

### Unexplained

`s2 slapd-readonly-0` sat at **4m** while site-1's RO burned 412m. If
`s2 slapd-2` was refusing its in-cluster consumers the way `s1 slapd-2`
demonstrably was, that RO should have been refused too. No evidence was
captured either way before the lab was wiped.

---

## The detector

Per accesslog DB, per pod, read as `cn=admin,cn=config` (ADR-020 denies the data
rootDN):

```bash
slctl ldapsearch -n <ns> --database <db> --pod <ord> --as config \
      -b cn=accesslog-<db> -s base '*' +
```

and assert **every `minCSN` sid appears in `contextCSN`**. It needs no cross-pod
comparison and no log scraping: the contradiction is local to one entry.

A firing sample is conclusive. A **clean** sample is not — the condition can
clear between reads (see above), so absence of the signature means "not right
now", not "never happened". Pair it with the CPU signal: a site whose pod
ordinals 0 and 1 are busy has a broken provider in it.

Implemented 2026-10-11 as the `slctl inspect` check **`accesslog-mincsn`**: it
fails naming the pod, the log and the missing sids, warns when a log could not
be read, and passes only when every log was read clean. `tests/e2e.sh` runs it
on every site after the suite (`check_no_refresh_storm_every_site`), so a run
that leaves a pod storming ends red. Its first live reading:

```
[FAIL] accesslog-mincsn  slapd-0 cn=accesslog-example-db2: minCSN sid 003,013 not in contextCSN;
                         slapd-0 cn=accesslog-example-db: minCSN sid 012 not in contextCSN — ITS#9398: …
```

on a pod that had answered about 4,000 operations a second for half an hour,
while `csn-convergence` on the same run read OK and a full three-site e2e had
just gone 104/0.

Still open: `dataloss_recovery_test.go` counts stale-cookie **log lines**
instead of asserting this predicate. That spec's gating claim is separately
stale — it reads as if the storm were `-ol26`-only, which the ITS#9398
investigation disproved.

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
