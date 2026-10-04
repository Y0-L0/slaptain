# ITS#9398: how the ecosystem handles it, and what we could do

Decision input, gathered 2026-10-04. The bug itself is in
[`ITS-9398-REPRODUCTION.md`](ITS-9398-REPRODUCTION.md); this file is only about
*coping with it*.

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

**One gap, stated rather than papered over:** the OpenLDAP Project's own
[`syncmonitor`](https://git.openldap.org/openldap/syncmonitor) is the tool most
likely to be an exception, and **we have not read its source** — the GitHub
mirror 404s and the canonical repo is on git.openldap.org. What we actually
checked is Symas's page describing it, which does not mention the accesslog.
That is weaker evidence than the rest of this row. Read the source before
repeating the claim.

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

**Vendor**, Symas "Solving Common Problems", verbatim:

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
| **A** | `olcSpCheckpoint` on every `cn=accesslog-<db>` | ITS#9398 names the missing checkpoint as the cause; our measurement (`1 1` clean, `100 5` storm) | Against upstream's guide, its fixtures, Symas, every distro; Chu's "only an optimization"; ITS#8607 shows checkpoints and accesslog have interacted badly before. Write amplification on a write-heavy DB |
| **B** | Raise `terminationGracePeriodSeconds` to ~90 s | Every slapd packaging rides systemd's 90 s; Zeilenga 2001 | 30 s never *shown* insufficient; no upstream figure for shutdown duration; slows worst-case rollouts; useless against OOM/node crash |
| **C** | `preStop` hook | k8s-native | No slapd precedent anywhere, not even Symas's own chart; slapd has no "flush now" command, so a hook can only sleep or poll |
| **D** | Extend operator CSN monitoring to the accesslog suffix | Kuzník verbatim; ahead of the entire ecosystem | Sees the consequence, not the latent state; needs the replication identity bind (ADR-020) |
| **E** | On-disk preflight: `slapcat` vs `ldapsearch` on the log DB | The only check that sees exposure *before* a restart; ITS#10173's repair starts here | Folklore provenance; distroless image needs an init container or Job (ADR-018 reaping) |
| **F** | Operator repair: write from the absent serverID on a detected stall | Symas documents exactly this; our measurement shows instant clearance | ADR-026 R2 if it writes customer data — would need a slaptain-owned entry; repairs symptom, recurs every unclean stop |
| **G** | Wipe and rebuild the log DB from a converged peer | Symas's second rung; Quanah's ITS#10173 | Kuzník: transplant is safe *"if the server was shut down cleanly"* — the very precondition we'd be repairing. Scale-to-0 class outage |
| **H** | Document only | The ecosystem's revealed preference for six years | Their tolerance rests on rarity, which k8s falsifies; no-backoff retry makes it resource-exhausting rather than merely degraded |

**Cross-cutting, and the key constraint on any choice:** A and B/C are **not
substitutes**. B and C only cover stops that go *through* the pod lifecycle. OOM
kills, node crashes, and node shutdown on a default cluster bypass them
entirely. **Only A survives those.**
