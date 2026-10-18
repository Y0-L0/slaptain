# ADR-037: The accesslog purge runs where the writes are not

**Status:** Proposed
**Date:** 2026-10-17

## Context

`accesslog_purge()` collects every expired entry with one uncapped search and
deletes them in an unpaced loop, each delete its own transaction. Those deletes
take the accesslog environment's single LMDB writer, and every client write
appends its own log entry to that same environment. Because the writer is
exclusive, a provider's purge blocks that provider's **writes**. Reads keep
flowing at ~75 %.

Every measured figure below comes from one platform. The quantity that governs
the whole analysis — how fast a provider can delete log entries — is a property
of its storage, so a deployment that wants to size this feature measures its own
rather than taking ours.

**And every one of them was measured on OpenLDAP 2.6.10, not on 2.7.1.** All
the runs below ran on the `-ol26` image pair (`slapd 2.6.10+dfsg-1`, one image
across the whole table). That is not a detail: the two versions' purge loops
differ exactly where this ADR looks. With pacing unconfigured, 2.7.1 calls
`ldap_pvt_thread_yield()` after **every** delete and 2.6.10 never does — the
`else` branch of the pacing block runs on every iteration when the factor is 0.
Both call `ldap_pvt_thread_pool_pausewait()` in the same position, but that
yields to a thread-pool pause wanting quiescence, not to client work, so it
does nothing for the contention this ADR is about.

→ The unpaced baseline we measured is **not** 2.7.1's unpaced baseline, and a
per-delete `sched_yield` is precisely the kind of thing that could move how
promptly a waiting client gets the writer. Everything below describes the
behaviour of a version we no longer default to. It is the best evidence we
have, the mechanism and the shape of the problem carry across, and the numbers
should not be quoted as 2.7.1's.

Measured on a 2+2 cluster at 100k ops/s (0.3 % writes), one hour per arm,
`cn=config` set asymmetrically with reconciliation suspended:

| | purge on the write target | purge on the peer only |
|---|---|---|
| write stall per cycle | 30 s | **0 s** |
| iterations dropped per cycle | 969,786 | **0** |
| peak p99 | 3,384 ms | **1.3 ms** |
| peak replication lag | 40.8 s | 34.8 s |

Purging only the peer makes the event invisible to clients: the write rate
holds 295–305/s through every cycle where the symmetric case collapses to
~5/s. A symmetric control matches the write-target arm.

Today both providers purge simultaneously — they restart together and the
runqueue task is scheduled from startup — so every deployment runs the worst
case, the write path stalling at the same instant the peer's replay does.

The purge is what prompted this, but it is not the only reason we want a
rotating write target. A provider that never takes a local write never advances
its own sid, and we want the mode for that on its own merits (ADR-036) — which
is why the rotation is worth having at write rates where the purge stall is
not measurable.

### The duty cycle, which is what makes this structural

In steady state a cycle deletes what aged out, so deletes per second averaged
over a cycle equal the write rate. The fraction of wall clock a provider spends
purging is therefore

    duty = write rate / delete rate

and the purge *interval* does not appear. Halving it halves each batch and
doubles the frequency, leaving the total stalled time unchanged.

This is a first-order model, counting the delete work alone. Option B shows it
breaking down at short intervals, by a mechanism we have not explained.

→ Scheduling alone cannot remove the stall from a provider that is taking
writes. The only lever that works is moving the purge off the write path. The
same ratio says where the feature is pointless: as the write rate falls far
below the delete rate the duty cycle vanishes, and with it the problem.

The field observation that prompted this — a 0.1 % write mix against a
high read rate, purging every 5 minutes with 15 minutes retained, slapd
unresponsive for >100 s per cycle — is of the order the duty cycle predicts for
its entries per cycle. What the model does not account for is the **kind**: the
controlled runs show writes stalling while reads continue, the field reports
full unresponsiveness. That discrepancy is unresolved and it matters, because
full unresponsiveness is the signature of an oversized purge rather than of the
duty cycle.

### What rotation must respect

Each provider logs the writes it applies, including the ones it receives by
syncrepl, so every provider's log grows at the full write rate while only the
inactive ones may purge. With N providers and an even rotation, each is
inactive (N−1)/N of the time, giving the capacity condition

    write rate ≤ delete rate × (N−1)/N

evaluated against that deployment's own measured delete rate. Above that bound
the logs grow without limit no matter how the purges are scheduled, and the
scheme does not apply.

## Options considered

### A. Pace the purge inside slapd — available, untested, and it may moot this ADR

