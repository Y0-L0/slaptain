# ADR-039: Two tiers of support — validated, and configurable

**Status:** Accepted 2026-10-22 — after its first enforcement rule was tested
against the codebase and replaced; see "Why the first draft's rule was wrong".
**Date:** 2026-10-22

## Context

"Supported" is doing two jobs in this project and they have started to collide.

In the warranty sense nothing here is supported: slaptain is open source, offered
without guarantee, and that is stated where licences are stated. That sense is
settled and is not what this ADR is about.

The sense that matters is internal, and it is two questions wearing one word:

1. **Have we validated this for production use?** Measured, covered by e2e,
   sized in the documentation, with a migration path when it changes.
2. **Will the operator produce a functionally valid cluster if someone sets
   this?** Will it reconcile, converge, report honestly, and not corrupt
   anything — whether or not we think the setting is a good idea.

Conflating them produces a specific mistake: a knob whose benefit we have not
demonstrated gets *withheld* rather than *labelled*, and the project loses the
one thing it most needs from its users — somebody running the arm we did not
run. It happened in the design discussion that produced this ADR. ADR-037's
third amendment measured that staggering purge phases without moving the write
endpoint does nothing, and that measurement was read as grounds not to expose
the configuration at all. The measurement was sound and the inference was not:
the arms measured **writer throughput**, and the benefit claimed for that
configuration was **fleet read capacity** — a quantity ADR-037 itself reports
degrades to ~75 % during a purge, and which no arm isolated.

The project already drew this line once without naming it. **ADR-038 R2** keeps
`deltaSync: false` as "a migration-interop escape hatch, not a deployment
mode" — a configuration surface that is functionally valid, deliberately
reachable, and deliberately not presented as something to deploy on. That is
tier 2 in everything but vocabulary, and a second instance (ADR-037's
decomposition) is now arriving. Two instances with no shared language is how
the two drift into different rules.

A third instance is already queued. `slapd:<tag>-ol26` exists for ADR-011's
hot-migration lane, and there is live discussion about promoting OpenLDAP 2.6
from an interop lane to a deployment target. That is a tier change, and without
criteria it is a matter of opinion.

## Options considered

### Option 1: One tier — everything configurable is supported — rejected

Forces the project to remove any knob whose benefit it cannot demonstrate,
which is exactly the knob worth exposing to somebody with a different workload.
It also makes every measurement a gate rather than a finding: an arm that comes
back flat deletes a feature instead of annotating it.

### Option 2: One tier — ship only what is validated — rejected

Same defect from the other end, plus it is not how this project has ever
worked. `deltaSync: false`, the `-ol26` pair, `spec.backend.idlExponent` and
`podAffinity: false` are all live surfaces that exist for a stated non-default
purpose. Retiring them would cost real capability to buy a tidier claim.

### Option 3: Two tiers, separated by what is default and what evidence is attached — chosen

Keeps both capabilities and makes the distinction legible at the point where a
user makes the choice, rather than in a document they have not read. An earlier
form of this option separated the tiers by *reachability* — tier 2 absent from
the charts — and was discarded after testing it against the codebase; see "Why
the first draft's rule was wrong".

## Decision

**Two tiers. A surface is in exactly one of them at any time, and which one is
a decision, recorded like any other.**

### Tier 1 — Validated

What the project asserts is fit for production use.

