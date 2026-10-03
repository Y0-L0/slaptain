# A permanent `sync cookie is stale` loop on OpenLDAP 2.7.1

Working notes toward an upstream report. Written so that every claim is either
backed by a source line or explicitly marked as reasoned-but-unobserved — the
distinction upstream will press on first.

**Build:** `@(#) $OpenLDAP: slapd 2.7.1-0+slaptain1 (Sep 11 2026 10:00:00) $`,
built from a vendored Debian packaging fork of the 2.7.1 release (ADR-021).
Source line numbers below are from that tree.

## The invariant

> On a delta-syncrepl provider, if the **log DB's `minCSN`** names a serverID
> that the **log DB's `contextCSN`** does not contain, then *every* consumer of
> that provider receives `err=4096 sync cookie is stale` on *every* connection,
> regardless of how current it is — and retries with **no backoff**.

It does not heal. Observed flat at thousands of lines per minute for 22 minutes,
with no decay.

This is reached by, but not caused by, losing a replica's storage. The storage
loss is one way to make the two attributes diverge; the divergence is the bug.

## Mechanism

### Provider — `servers/slapd/overlays/syncprov.c`, `syncprov_op_search()`

1. **`:3320`** — *"If there are SIDs we don't recognize in the cookie, drop
   them"*. The consumer's cookie is trimmed to the provider's own `contextCSN`
   SID set.
2. **`:3342`** — `if (srs->sr_state.numcsns != numcsns)` → *"consumer cookie is
   missing a csn we track"* discards the cookie and forces a refresh. Passing
   this guard means the trimmed cookie and the provider's `contextCSN` are now
   index-aligned, which the next step relies on.
3. **`:3457`** — the branch is entered only when
   `ad_minCSN != NULL && si->si_nopres && si->si_usehint`, i.e. only on a log DB
   whose `syncprov` has `nopresent` and `reloadhint` set.
4. **`:3480`** — the check itself:

   ```c
   for ( i=0, j=0; i < a->a_numvals; i++ ) {
       while ( j < numcsns && minsids[i] > sids[j] ) j++;
       if ( j < numcsns && minsids[i] == sids[j] &&
               ber_bvcmp( &a->a_vals[i], &srs->sr_state.ctxcsn[j] ) <= 0 ) {
           /* minCSN for this serverID is contained, keep going */
           continue;
       }
       /* … LDAP_SYNC_REFRESH_REQUIRED, "sync cookie is stale" … */
   ```

   A SID present in `minCSN` but absent from `contextCSN` drives `j` to
   `numcsns`, so `j < numcsns` fails and control falls through to stale —
   **unconditionally, for every possible cookie**. The cookie was just
   normalised against `contextCSN`; it is then validated against `minCSN`; and
   nothing requires the two to agree.

Note the pre-existing `ITS#9580 FIXME` immediately above the lookup at
**`:3462`**: *"when we've figured out and split the sessionlog/deltalog
tracking, use the appropriate attribute"*. Upstream already considers this area
unfinished.

### Consumer — `servers/slapd/syncrepl.c`

5. `do_syncrep2()` turns `REFRESH_REQUIRED` in `SYNCLOG_LOGGING` into
   `SYNCLOG_FALLBACK` — *"delta-sync lost sync, switching to REFRESH"*.
6. The fallback full refresh against the **data** suffix succeeds and returns an
   **empty** cookie, so the consumer's stored cookie is never updated.