2.7.1 already has the knob. `olcAccessLogPurgeBatch` (`OLcfgOvAt:4.8`,
"Pause purge task after every N entries have been purged") sets
`li_yield_factor`, and the delete loop acts on it: after every N deletes it
sleeps for the measured mean duration of one delete, so the pacing overhead is
about 1/N and *small* N paces hard — N=1 runs the purge at roughly half speed.
The sleep is computed from measured elapsed time, so it scales itself to how
slow the deletes actually are. Default 0 is off, which is what we run today.

It is 2.7-only: 2.6.14 has no such setting and its loop paces not at all, which
is one more thing ADR-021's default buys. It appears in neither the man page
nor `CHANGES`.

Safe to converge live: the set and delete paths are a plain integer assignment
(`accesslog.c:1194`, `:1018`), with no task manipulation and no free, so none of
the live-modify hazards ADR-024 records apply to it.

→ We have never tried it. Pacing attacks the stall at its cause — the purge
monopolising the writer — where rotation only moves the stall somewhere it does
not hurt, so if pacing alone holds p99 at an acceptable level the purge argument
for rotation goes away and only the silent-provider argument remains.

**Out of scope here, and this ADR waits on it.** Pacing is a tunable, not a
topology; it needs no rotation, no endpoint and no phase control, and it is
decided by one load test rather than by this decision.

That load test is not "add one attribute to the existing rig". Since every
measurement we hold is 2.6.10, it needs a **fresh 2.7.1 control arm** before
any paced arm means anything — otherwise a paced 2.7.1 arm against a 2.6.10
control varies two things at once, one of them the per-delete yield above.
Re-running the 15-second comparison on 2.7.1 is worth the same argument: its
whole shape was measured on a loop that never yielded between deletes.

One hazard on 2.7.x blocks measuring there at all, and it is easy to get
backwards — we did, in both directions, before reading the whole branch.

`accesslog.c`'s `skip:` path frees the shared `li_uuid` unconditionally
(`:2173-2176`) while unlocking only for writes (`:2177-2180`), and all four
`goto skip` sites precede the conditional lock at `:1692`. A client **search**
reaches it: `accesslog_op_misc` installs the response callback for bind,
compare, search and extended with no `logops` check (`:2195-2210`), and the
search then fails the `logops` test at `:1673` *because* we log writes only,
and jumps to `skip`. So `olcAccessLogOps: writes` routes searches into the
unprotected free rather than away from it.

`li_uuid` is non-NULL for exactly the window that matters. Its only populating
site (`:2279`) sits in the **else** of the `li_oldf` test at `:2254`: with
`logold` set a write dups the old entry into `li_old` and never touches the
uuid; with `logold` unset — slaptain never sets it, and there is no CRD field —
every logged write populates `li_uuid` and holds it until the response clears
it at `:1716`. Any search completing inside that window frees it.

→ A read-dominated directory is the **worst** case, not a safe one, and that is
the profile this ADR targets. At our measured rates the overlap would be
continuous. The free is not a 2.7 regression (2.6.14 carries the identical
`skip:` body and the same seven `li_uuid` sites), which is consistent with the
project running 2.6.10.

**This blocks the option A measurement on stock 2.7.1 under realistic load.**
master removes the class structurally — the uuid moves into a per-operation
`lmodinfo` with an inline buffer — but no release carries it, and a backport is
a refactor of how the uuid threads through `accesslog_response` rather than a
cherry-pick. So the pacing measurement costs a custom image built from an
unreleased tree, or that backport, and either way measures a slapd nobody
deploys.

→ The third course is to **park the pacing question until a release carries
the fix**, and record here that it is blocked on upstream rather than on us.
That costs nothing this ADR needs: option A changes the weight of the
justification, never the design, and the second driver — a provider that never
takes a local write never advances its own sid (ADR-036) — is untouched by how
pacing turns out.

`logold` is not a mitigation, and it is worth saying why, because it looks
like one. Setting it closes the uuid path for deletes and modifies only: ADD is
absent from the `:2254` condition and is handled by the same `accesslog_op_mod`
(`:2924`), so every add still takes the else and populates `li_uuid`, as does
modrdn whenever `olcAccessLogOldAttr` is unset. Worse, it would break a feature
we ship: `syncprov_accesslog_uuid_cb` (`syncprov.c:1983`) requires
`reqEntryUUID` and returns `LDAP_CONSTRAINT_VIOLATION` without it, and that
callback drives the search against `si_logbase` — `olcSpSessionlogSource`
(`OLcfgOvAt:1.5`). Sourcing the session log from the accesslog (ADR-034) would
fail for deletes.

