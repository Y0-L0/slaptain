# ITS#9398: how the ecosystem handles it, and what we could do

Decision input, gathered 2026-10-04. The bug itself is in
[`ITS-9398-REPRODUCTION.md`](ITS-9398-REPRODUCTION.md); this file is only about
*coping with it*.

> **Revised 2026-10-04** after the reproduction and heal experiments. The
> mitigation table now reflects two distinct failure modes, a reframed option
> A, a validated option D and F, and a new option I. The survey sections above
> it (detection, avoidance, shutdown, recovery, Kubernetes) are unchanged and
> still current — they are about what the *ecosystem* does, which has not moved.

Every claim is labelled by provenance — **upstream doc** / **maintainer** /
**vendor or distribution** / **folklore** / **our inference** — because we have
already been burned once by reading a code comment as a ticket.

## The short version

Nobody handles it. Upstream has had a maintainer-filed ticket open since 2020
whose fix is blocked behind a SUSPENDED enhancement, and has never published a
workaround. No delta-syncrepl recipe anywhere puts a checkpoint on the log DB.
No monitoring tool reads the accesslog's CSN state at all.

The ecosystem tolerates this because the trigger — an unclean stop — was rare on
long-lived hosts. **Kubernetes falsifies that assumption**, which is why it
keeps biting us and not them.

## Two maintainers disagree, and our reproduction is the tiebreaker

**Maintainer**, Howard Chu, openldap-technical 2011-12
(<https://www.openldap.net/lists/openldap-technical/201112/msg00273.html>):

> "the syncprov overlay always reads the largest entryCSN out of the database at
> startup time anyway… Checkpointing the CSN is only an optimization, it will
> reduce the length of the search time on the next startup."

**Maintainer**, Quanah Gibson-Mount, ITS#9398, 2020:

> "Depending on the syncprov overlay checkpoint settings (usually no
> checkpointing is enabled on the accesslog db) this can cause the system to
> **refuse engage in replication** at startup."

Both cannot be right for a log DB. This matters for the ITS report: the
"checkpoint is only an optimization" position is probably why the directive was
left out of every example, and our measurement is direct evidence against it in
the delta-syncrepl case.

There is also **maintainer evidence that a log DB's `contextCSN` is
load-bearing**, which undercuts the folk reading of Chu's remark. **ITS#9361**
(Chu): logpurge made *"the logDB's contextCSN become newer than its main DB's…
trigger the syncprov on the logDB to send NEW_COOKIE messages to all delta
consumers."* So its value demonstrably drives consumer behaviour — it is not
merely a startup optimisation.

Two cautions for option A from the same history: **ITS#8607**, *"Fixed
slapo-syncprov not to log checkpoints to accesslog db"* — checkpoints and
accesslog have interacted badly before; and **ITS#8100**, where upstream's
answer to log-DB recovery was entry-level persistence rather than a checkpoint.

## 1. Detection — nobody watches the right thing