7. **`:2336`** — on `REFRESH_REQUIRED` outside `SYNCLOG_LOGGING`:
   `/* give up but schedule an immedite retry */  rc = SYNC_PAUSED;`
   (typo upstream's).
8. **`:2383`** — `SYNC_PAUSED` sets `rtask->interval.tv_sec = 0` and reschedules
   immediately. **`retry=` and `interval=` are never consulted on this path.**

→ same cookie, same trim, same stale. Measured at >100 stale results per second
per consumer, each preceded by a full refresh of the entire DIT.

### How the attributes diverge

`servers/slapd/overlays/accesslog.c:2069` carries its own `ITS#9580 FIXME`:

> *"This will only work if we log successful writes and nothing else, otherwise
> we're reverting some CSNs (at least our own) in the contextCSN to an older
> value. Right now we depend on syncprov's checkpoint to clean up after."*

A plain (non-delta) refresh delivers a `MOD REPLACE contextCSN` to the data DB,
and the accesslog overlay copies that modlist verbatim onto the log DB.
`REPLACE` is destructive, so a shorter vector from the refresh source **removes
SIDs** from the log DB's `contextCSN`. `minCSN` only ever grows. They diverge,
and nothing repairs it.

**Reasoned, not observed:** the truncating modify was never captured in the act
— both occurrences consumed the provider's log ring first. What was observed is
the before state, the after state, and a log DB that went from five SIDs to one
*and to an older value*, which is a replace rather than a merge.

## Reproducing it: two plain slapd servers, deterministically

`docs/its9580/repro.sh` does the whole thing and needs nothing but podman.
Captured output of a run is in `docs/its9580/sample-run.log`. The container
plumbing is one marked ten-line block at the top — delete it and the rest runs
unchanged on a VM with OpenLDAP 2.7.1 installed.

No Kubernetes, no operator, no three-site mesh, no storage loss, **and no
race**: the bad state is manufactured offline rather than waited for.

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

1. Write once at **A** and once at **B**, so both log DBs carry SIDs 001 and
   002 in `minCSN` and in `contextCSN`. Let them converge.
2. Stop **A**. `slapcat` its log DB, **delete the single `contextCSN` line whose
   SID is 002**, `slapadd` the result into a fresh directory, start **A**.

   Deleting a `contextCSN` value is preferred over adding a fictional `minCSN`
   one: it mirrors the destructive `MOD REPLACE` that `accesslog.c:2069`
   describes, and it leaves the missing SID owned by a **real running node**, so
   the cure in step 4 can be demonstrated with only two nodes.

   `slapadd` accepts `minCSN` verbatim despite its `NO-USER-MODIFICATION` flag,
   and slapd does not recompute or overwrite it at startup. That is what makes
   this deterministic.
3. **Observe.** B now receives `err=4096` on every connection and retries with
   no delay. A logs, for every one of them:

   ```
   syncprov_op_search: consumer not within recorded mincsn for DB's
     mincsn=<the lowest minCSN value whose SID is absent from contextCSN>
   SEARCH RESULT tag=101 err=4096 ... nentries=0 text=sync cookie is stale
   ```

   The script prints which CSN it expects **before** starting slapd, so the
   prediction is falsifiable rather than fitted afterwards.
4. **Cure, to confirm the mechanism.** One `ldapadd` at **B**. SID 002 re-enters
   A's log-DB `contextCSN` via `syncprov_new_ctxcsn()`, `minCSN` untouched, and
   the storm stops within one round trip.

### Measured, on a loopback pair

- **1449 `err=4096` per second**, with `retry="5 +"` configured and never
  consulted on this path. The three-site Kubernetes mesh measured >100/s; a
  loopback pair is simply faster at the same loop.
- **No healing.** Cumulative on the consumer at 30-second intervals: 101860 →
  144649 → 187767. Flat, no decay.
- **Cure: 1449/s → 0/s**, with all entries present on both nodes afterwards.

(Figures from an independent re-run of the committed script, not from the
session that wrote it.)

### A corroborating detail

A separate prototype injected two *fictional* `minCSN` SIDs (003 and 004) into a
`contextCSN` of {001, 002}. The provider named `#003#` — the lowest absent SID —
in all 26274 messages over 20 seconds. So the naming rule holds with more than
one absent SID, and an absent SID need not belong to any node that ever existed.

## What was proven by removal, and what was not

**Proven unnecessary** — removed, storm absent:

- three sites / cross-site consumers (single-site 3-replica: 0 occurrences);
- losing the data or config directories (wiping only the accesslog: 0);
- dormant SIDs, long uptime, an aged accesslog (one occurrence was on a mesh
  under 20 minutes old);
- **more than two nodes** — the committed reproduction uses two;
- **any replication history** — the divergence is injected into a database
  ninety seconds old;
- **storage loss of any kind**;
- **any race** — the setup is deterministic and worked first time.

**Proven sufficient** — the storm tracked this condition exactly, appearing with
it and stopping within 90 s of its removal, nothing else changed:

- `minCSN` ⊋ `contextCSN` by SID set, on the log DB.

**Observed in every reproduction but never removed, so not proven necessary:**

- `syncprov-nopresent` / `syncprov-reloadhint` on the log DB (the code says they
  gate the branch; not tested by turning them off);
- `olcMultiProvider`, the data DB's sessionlog, two databases per node.

**Still reasoned, not observed — and the report must say so.** The committed
reproduction injects the *end state*. It demonstrates the consequence of the
divergence, not its cause: the destructive `MOD REPLACE contextCSN` copied onto
the log DB has never been captured in the act, because both field occurrences
consumed the provider's log ring first. What was seen in the field is the before
state, the after state, and a log DB that went from five SIDs to one *and to an
older value* — a replace, not a merge.

**Not established:** whether a single-site cluster can reach the bad state at
all. It was shown not to reproduce *via the storage-loss trigger* in two
attempts; nothing in the code path requires more than one node.

## Two points for upstream

1. **The trim-then-validate asymmetry** at `syncprov.c:3320` versus `:3480` is
   the defect. The cookie is normalised against `contextCSN` and then checked
   against `minCSN`, and the two sets are permitted to differ. Skipping `minCSN`
   entries whose SID is absent from `sids[]` would turn a permanent outage into
   the graceful degradation the surrounding code plainly intends.
2. **Independently: the retry path has no backoff.** `syncrepl.c:2336` → `:2383`
   reschedules immediately. Any persistent provider-side `REFRESH_REQUIRED`
   therefore becomes a self-inflicted denial of service, with a full DIT refresh
   per iteration. That is worth fixing whatever happens to point 1.