→ The two are coupled on 2.7.x and cannot both be had: `reqEntryUUID` exists
only on the branch taken when `logold` is unset, which is the branch that
populates `li_uuid`. What `logold` explains is why the bug is reached by a
*default* configuration rather than an unusual one — the uuid is the fallback
recorded when the richer old-entry path is off.

### B. Purge more often, in smaller batches — refuted

Not rejected on reasoning — measured and refuted. A 15-second interval against
the 5-minute baseline, same load, same configuration:

| | 5-min | 15-s |
|---|---|---|
| achieved | 120,002/s | 116,806/s |
| slapd CPU | 6.29 cores | 6.45 cores |
| disk read | 0.33 MB/s | 6.12 MB/s (18×) |
| disk write | 626 MB/s | 1,050 MB/s (+68 %) |
| p99 | 2.1 ms | 143–230 ms |
| dropped | 0 | ~2,600/s (2.2 %) |

Twenty times the purge frequency costs an 18-fold read amplification,
two-thirds more write volume, a p99 two orders of magnitude worse, and a
service that drops 2.2 % of its load for no gain in throughput. We do not have
an explanation for the shape of this and do not need one to act on it: the
measurement stands on its own, and we record it unexplained rather than
dressing a guess as a mechanism.

→ Intervals should be long, bounded only by how large a single purge may
become.

### C. Stagger the providers' purges, writes stay pinned — WITHDRAWN (third amendment)

Removing the simultaneity costs exactly one thing, phase control. It ends the
worst case every deployment runs today and it is worth having on its own. It
leaves the write target purging under its own writes, so it does not deliver
the quality of service this ADR is about.

→ C and F share one mechanism and differ only in whether the write endpoint
moves with the phase. We build the mechanism once and ship C on its own merits.

### D. `olcReadOnly` on the non-active providers — rejected

Enforcing the rotation at the database rather than the endpoint.

− ADR-036 makes `olcReadOnly` a one-way recovery gate: set at creation,
  released once on own-sid dominance, never re-taken for that incarnation.
  Rotation would make the same attribute a per-phase toggle. Two writers with
  opposite lifetimes is ADR-026 R2, and the failure is silent — a rotation
  clearing read-only on a pod ADR-036 is deliberately holding defeats the gate
  with nothing reporting it.
− It is not needed for correctness. In a multi-provider mesh a write landing on
  a non-active provider is legal: it succeeds and replicates. It is off-plan,
  not wrong.
− It converts a performance policy into client-visible `unwillingToPerform`.

→ We would be enforcing a quality-of-service contract with a correctness
mechanism, against the one attribute that already has an owner.

### E. Gate the purge on observed write quiescence — rejected

Read the candidate's own write counters and purge only when they are quiet.

− It makes a liveness requirement defeasible by the clients it protects: any
  client that keeps writing to the inactive provider defers its purge
  indefinitely.
− The deferral failure mode is the bad one. A deferred log grows, and a grown
  log is the oversized purge that takes a provider out of service.

→ Gate on operator-owned state, never on client-driven state. The rule
survives the rejection and governs decision 4.

### F. Rotate the write endpoint, purge only the inactive providers — chosen

## Decision

### 1. The purge runs where the writes are not

The operator owns a rotation plan and derives from it, for each pod and each
phase, whether that pod is the write target and when its purge fires. The plan
is a pure function of the configured rotation and the clock, with no feedback
from observed traffic, so the decision is unit-testable and the e2e is a thin
integration shell.

### 2. The write endpoint is a Service the operator repoints

Clients reach the write target through a Service the operator moves at each
phase, in lockstep with the purge rotation. We publish a contract:

- follow the write Service and no purge ever runs where your writes land;
- do not, and you may be writing to a provider that is about to purge, and you
  will see the stall.

The cluster stays healthy and the logs stay bounded either way. The blast
radius of not following is confined to the client that does not follow, and
what that client does about it is its own business — we rotate the Service and
the purge together, and the rest is the user's responsibility.

The write endpoint is a recommendation, not an enforcement. A multi-provider
mesh accepts a write anywhere and we keep it that way; we record it as a choice
so it does not read as an oversight later.

**Unverified, and load-bearing:** a Service selector change does not move
established connections — kube-proxy steers new ones while existing conntrack
flows continue to the old endpoint. An LDAP client that pools connections may
therefore keep writing to the former write target for a long time. If the bulk
of write traffic does not follow, the rotation degrades to no rotation and the
former target's log grows. Measuring the share of write traffic that follows a
selector change, and how fast, is the first experiment and it decides the shape
of this decision.