**Maintainer**, Ondřej Kuzník, openldap-technical 2023-02-28
(<https://marc.info/?l=openldap-technical&m=167757941621578&w=2>):

> "yes, accesslog's contextCSN should always be in sync with its main DB."
> "in δ-multiprovider both main DB and accesslog (and their contextCSNs) are
> used together and **should be monitored as such**."

He also names our error class as *"the most common reason to see
e-syncRefreshRequired - 4096"*, and notes in passing that transplanting an
accesslog is *"safe **if** server was shut down cleanly and everything was
flushed into both"* — a maintainer acknowledging the durability dependency.

**Documented absence.** Every shipped tool whose source or docs we read checks
the **data** suffix only: LTB's `check_ldap_syncrepl_status`,
`check_syncrepl_extended`, the Prometheus exporters (which read `cn=Monitor`
counters plus explicitly configured `--replicationObject` DNs — nothing stops
you listing an accesslog suffix, but none ships it). `slapd-monitor(5)` mentions
syncrepl, replication state and CSNs **zero times**, so `cn=Monitor` is not a
route in. **No tool anywhere reads `minCSN`.**

### syncmonitor: read, and it is worse than "does not cover the accesslog"

The gap flagged above is closed. [`syncmonitor`](https://git.openldap.org/openldap/syncmonitor)
(1622 lines, `f2debe3`) was read in full on 2026-10-04. It is **written by
Ondřej Kuzník** — the same maintainer who said both contextCSNs "should be
monitored as such". His own tool does not do it.

**It is not a poller.** It opens a real syncrepl session (`SyncRequestControl`,
`refreshAndPersist`) against a user-supplied `-b base`, with filter `(|)` —
RFC 4526 absolute-false, matching nothing — and tracks only the cookies the
provider sends. A zero-entry passive replica. `contextCSN`, `minCSN`,
`accesslog` and `entryCSN` appear **zero times** in the source.

**And it silences our exact symptom, by number.** `environment.py:199`:

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

`0x1000` is 4096, `e-syncRefreshRequired` — the storm signal. It sits in the
allowlist of outcomes that **do not even log a warning**, next to time- and
size-limit exceeded. The handler then discards the cookie and reconnects. The
observer does the same on the in-flight path (`syncrepl_observer.py`: *"Refresh
required (cookie was rejected), forget it before we propagate"*).

So the Project's own monitoring tool, pointed at a provider stuck in ITS#9398,
reports **In-sync** and logs nothing. It is not merely blind to the cause; it is
explicitly deaf to the effect. Its `BROKEN` state comes only from a 30 s
no-progress watchdog, which an idle directory trips anyway.

*Two things this changes for us.* First, the claim above is now stronger than
"every tool checks the data suffix only" — syncmonitor does not check a suffix
at all, and still misses it. Second, there is a real affordance here: `-b` is
just an argument, so syncmonitor **can be pointed at `cn=accesslog-<db>`**. That
is the only off-the-shelf thing we found that could observe log-DB cookie state,
and nobody documents doing it.

*Method note.* A grep for `4096` returned zero and nearly produced a false
"handles it not at all" — the constant is written `0x1000`. Same lesson as the
`slapcat` near-miss in CLAUDE.md: count greps do not substitute for reading.

**Maintainer**, Quanah, ITS#10173 comment #3 — the only published `minCSN`
health check in existence, and it checks *cardinality*, not our ⊆ relationship:

> "restart slapd with sync logging enabled… `accesslog_purge: updating minCSN
> with 4 values`. In an environment where there are only 2 providers, this
> indicates the issue is present."

**Folklore, better-sourced than we thought.** The 2006 Poliakoff report — the
origin of the "one write" cure — has a follow-up (msg00068) mentioning *"a slapd
segmentation fault occurred during his maintenance window"*. So the 2006 case
was itself an **unclean stop**: a true ITS#9398 instance fourteen years before
the ticket, not merely an adjacent symptom. Chu attributed it at the time to
`slapadd` without `contextCSN`.

**Folklore, mechanically exact.** openldap-technical 2023-10-13, "contextCSN via
slapcat outdated, despite having set syncprov-checkpoint": a user noticed
`slapcat` returning stale `contextCSN` while `ldapsearch` returned correct
values on all four servers. That is our in-memory-versus-on-disk split, observed
by accident, on the data DB, by someone who got no substantive answer.

→ The pre-failure signal exists only on disk, and nobody looks there.

**Maintainer**, Quanah, openldap-technical 2021-07-06 — the closest public
thread to our failure mode (a consumer dropped into REFRESH after a *provider*
restart): *"I generally advise using some type of monitoring on the CSNs for
each server so you can quickly be notified when such an issue has arisen."* He
does not say which CSNs, and says nothing about the log DB.

### And we implement exactly the half Kuzník calls insufficient

**Verified in our own code, 2026-10-04.** Both probe paths cover the data
suffix only:

- `operator/internal/controller/slapdcluster_controller.go:1380` passes
  `db.suffix`, and `slapdbackup_controller.go:397` passes `sd.Spec.Suffix` —
  suffixes that come from `SlapdDatabase` CRs. An accesslog DB has no CR, so it
  is never probed.
- `slctl` skips it *deliberately*, in two places:
  `cmd/inspect.go:452` (`continue // accesslog / internal DBs: journals, no seed
  identity of their own`) and `cmd/inspect.go:1245`, whose doc comment names the
  exclusion outright.

That skip was right for what it was written for — ADR-025 suffix health, where a
journal has no seed identity. It is exactly wrong for ITS#9398. Note the hard
limit though: both paths read over LDAP, i.e. the **in-memory** value, which
stays healthy right up to the restart that matters. Probing the log DB detects
the *post-restart* broken state, not the pre-restart exposure — only reading the
on-disk value (`slapcat`) sees it coming.

## 2. Avoidance — the absence is near-total

**Upstream doc.** The Admin Guide's delta-syncrepl example — identical in 2.4,
2.5, 2.6, 2.7 — gives the log DB `nopresent` + `reloadhint` and nothing else,
then puts `syncprov-checkpoint 1000 60` on the main DB twenty lines later.
**Upstream's own regression fixtures do the same.**

**Vendor.** Symas, verbatim: *"Define only the syncprov overlay for the
Accesslog database"* → `olcSpNoPresent`, `olcSpReloadHint`. Checkpoint on the
main DB.

**Distribution/community, all without a log-DB checkpoint:** Quanah's own
replication gist, Ubuntu Server Guide, Gentoo Wiki, Zytrax, Zimbra and forks,
and ~20 surveyed Ansible/Puppet/Salt/compose configs.

**The two exceptions**, both apparently by uniform parameter application rather
than reasoning: `bodgit/puppet-openldap` (also sets a sessionlog on the log DB,
which ADR-022 rejects) and `shawnmckinney/openldap-mmr-tutorial`. **Two
near-misses** — `peppelinux/ansible-slapd-eduperson2016` and `ResEl-TB/myresel`
carry `# olcSpCheckpoint: 100 10` *commented out* on the accesslog overlay.
Somebody considered it and backed off.

→ Strong evidence that **every delta-syncrepl deployment is exposed**, and the
omission is upstream-originated and copied.

## 3. Clean shutdown — and the number that indicts our config

**Upstream doc**, Admin Guide §7.3, in full: `kill -INT \`cat slapd.pid\``, and
*"Killing slapd by a more drastic method may cause information loss or database
corruption."* `slapd(8)` says nothing about signals at all.

**Maintainer**, Kurt Zeilenga, ITS#1227, **2001-07-09** — our problem, stated
twenty-five years ago:

> "N (5?) seconds of real time between TERM and KILL signals does not
> necessarily give any time, let alone enough time, to slapd to shutdown."

The intended resolution (slow-safe SIGINT, fast-unsafe SIGTERM) was never
implemented; the two signals remain identical.

**Distribution practice — no slapd unit anywhere sets a stop timeout:**

| Packaging | Stop configuration |
|---|---|
| upstream `slapd.service` | no `TimeoutStopSec`/`KillMode`/`ExecStop` |
| Debian trixie 2.6.14 | none — verified in the packaging source |
| Debian ≤ bookworm | no unit; SysV via sysv-generator |
| CentOS 7 / RHEL `openldap-servers` | none (RHEL 8+ ships no slapd) |
| openSUSE Factory, Fedora rawhide | none |
| Symas | shutdown never mentioned in their systemd docs |

So the effective budget everywhere is systemd's **`DefaultTimeoutStopSec` = 90 s**.
The one stricter case is Debian's init script: `--retry TERM/10` — SIGTERM, wait
10 s, then **report failure rather than escalate to SIGKILL**. Written as
`--retry 10` it would have expanded to `TERM/10/KILL/10`; the restraint looks
deliberate.

> **Our pods use the Kubernetes default of 30 s with no `preStop` hook. That is
> shorter than every slapd packaging surveyed, and the only shorter one refuses
> to SIGKILL at all.**

**And nobody has carried the requirement into Kubernetes either.** Symas's own
Helm chart (`symas/helm-openldap`) sets **no `terminationGracePeriodSeconds`**
anywhere in `templates/statefulset.yaml` and ships `lifecycleHooks: {}` — so
30 s, no preStop, and no log-DB checkpoint. Same for `jp-gouin/helm-openldap`
and `MaximeWewer/openldap-platform`. *The vendor that filed the bug ships a chart
that runs into it.*

**A second, independent argument for ADR-030.** A liveness probe is an
unclean-shutdown *generator*: it manufactures exactly the event this bug needs,
on a healthy-but-slow server. All three charts above ship one by default. We
dropped ours for an unrelated reason (a frozen pod should leave the Service, not
the cluster); ITS#9398 would have demanded the same decision on its own.

Two details worth carrying: Debian's `slapd.service` is **new in trixie** —
bookworm (2.5.13) has no unit at all, only the sysv-generator plus a drop-in
containing nothing but `RemainAfterExit=no`. And a widely-echoed claim that
*"Symas gives slapd 30 s and recommends 60 s+"* has **no primary source** — it
could not be located in their KB or packaging. **Do not cite it.**

**Documented absence:** nobody anywhere states how long a clean slapd shutdown
takes with a large back-mdb. The pathological case is unbounded (a 2013 report
of *"waiting for 12 operations/tasks to finish"* still alive after five
minutes), but no credible report of slapd being SIGKILLed at a systemd *stop*
timeout was found. So 30 s is unusually tight by ecosystem standards, **but has
not been shown insufficient.**

**Our inference, and it is worth stating:** LMDB is marketed as *"crash-proof,
no recovery needed after restart"*, which is true of the **data** and false of
syncprov's in-memory `contextCSN`. That marketing is probably why nobody felt
pressure to lengthen slapd's stop timeout after BDB was dropped — and in a
container context, where SIGKILL is scheduled rather than accidental, it is
actively misleading.

## 4. Recovery — the vendor ladder

**Vendor**, Symas "Solving Common Problems"
(<https://www.symas.com/kb/article.php?slug=solving-common-problems>), verbatim
— quote and both rungs re-verified against the live page 2026-10-04:

> "issue an modification to the Producer's primary MDB database which will
> create a new and updated contextCSN attribute value. If this does not resolve
> the problem, **replace the Consumer's accesslog database with a copy of the
> Producer's accesslog database**" — stop, `rm -rf …/accesslog/*.mdb`,
> `slapcat -n 2` on the producer, `slapadd -n 2` on the consumer.

So our "one write from the absent serverID" cure **is vendor-documented**, not
merely folklore — and the 2006 sighting that first recorded it followed *"a
slapd segmentation fault during his maintenance window"*, i.e. an unclean stop.

**Maintainer**, Quanah's ITS#10173 repair is the canonical log-DB shape: stop →
`slapcat` the log DB → hand-edit the context entry's CSN attributes → delete the
`.mdb` files → `slapadd` → restart.

**Upstream doc**, `slapd(8)`: `-c rid=<n>` — *"Use only the rid part to force a
full reload"* — the sanctioned consumer-side reset.

**Documented absence:** no upstream recovery procedure for a *provider* stuck
refusing every consumer. Not in the guide, not in ITS#9398, not in any release
note.

## 5. What transfers to Kubernetes

**Does not transfer:**

- *"A human notices and makes a trivial modification."* Every recovery in the
  ecosystem assumes an operator with a shell and the authority to stop a node.
- *"Long-lived hosts make unclean stops rare."* This is the assumption the whole
  ecosystem's tolerance rests on, and Kubernetes deletes it: rollouts, drains,
  evictions, OOM kills and image updates make the trigger routine.
- *"LMDB is crash-proof so killing slapd is fine."* True for data, false for
  syncprov state, and dangerous advice in a container.

**Transfers directly:**

- `kill -INT` → SIGTERM to PID 1. Already correct; slapd is PID 1 and the two
  signals are identical.
- systemd's 90 s → `terminationGracePeriodSeconds`. Exact analogue, and Zeilenga
  2001 is literally a complaint about this mechanism.
- "Start the producer before the consumer" → `OrderedReady`. Already have it.
- Kuzník's monitor-both-contextCSNs → extend our existing per-database CSN probe
  to `cn=accesslog-<db>`. Nothing in the ecosystem ships this.

**Worse under Kubernetes.** **Upstream k8s doc**
(<https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/>):
`shutdownGracePeriod` and `shutdownGracePeriodCriticalPods` *"are set to zero,
thus not activating the graceful node shutdown functionality"* by default. **On
a default cluster a node reboot gives slapd no grace period whatsoever** —
strictly worse than any init system. No pod-side setting fixes that.

**A new, self-inflicted source.** A liveness probe would be an unclean-stop
generator with no pre-k8s analogue. ADR-030 already refuses one for a different
reason; this is an independent second argument for the same decision. (The one
"production-oriented" k8s delta-syncrepl platform found,
`MaximeWewer/openldap-platform`, ships a livenessProbe, no grace period, no
`preStop`, and no log-DB checkpoint — every exposure at once.)

## Candidate mitigations

No recommendation; this is the menu for the decision.

| | Option | Evidence for | Evidence against / cost |
|---|---|---|---|
| **A** | `olcSpCheckpoint` on every `cn=accesslog-<db>` | ITS#9398 names the missing checkpoint as the cause; our measurement (`1 1` clean, `100 5` storm). **Reframed 2026-10-04:** this is not belt-and-braces — a checkpoint *restores the safety net `logpurge` removes*. slapd rebuilds a lost `contextCSN` by scanning for the greatest `entryCSN`; purging deletes exactly those entries. **Strengthened 2026-10-23:** the purge is not what opens the window — `accesslog_response` persists `minCSN` on the first write from a new serverID with no sweep at all (`accesslog.c:2113-2159`), and the `100 5` control stormed with every journal entry intact. So A is not a repair for something purging broke; it is the only option that closes the window at its source, and upstream's own ITS#9580 `FIXME` (`accesslog.c:2076-2082`) names this checkpoint as the intended cleanup. It also widens A's scope: the exposure is continuous from a new serverID's first write, not confined to post-sweep windows | Against upstream's guide, its fixtures, Symas, every distro; Chu's "only an optimization"; ITS#8607 shows checkpoints and accesslog have interacted badly before. Write amplification on a write-heavy DB |
| **B** | Raise `terminationGracePeriodSeconds` to ~90 s | Every slapd packaging rides systemd's 90 s; Zeilenga 2001 | 30 s never *shown* insufficient; no upstream figure for shutdown duration; slows worst-case rollouts; useless against OOM/node crash |
| **C** | `preStop` hook | k8s-native | No slapd precedent anywhere, not even Symas's own chart; slapd has no "flush now" command, so a hook can only sleep or poll |
| **D** | Extend operator CSN monitoring to the accesslog suffix | Kuzník verbatim; ahead of the entire ecosystem. **Implemented and validated** as `its9398.sh diagnose` (serverIDs in `minCSN` minus those in `contextCSN`), red→green against a deliberately armed fault | Sees the consequence, not the latent state — over LDAP it reads the in-memory view, which stays clean until the restart; needs the replication identity bind (ADR-020, verified sufficient) |
| **E** | On-disk preflight: `slapcat` vs `ldapsearch` on the log DB | The only check that sees exposure *before* a restart; ITS#10173's repair starts here | Folklore provenance; distroless image needs an init container or Job (ADR-018 reaping) |
| **F** | Operator repair: write from the absent serverID on a detected stall (**mode A**) | Symas documents exactly this; validated twice — the live incident (3 writes, 6968/min → 0, CPU recovered mesh-wide) and an armed fault (1 write) | The write must originate at the pod owning the missing serverID, usually **another site** → operator fan-out, which ADR-028 rejected under ADR-026 R2. Cannot help a serverID that no longer exists (scaled-down ordinal, decommissioned site). Would want a slaptain-owned entry rather than customer data |
| **I** | **Own-serverID regression watch + local heal (mode B)** — *added 2026-10-04* | Only pod P mints CSNs stamped P, so no peer may hold a newer value for P's own serverID than P does: a check needing **no disk read**, from data the existing cross-pod CSN monitoring already fetches. Cure is a **purely local** write on P — no fan-out, no ADR-026 R2 question. Zero cost in steady state. Validated: `err=53` ~30/min → 0 | Only covers mode B; mode A still needs F. Detects after the restart, not before. Reverses ADR-008's twice-deferred heartbeat stance — needs an ADR, not a quiet implementation |
| **G** | Wipe and rebuild the log DB from a converged peer | Symas's second rung; Quanah's ITS#10173 | Kuzník: transplant is safe *"if the server was shut down cleanly"* — the very precondition we'd be repairing. Scale-to-0 class outage |
| **H** | Document only | The ecosystem's revealed preference for six years | Their tolerance rests on rarity, which k8s falsifies; no-backoff retry makes it resource-exhausting rather than merely degraded |

**Cross-cutting, and the key constraint on any choice:** A and B/C are **not
substitutes**. B and C only cover stops that go *through* the pod lifecycle. OOM
kills, node crashes, and node shutdown on a default cluster bypass them
entirely. **Only A survives those.**

**Second constraint, from the 2026-10-04 experiments:** there are **two failure
modes**, and no single option covers both. Mode A (missing serverID, the storm)
can only be healed from another site; mode B (own-serverID regression,
`err=53`, cheap) can only be healed locally. D detects mode A, I detects mode
B. A is the only option that prevents either, because it is the only one that
makes `contextCSN` durable. See `ITS-9398-REPRODUCTION.md` for the measurements.

**Third constraint, 2026-10-23: A's cost is still unmeasured, and that is now
the gating unknown.** `1 1` is the only setting shown to survive a `SIGKILL`,
and it means a `contextCSN` write per logged operation into the environment
whose single LMDB writer already contends with every client write (ADR-037).
Why it works is an *ordering* argument read from source — the log entry is
added before `minCSN` is modified, so with `si_chkops = 1` the checkpoint fires
first (`syncprov.c:2607`, `:2615`) — and neither control attempts a kill inside
the window. Before A can be recommended, it needs a kill-inside-the-window
control and a cost measurement under load.

---

## Sources

Checked 2026-10-04 unless noted. Symas moved its KB: `kb.symas.com/<path>`
now 301s to `www.symas.com/kb/article.php?slug=<slug>`, and
`symas.helpjuice.com` (the backing SaaS) still serves the same pages. The
canonical `www.symas.com` form is used here.

**Upstream tickets** — all at `https://bugs.openldap.org/show_bug.cgi?id=<n>`:

| ITS | Title (verbatim) | Status | Why it is here |
|---|---|---|---|
| [9398](https://bugs.openldap.org/show_bug.cgi?id=9398) | Stale accesslog cookie due to unclean shutdown | UNCONFIRMED | **Our bug.** Filed 2020 |
| [9225](https://bugs.openldap.org/show_bug.cgi?id=9225) | back-mdb: Add support for PREPARE/2-phase commit | VERIFIED SUSPENDED | 9398's fix is blocked behind this |
| [9580](https://bugs.openldap.org/show_bug.cgi?id=9580) | Refresh vs. accesslog in delta-MPR | IN_PROGRESS | **Not our bug** — the misattribution, see the ADR-021 amendment |
| [9361](https://bugs.openldap.org/show_bug.cgi?id=9361) | accesslog logpurge generates new CSNs for its deletes | VERIFIED FIXED | Chu: logpurge made the log DB's contextCSN newer than the main DB's and sent NEW_COOKIE to all delta consumers — so a log DB's contextCSN demonstrably drives consumer behaviour |
| [10173](https://bugs.openldap.org/show_bug.cgi?id=10173) | Accesslog bootstrap doesn't populate minCSN internally | VERIFIED FIXED (2.5.18) | Comment #3 is the canonical log-DB repair, and the only published minCSN health check |
| [8607](https://bugs.openldap.org/show_bug.cgi?id=8607) | Second master in MMR config logging contextCSN changes to accesslog | VERIFIED FIXED | Fixed by *"slapo-syncprov not to log checkpoints to accesslog db"* — checkpoints and accesslog have interacted badly before |
| [8100](https://bugs.openldap.org/show_bug.cgi?id=8100) | Empty accesslog causes issues with delta-syncrepl MMR configurations | VERIFIED FIXED | Upstream's answer to log-DB recovery was entry-level persistence, not a checkpoint |
| [1227](https://www.openldap.org/lists/openldap-bugs/200107/msg00024.html) | BUG: Unclean shutdown on SIGTERM (Zeilenga, 2001, pre-Bugzilla) | — | The TERM→KILL window complaint, 25 years old |

Titles and statuses read off bugs.openldap.org on 2026-10-04. NB a 200 from
that host proves nothing — `?id=<nonsense>` also returns 200 — so these were
verified by extracting each page's title, not by status code.

**Maintainer statements**

- Chu, openldap-technical 2011-12, *"checkpointing the CSN is only an optimization"* — <https://www.openldap.net/lists/openldap-technical/201112/msg00273.html>
- Kuzník, openldap-technical 2023-02-28, *"should be monitored as such"* + the transplant caveat — <https://marc.info/?l=openldap-technical&m=167757941621578&w=2>
- Quanah, openldap-technical 2021-07-06, *"I generally advise … monitoring on the CSNs"* — <https://lists.openldap.org/hyperkitty/list/openldap-technical@openldap.org/message/SNGEVZTZXLKJF6R64VHD53PXH23UN6Z6/>

**Upstream documentation**

- Admin Guide 2.6, delta-syncrepl example (identical in 2.4/2.5/2.7) — <https://www.openldap.org/doc/admin26/replication.html>
- `slapo-syncprov(5)`, the shutdown-only persistence rule — <https://man7.org/linux/man-pages/man5/slapo-syncprov.5.html>
- `slapd(8)`, `-c rid=` full-reload — <https://man7.org/linux/man-pages/man8/slapd.8.html>
- `slapd-monitor(5)`, which mentions replication zero times — <https://man7.org/linux/man-pages/man5/slapd-monitor.5.html>
- `syncmonitor`, read at `f2debe3` — <https://git.openldap.org/openldap/syncmonitor>

**Vendor and distribution**

- Symas, Solving Common Problems (the recovery ladder) — <https://www.symas.com/kb/article.php?slug=solving-common-problems>
- Symas, Configure Delta-Syncrepl (*"Define only the syncprov overlay"*) — <https://www.symas.com/kb/article.php?slug=configure-delta-syncrepl>
- Symas, Using the Accesslog Database (no checkpoint, no contextCSN) — <https://www.symas.com/kb/article.php?slug=using-the-accesslog-database>
- Symas, Replication Primer (*"sync logs … the only way"*) — <https://www.symas.com/kb/article.php?slug=replication-primer>
- Symas Helm chart: no `terminationGracePeriodSeconds`, `lifecycleHooks: {}` — <https://github.com/symas/helm-openldap>
- Debian `slapd.service` (no `TimeoutStopSec`; new in trixie) — <https://sources.debian.org/src/openldap/2.6.14%2Bdfsg-2/debian/slapd.service/>
- Debian `slapd.init` (`--retry TERM/10`, never escalates) — <https://sources.debian.org/src/openldap/2.6.14%2Bdfsg-2/debian/slapd.init/>
- Fedora/RHEL `slapd.service` (also no stop settings) — <https://src.fedoraproject.org/rpms/openldap/raw/rawhide/f/slapd.service>

**Community**

- Poliakoff 2006, origin of the "one write" cure; the follow-up names the segfault — <https://www.openldap.org/lists/openldap-software/200605/msg00065.html> and `…/msg00068.html`
- 2023 *"contextCSN via slapcat outdated, despite having set syncprov-checkpoint"* — <https://lists.openldap.org/hyperkitty/list/openldap-technical@openldap.org/thread/FG7PBHP7EVL4QRPCQOIV4KIQ42V2QJA3/>
- Zytrax accesslog chapter (no syncprov directives at all) — <https://www.zytrax.com/books/ldap/ch6/accesslog.html>
- LTB `check_ldap_syncrepl_status` — <https://ltb-project.org/documentation/check_ldap_syncrepl_status.html>
- `check_syncrepl_extended` — <https://exchange.nagios.org/directory/plugins/network-protocols/ldap/check_syncrepl_extended/details/>

**Kubernetes**

- Graceful node shutdown is **off by default** (`shutdownGracePeriod: 0`) — <https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/>

**Deliberately absent.** The widely-repeated *"Symas gives slapd 30 s and
recommends 60 s+"* has no locatable primary source and is **not** cited above.
