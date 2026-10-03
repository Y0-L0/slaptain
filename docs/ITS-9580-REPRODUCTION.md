# A delta-syncrepl provider stops serving every consumer after an unclean stop

Working notes toward an upstream report on OpenLDAP 2.7.1. Every claim below is
either demonstrated by a script in `docs/its9580/`, backed by a source line, or
explicitly marked as inferred-but-unobserved.

**Build:** `@(#) $OpenLDAP: slapd 2.7.1-0+slaptain1 (Sep 11 2026 10:00:00) $`,
built from a vendored Debian packaging fork of the 2.7.1 release (ADR-021).
Source line numbers are from that tree.

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

`SIGTERM` is safe, which is why routine restarts never show this and why it
looks random in the field: the trigger is node loss, an OOM kill, a lost
machine, or a container runtime's grace period expiring.

## Reproducing it

`docs/its9580/repro.sh` — two plain slapd servers, podman only, no Kubernetes,
no fault injection, no race. The container plumbing is one marked block; delete
it and the script runs on any host with OpenLDAP 2.7.1 installed.
`docs/its9580/controls.sh` runs the variants in the table below.
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

## Mechanism

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

**Proven by experiment**, on `2.7.1-0+slaptain1`, scripts in `docs/its9580/`:

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