### 3. `olcReadOnly` keeps its single owner

ADR-036 owns the attribute. Rotation does not write it (option D).

### 4. The purge is unconditional

We set the rules: we rotate, we purge. No client behaviour defers a purge
(option E). The only promotion gate is operator-owned state — a provider still
working off its purge backlog or its replication lag does not become the write
target.

### 5. The operator controls phase, not policy

`olcAccessLogPurge` expresses an age and an interval. slapd has no cron-like
schedule and no time-of-day setting, so there is no per-pod *policy* to
diverge: every pod carries the same configured value, exactly as it does today,
and the CR gains no per-pod purge field.

What the operator controls is **phase**, and the 2.7.1 sources say exactly how
far that control reaches:

- Setting the attribute on a task that already exists only assigns
  `re->interval.tv_sec` and leaves `next_sched` alone (`accesslog.c:1077`), so
  editing the value never moves the next fire.
- Removing the attribute deletes the task; setting it again inserts a fresh one
  with `next_sched = time(NULL)` (`rq.c`, `ldap_pvt_runqueue_insert`) — due
  immediately, which is why it fires within the daemon's wake latency. Measured
  under 11 s against a one-minute cycle.

→ Setting `none` and then re-setting the configured value at T places that
pod's purge at T. Placing each pod's purge at a different T is the whole
mechanism, for both option C and this one.

### 5a. Phase cannot be read back, so we re-assert it rather than monitor it

`back-monitor` has no runqueue subsystem: no task list, no `next_sched`,
nothing. A pod's purge phase is not represented in LDAP at all, so the operator
cannot read it, and an operator that merely *set* it once is trusting a value
it can never confirm.

Phase also drifts, in three ways, all of them one-directional:

1. `ldap_pvt_runqueue_resched` sets `next_sched = time(NULL) + interval` at
   **dispatch** (`daemon.c:2784-2785`), not at the scheduled instant, so every
   cycle absorbs that cycle's dispatch latency and never gives it back.
2. If the task is still running when its next fire comes due, the daemon
   reschedules it without running it (`daemon.c:2781-2783`) — a skipped cycle,
   re-phased by a whole interval's lateness.
3. A restart schedules the task from the restart instant.

Drift from the purge's own duration is **not** among them: `resched` is called
before the task is submitted to the pool, so a long purge does not push its own
next fire out.

→ We do not monitor phase and we do not chase drift. The operator **re-asserts**
each pod's phase — `none`, then the configured value — at the moment that pod
enters its inactive window, in the same step that moves the write Service. One
write per pod per rotation: at 6- or 12-hour phases, a handful a day. Whatever
drift accumulated since the last rotation is discarded rather than measured,
and a restart needs no special handling because the next rotation overwrites it
anyway.

Re-asserting is cheap on the overlay's side and not free on the server's. The
delete path stops the task if it is running, removes it from the runqueue and
zeroes `li_age`/`li_cycle` (`accesslog.c:955-967`); the set path parses the two
values and inserts a fresh task. No flush, no re-open, no overlay
re-initialisation, nothing touches the database. Two details follow from it:
`logpurge` carries both age and interval, so the re-set must supply the age
again, and purging is off between the two writes.

The cost is elsewhere. `config_back_modify` pauses the **whole server** —
`slap_pause_server()` is `ldap_pvt_thread_pool_pause()` plus every backend's
`bi_pause` — and the only case that skips it is an internal `contextCSN`
update (`bconfig.c:6426`, `:6475`, `:6512`). So each re-assertion costs two
server pauses on that pod, one per write, each lasting as long as the in-flight
operations take to drain. The purge itself does not extend them: its delete
loop calls `ldap_pvt_thread_pool_pausewait()` after every entry
(`accesslog.c:801`), so a pause waits for one delete rather than for a whole
purge. This cost is not special to this feature — every `cn=config` write the
operator makes pauses that pod's server — but a rotation makes it recurring,
and that is a reason to re-assert once per phase rather than often.

The principle underneath this is worth naming, because it is the same failure
twice over in the work that produced this ADR. A load-run record said which
purge arm was *requested* rather than which was live, so a sequence killed
mid-arm silently mislabelled the next two runs; and the measurement table said
nothing about its slapd version, so it read as version-neutral when every row
was 2.6.10. Both are a record that describes an **intention** rather than a
**state**, and no threshold catches either — the numbers look fine, they are
simply about something else.