- Measured, with the measurement recorded and its conditions stated.
- Covered by e2e, and by a multi-site cycle when it touches the replication
  path (CLAUDE.md's existing rule).
- Documented with a sizing or operating path, not merely an API description.
- Reachable through the Helm charts.
- A change to it gets an `UPGRADING.md` entry.

### Tier 2 — Configurable

What the operator will faithfully do if asked, without the project asserting it
is a good idea.

- **Functionally valid**: it reconciles, converges, and leaves a cluster that
  works. Tier 2 is not permission to produce a broken cluster.
- **Honestly reported**: the same conditions, the same status, the same refusal
  messages. A research vehicle whose state cannot be read is not one.
- **Off by default, and labelled wherever it is offered.** A chart MAY expose
  a tier-2 surface — see "Why the first draft's rule was wrong" below — but its
  values key defaults to empty or false, and the comment block above it names
  the tier. The property that matters is that **nobody reaches it by accident**,
  which off-by-default delivers; absence would deliver "nobody reaches it at
  all", which defeats the point of having the tier.
- **Reachable without cloning the repository.** A research vehicle nobody can
  find is not one. Tier 2 is discoverable and deliberately inconvenient — a
  commented-out block behind a label — not hidden.
- **Labelled where it is configured**: the CRD field documentation says the
  tier, because `kubectl explain` is the surface a user actually reads.
- **The evidence travels with the knob.** Whatever is known — measured inert
  for X, unmeasured for Y, reasoned but never run — is stated in the field
  documentation. A tier-2 knob with no attached evidence is just an
  undocumented one, and this is the clause that distinguishes the two.

### A tier attaches to a surface, not to a field

Some fields are tier 1 while particular *values* of them are not. `image.tag`
is the case that forced this: the field is ordinary and supported, and it is
also how a user reaches the `-ol26` pair, which is an interop lane rather than
a deployment target (ADR-021, ADR-040). The field does not change tier because
one of its values is lower-tier.

So the unit of classification is the **thing being offered** — a feature, an
image, a mode — and the tier is stated wherever that thing is named: the image
in its own documentation, the feature in its field. A field that merely carries
a string is not where a tier lives.

### Why the first draft's rule was wrong

This ADR first said tier 2 meant **absent from the charts**, enforced by grep.
That rule was tested against the codebase before the ADR was accepted and it
failed twice, which is recorded here because the failure is the argument for
the rule that replaced it.

It failed against the feature it was written alongside. `spec.rotation` is
tier 2 by ADR-037, and v0.4.0 shipped its CRD field documentation saying so —
while both `charts/slapd` and `charts/slapd-mesh` template it, document its
prerequisites at length, and ship `examples/rotation.yaml` to turn it on. The
rule would have condemned the arrangement the feature actually needs: the
e2e's own `E2E_ROTATION=1` path stacks that example, so "absent from the
charts" would have removed the only mechanism by which the feature gets tested.

And it could not express the `image.tag` case above at all, because it
partitioned fields when the thing with a tier is not always a field.

The replacement keeps what the first rule was reaching for — a tier-2 surface
must not arrive by accident — and drops the part that confused *inaccessible*
with *not-default*.

### What tier 2 is not

It is not a staging area for unfinished work. Code that does not yet work is
not tier 2, it is unmerged. Tier 2 is for surfaces that **work and are
unproven**, which is a different thing and the only thing the tier is for.

### Promotion and demotion

Promotion to tier 1 requires what tier 1 lists — chiefly a measurement, which
may come from outside the project. Demotion requires evidence that the surface
does not deliver what tier 1 asserted, and is recorded in the ADR that made the
assertion. Neither happens silently: the charts change, which is visible.

### The rule that would have prevented the mistake

**A refutation names what it tested.** "Measured and does nothing" is not a
property of a configuration, it is a property of a configuration *against a
quantity under a load*. When a measurement is used to argue a surface should
not exist, the argument must state the quantity measured and the quantity
claimed, and show they are the same one.

## Consequences

- **The tier is still mechanically checkable, but by a convention rather than
  by absence**: every tier-2 values key defaults to empty or false, and its
  comment block names the tier. That is weaker than a grep for absence and it
  is worth saying so plainly — a mislabelled key is a documentation bug that no
  test catches, where an absent one could not be set at all. The trade bought
  discoverability, which the tier exists to provide.
- **The mesh story is unchanged and slightly improved.** A mesh CR is
  byte-identical at every site (ADR-028); a tier-2 knob set by hand at one site
  shows up as divergence, which is the correct outcome.
- **e2e obligations differ by tier.** Tier 1 gets behavioural coverage. Tier 2
  gets enough to show it produces a valid cluster — the claim the tier actually
  makes — and no more.
- **ADR-038 R2 is relabelled, not changed.** `deltaSync: false` was already
  tier 2; it now has the word for it.
- **ADR-037's rotation decomposition becomes expressible**: purge staggering
  and write-endpoint rotation can be settable independently as tier-2 surfaces,
  each carrying its own evidence, while the charts offer only the coupled form
  that was measured end to end.
- **The 2.6 question becomes answerable on criteria** rather than on taste: it
  is a promotion from interop lane to deployment target, and the list above is
  what it costs — chiefly an e2e matrix, which is a real and recurring bill
  (ADR-028's one-path rule is the counter-argument, and deleting the hand-wired
  e2e path has already cost coverage that is still missing).
- **A cost worth naming**: two tiers is more vocabulary, and vocabulary decays
  if unused. The mitigation is that tier 2 has a mechanical definition (not in
  the charts), so a drifting label is detectable rather than merely wrong.

## Related

- ADR-038 — R2 is the first instance of this distinction, drawn before it had a
  name.
- ADR-037 — the second instance; its third amendment is the measurement whose
  over-reading prompted this ADR.
- ADR-035 — ADRs conflict and a collision is itself a decision. This ADR is
  upstream of several and is expected to be cited in those collisions.
- ADR-028 — one path, not two. The standing argument against tier promotion,
  and the reason promotion has a cost rather than being free.
- ADR-021 — the `-ol26` pair, the tier-change candidate.
- ADR-024 — unset means slaptain's default; a default change is a change to
  every cluster that said nothing, which is a tier-1 obligation.