→ There are two cures and only one is available here. Where the state can be
read, read it: the harness now captures `slapd -VV` and detects the live arm
instead of assuming its own last instruction. Where it cannot — and a purge
phase cannot — the answer is to make the intention authoritative by asserting
it often enough that drift never accumulates, which is what this decision does.
Re-asserting is not a convenience chosen over monitoring; it is what is left
when monitoring is not on offer. The same shape is a live defect elsewhere in
the operator: `olcAccessLogOps: writes` is an intention written once at overlay
creation and never re-asserted (see `docs/BACKLOG.md`).

This makes the configured interval a fallback rather than the schedule. Set it
no shorter than a full rotation, or slapd fires on its own part-way through a
phase — possibly on the write target, which is the one outcome the feature
exists to prevent. If the operator stops, each pod keeps purging at its last
asserted phase and the rotation simply stands still.

### 6. Rotation is optional, and sizing it is the user's job

The feature is a switch. Whether it defaults on or off is open and does not
change the rest of this ADR.

A user who turns it on takes on the arithmetic: expected write rate, resulting
log size per phase, and the capacity condition above against their own storage.
We do not police it. The operator will not refuse a rotation whose phases are
too long for the write rate, and it will not shorten one to keep a purge small.

The hazard that arithmetic exists to avoid is measured: draining ~250,000
entries at a one-minute cycle restarted both providers — no crash captured,
matching the accesslog hang where slapd stops answering and readiness fails —
and produced 255 s of replication lag.

An oversized purge costs lockstep as well as service. A purge still running
when its next fire comes due makes the daemon skip that cycle and re-phase the
task (decision 5a, drift 2), so the pod whose purge ran long is also the pod
whose phase has moved. The next rotation re-asserts it, which bounds the damage
to one phase.

Expect long phases. OpenLDAP's own default purges roughly daily; the 5-minute
and 15-minute figures in this ADR are synthetic load-test values, and a
production-shaped deployment is likelier to sit at 6 or 12 hours. Option B
makes the same argument from the other side.

## Consequences

The disturbance moves rather than disappearing. It lands on the inactive
provider's replication lag, which it works off before becoming the write target
again — decision 4's promotion gate is what keeps that from becoming the next
outage.

Both providers' sids keep advancing. A provider's own sid in `contextCSN` is
restored only by a local write, so pinning writes to one provider leaves the
other's stale, which is part of why an unclean death needs a write under the
absent sid to recover (ADR-036). Rotation removes that by construction. This is
a second, independent reason to rotate, and it argues for rotating even at
write rates where the purge stall does not hurt.

Small amounts of off-target write traffic are harmless — each costs one brief
writer acquisition. The scheme needs the bulk of writes to follow, not all of
them.

The feature earns its place on write-heavy deployments. Where the write rate
sits far below the delete rate the duty cycle is negligible and the stall is
not resolvable; those deployments want option C's staggering, which needs the
same phase control and neither the endpoint nor the rotation.

That low-rate claim rests on a measurement of the `both` arm — accesslog
checkpoint plus accesslog-sourced session log — and there is no clean
`sessionlog` measurement at that load to set beside it. The direction is
favourable: `both` is the more expensive arm wherever the two separate (+19 %
peak replication lag at 190k, +12.9 % at 100k), so the bound is conservative
rather than wrong. It is a bound measured on the worse configuration, which is
the right way round for a claim that the feature can be skipped, and it is
still one arm of one rig on 2.6.10.

We gain a write-endpoint Service, which is new user-facing surface and a
migration for anyone pointing at the existing ClusterIP.

### What is proven and what is not

Proven: the mechanism and the A/B table above, one clean run per arm plus a
symmetric control; that short intervals make everything worse (option B); that
purge phase is choosable by the delete-and-re-set sequence in decision 5; and,
from the 2.7.1 sources, that phase is unreadable and drifts one way only
(decision 5a).

Not proven: the rotation itself. Both measured arms pinned writes to the same
pod and varied only which pod purged. We have never moved the write target.
Open, in the order they block the design:

1. what share of write traffic follows a Service selector change, and how fast
   (decision 2);
2. whether the field's full unresponsiveness is the duty cycle or an oversized
   purge;
3. how long an inactive provider needs before it may take writes;
4. what happens to in-flight writes at the switch;
5. whether re-asserting phase once per rotation holds lockstep in practice, or
   whether the drift inside a single 6- to 12-hour phase is already enough to
   matter (decision 5a).

Until 1 is answered, decision 2 stands on a mechanism we have not measured, and
the fallback is option C — staggering, which needs neither the endpoint nor the
rotation and is worth shipping on its own.

Separate from those, and prior to the whole ADR: whether
`olcAccessLogPurgeBatch` makes the purge stall tolerable without any of this
(option A). It is not an open question *of* this design — it is the question of
whether this design is needed.

### Test discipline

The rotation plan is a pure function of the configuration and the clock: which
pod is the write target at T, which pod's purge fires at T, and whether a
candidate is eligible for promotion. That decision gets assertions written from
this ADR before the implementation, per the project's red-first rule. The e2e
carries what only a cluster can show — that the stall follows the purge and not
the clock, and that a rotation does not drop a write.

This is a replication-path change: it alters when and where log entries are
deleted, and it moves an endpoint clients use. It sees a multi-site e2e cycle
before merging, or the commit says plainly that multi-site is unvalidated.

## Related

- ADR-019 — one accesslog DB per replicated data DB; the environments this ADR
  purges, and why a shared log would make the stall cross databases.
- ADR-020 — the accesslog ACL; unchanged, and the reason the purge is the only
  bulk deleter of that DB.
- ADR-021 — 2.7.1 by default; `olcAccessLogPurgeBatch` exists only there, so
  option A is available to the 2.7 images and not to the `-ol26` pair.
- ADR-024 — placement classes; `olcAccessLogPurge` is R1, converged per pod,
  and decision 5 keeps it there with one configured value for every pod.
  `olcAccessLogPurgeBatch` is R1 too, and currently unset by the operator.
- ADR-002 — `cn=config` is node-local; what makes per-pod phase control
  mechanically available.
- ADR-026 — R2; why decision 3 leaves `olcReadOnly` alone.
- ADR-030 — Ready means slapd answers LDAP; the readiness an oversized purge
  already breaks.
- ADR-035 — how a collision with ADR-036 would be decided, if decision 3 is
  ever revisited.
- ADR-036 — owns `olcReadOnly`; and the own-sid recovery this ADR improves as a
  side effect.
- `docs/BACKLOG.md` — "The accesslog purge stalls the pod that runs it, and we
  pin writes to one pod": the measurement, the A/B table and the hazard.

---

## Amendment, 2026-10-18: a phase is a value, and decision 5a is a workaround

Decisions 5 and 5a were written against the scheduler slapd ships, where the
next purge is `now + interval` computed at dispatch. We now carry a local patch
that adds an optional third value — `olcAccessLogPurge: <age> <interval>
[<phase>]` — firing the task at `phase + k*interval` counted from the epoch
(`images/openldap-deb/local-patches/accesslog-purge-phase.patch`, built by
`images/openldap-deb/Containerfile.ol26src`). It is ours, not upstream's, and
it is deliberately absent from the 2.7.1 packaging fork's series.

**5a is retired where the patch runs.** All three drift sources it describes
come from deriving the next run from the moment of dispatch; deriving it from
absolute time removes them together. Dispatch latency no longer accumulates, an
overrun lands on the next grid slot rather than re-phasing by a whole interval,
and a restart returns to the same grid instead of re-phasing to the restart
instant. The half of 5a that stands is that phase still cannot be read back —
`back-monitor` has no runqueue subsystem — but that stops mattering once
nothing drifts: re-assertion was only ever needed because the value would not
stay put. Re-asserting once per rotation becomes the fallback for an unpatched
slapd, not the mechanism.

**5's premise inverts.** It says there is no per-pod *policy* to diverge, so
every pod carries the same configured value and the CR gains no per-pod purge
field. That was true of stock slapd and is false with a phase: the phase is
exactly a per-pod value, and something must derive it — ordinal × step, or an
explicit per-pod assignment. The operator reconciles `olcAccessLogPurge`
uniformly from the CR today, so **the patch alone does not produce a stagger**;
it only makes one expressible. A `SlapdDatabase` field carrying the step, plus
a per-pod derived value, is now required work rather than a thing this ADR
could decline.

The failure mode if that is missed is quiet, and was nearly met in practice: a
driver written on the assumption that the server multiplies the value by the
pod's ordinal would write one phase for every pod, the providers would stay
synchronised, and the arm would report no difference — reading as "the patch
does nothing" rather than "the stagger never happened".

**And the case for rotation narrows, honestly.** Option C — staggering, which
this ADR adopted as the first step — was costed as phase control plus standing
operator work to maintain it. With the phase it is one declarative value per
pod and no standing work at all. What full write-endpoint rotation still buys
over it is the elimination of the **write target's own** stall, and the second
driver (every provider's sid keeps advancing, ADR-036). It no longer buys the
de-synchronisation, because staggering now delivers that by configuration.

---

## Amendment, 2026-10-18 (second): the rotation, specified

Decisions 1 and 2 said the purge runs where the writes are not, and that a
Service carries the write endpoint. They did not say how either is scheduled.
With the phase available as a configuration value, that can now be written
down.

### The grid

With `n` read-write pods and a slot length `t`, the period is `T = n·t` and
pod-k purges at phase `k·t`. The writer rotates every `t`, in lockstep.

**`n` is `spec.replicas` — declared, never observed.** It has to be, because
`n` determines `T`, and `T` is the interval written into every pod's
configuration: deriving it from readiness would rewrite the whole fleet's
`cn=config` on a single transient restart, which is the churn ADR-013 exists to
prevent. Readiness belongs in writer *selection*, which persists nothing.

**`t` is fixed and `T = n·t` varies**, not the reverse. Scaling then leaves
every existing pod's phase alone and gives the new pod `n·t`; only the interval
moves, and changing an interval does not move a phase. Holding `T` fixed and
recomputing `t = T/n` would re-phase the entire fleet on every scale event.

### Where the writer sits: `d`

Let `d` be the number of slots between a pod's own purge and its writer slot.
Two constraints bound it, one at each end:

- it must have finished purging before it writes: `d·t ≥ P`, for purge
  duration `P`;
- writes must have drained before it purges again: `(n−d)·t ≥ D`, for drain
  time `D`.

→ Adding them gives the sizing rule, **`T ≥ P + D`**, and leaves `d` free in
`[P/t, n − D/t]`. `d` is therefore a tunable with a measured range rather than
a fixed choice, and it stays one until there is evidence for a winner.

`d = n−1` — the writer is the next pod to purge, having had the longest time to
recover — is the preferred end and the one being measured first. Note what it
does to this ADR's capacity condition: `write ≤ delete·(n−1)/n` assumes the
purge may use the whole non-writing window, which is exactly `d = n−1`. At
`d = 1` it tightens to `write ≤ delete·(1/n)`, so the choice that looks safer
costs roughly half the write ceiling.

Consequence of `d = n−1` worth watching: the writer purges immediately after
its slot ends, carrying its freshest local writes with syncprov actively
serving them. `d = n−2` buys a cooldown slot and exists only for `n ≥ 3`. We do
not know whether it matters.

### Writer selection

At slot `s`, walk the preference order `d = n−1, n−2, … 1` and take the first
pod that is Ready, is not purging this slot (never `d = 0`), is not read-only
under ADR-036's authorship gate, and is not working off purge backlog, lag, or
a recent absence (decision 4's promotion gate).

With `n = 2` and one pod down the only candidate is the purging pod. Take it
and accept the stall: writes continuing badly beats writes not continuing.

A returning pod needs no special handling. Its phase is absolute, so it rejoins
its own slot at the next boundary without re-phasing anyone — which under the
stock scheduler would instead have re-phased it to the restart instant and
collided silently with whoever owned that slot.

### Databases align, they do not spread

A cluster with `D` replicated databases has `D` accesslogs and `D` purges per
pod per period. They are **aligned**: pod-k purges all of its databases at
`k·t`, giving one busy window per pod of duration `ΣPᵢ`.

Spreading them over `n·D` slots would isolate each purge and defeat the purpose
— every pod would then be intermittently disturbed throughout the period, and
the writer slot would have to dodge each database separately. Concentrating a
pod's disturbance into the window where it is not writing is the whole point.

### The reads

Reads go to a service of the read-only replicas plus the current writer. The
argument is **freshness, not latency**: only one provider purges at a time, so
latency alone would justify excluding only that one, but a provider that has
just purged is working off replication lag measured peaking near 35 s, and
under `d = n−1` the writer is by construction the most recovered provider.

It is a quantitative improvement against an already-asynchronous baseline, and
its cost scales inversely with `readReplicas` — at `readReplicas: 0` the read
service collapses onto one pod, which at a read-dominated profile is a cliff.
The default therefore follows `readReplicas`, with "every pod that is not
purging" available as the other mode. This is one of the few places where the
right answer is a property of the deployment rather than of the design.

### Where the configuration lives

`olcAccessLogPurge: <age> <interval> <phase>` has two owners, not one:

- `<age>` is a per-database retention policy and stays on `SlapdDatabase`;
- `<interval>` and `<phase>` are a schedule over pods and move to the cluster.

The operator composes the per-(pod, database) value from both. This removes the
consistency problem without cross-resource validation — databases cannot
disagree about a schedule they no longer own. Per ADR-024, the interval half of
the existing per-database field must then be refused when cluster rotation is
on rather than silently overridden.

### Unmeasured

`P` against `t` sets `d`'s lower bound and is computable from the running
sequence (`T × write rate / delete rate`); `D` sets its upper bound and has no
value at all yet. Until both exist, `d` is a parameter and `T ≥ P + D` is a
rule we cannot yet evaluate for any concrete deployment.

---

## Amendment, 2026-10-18 (third): staggering alone was measured, and does nothing

Option C is **withdrawn**, and the narrowing in the second amendment is
**retracted**. Both rested on the assumption that de-synchronising the
providers' purges would recover part of the disturbance. It recovers none of it.

Four arms at 100k ops/s, 0.3 % writes, 60-minute plateaus, medians over steady
cycles, all on one patched 2.6.10 binary. Writes were pinned to pod-0 for every
arm and pod-0 purged in every arm; only pod-1's phase varied:

| arm | achieved | quiet p99 | disturbed | stall | dropped/cycle | peak p99 | peak CSN |
|---|---|---|---|---|---|---|---|
| sync | 96,286 | 2.82 ms | 60 s | 30 s | 944,359 | 2,369 ms | 19.6 s |
| stagger | 96,780 | 2.98 ms | 60 s | 30 s | 949,123 | 2,904 ms | 34.7 s |
| stagger (repeat) | 97,206 | 3.12 ms | 60 s | 30 s | 958,468 | 2,963 ms | 39.2 s |
| peer-only (earlier) | 99,838 | 1.2 ms | **0 s** | **0 s** | **0** | 1 ms | 34.8 s |

The staggered arms land on the synchronised arm and nowhere near peer-only:
identical disturbance and stall, dropped work within 1.5 %. The stagger itself
was verified rather than assumed — a measured separation of ~120 s against a
configured 150 s, within one 30 s sampling interval, where the synchronised arm
measured 30 s of pure scrape jitter.

→ **The only variable that matters is whether the write target purges.** Moving
a peer's purge half a cycle away changes nothing a client can see, because the
write target's own purge was stalling its own writes the whole time.

### What this does to the decisions

Staggering is therefore **necessary but insufficient**: with every pod purging
at the same instant there is no non-purging pod for the writer to sit on, so
per-pod phases are a *prerequisite* of rotation rather than a cheaper
alternative to it. Option C's "ship it on its own merits" was wrong — it has no
merits alone. It ships as part of F or not at all.

Rotation's case is restored whole. The second amendment reasoned that a phase
makes staggering declarative and so leaves rotation buying only the write
target's own stall; the measurement says the write target's own stall was
always the entire quantity. Nothing is subtracted from F.

### A measured number this ADR was carrying wrongly

The delete rate under load was reported as ~1,000/s. That divided entries
removed by the *disturbance* window rather than the *stall* window. Measured
per cycle across both arm families — 73-77k entries removed against a 30 s
stall — it is **~2,500/s under load**, against ~2,800/s idle on the same
storage. Contention costs about 10 %, not the threefold this ADR implied.

Consequences: the duty cycle `write rate / delete rate` is correspondingly
lower, the capacity condition has more headroom than stated, and purge duration
is `P ≈ interval × write rate / 2,500` on this storage — about 36 s at
300 writes/s on a five-minute interval, against a measured 30 s stall.

`P` cannot be read from the log on 2.6: the `etime`/`nentries` line sits inside
a `LogTest( LDAP_DEBUG_SYNC )` block that exists in 2.7.1 and not in 2.6.x,
which carries only the minCSN message under the same tag. At `logLevel: 16640`
a 2.6 pod emits no purge lines at all, so on 2.6 `P` must be derived as above.

### Open, and deliberately not resolved here

Peak CSN lag is *higher* staggered (34.7 s and 39.2 s against 19.6 s). One
plausible mechanism is that synchronising is accidentally good for replication:
while the write target is stalled there is nothing new to replicate, so the
peer's stall overlaps a quiet period. Separating them makes the peer stall while
traffic flows. A second synchronised arm was still running when this was
written, and it decides whether 19.6 s was an outlier or the inversion is real.
Either way it does not touch the client-visible conclusion above.

Every arm is the 2+2 shape. There is no three-provider data, so the `d = n−2`
cooldown question stays unknown rather than answered badly.
