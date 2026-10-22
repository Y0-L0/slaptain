# Changelog

Release notes for each tagged version. The publish workflow prepends the
section matching the tag to the generated release notes, so **a release with no
section here ships without one** — write it before you tag.

Headings are the git tag verbatim (`## v0.4.0`), with the `v`. Everything
downstream of the tag — images, chart `version`, chart `appVersion` — carries
the bare version; see `docs/VERSIONING.md`.

## v0.4.0

### Breaking: `accesslogPurge` becomes an object

`SlapdDatabase.spec.replication.accesslogPurge` was a string; it is now an
object, and **the purge schedule moves to the cluster**. This affects you even
if you never touch the new feature.

A *stored* string value fails the typed LIST for the whole result set, so the
`SlapdDatabase` controller stops reconciling **cluster-wide, in every
namespace** — not just for the database holding it. Clear the field on every
`SlapdDatabase` at every site **before** upgrading the CRDs; under the old CRD
the field is still typed `string`, so patching in the object form is rejected.

`docs/UPGRADING.md` has the exact order, the commands, and what to do if you
upgraded first.

### New, and EXPERIMENTAL: the accesslog purge rotation (ADR-037)

Off by default. The accesslog purge stalls writes on the provider running it;
this gives each pod its own purge *phase* on an absolute grid and moves a
`<cluster>-write` Service endpoint ahead of the grid by a declared `leadTime`,
so the pod taking writes is never the pod purging.

**This is a tier-2 surface (ADR-039): configurable, honestly reported, and not
yet asserted fit for production.** What is missing is operating experience, not
code. Three things make it unlike a conventional slapd deployment:

- **It needs a slapd carrying our `accesslog-purge-phase` patch.** The per-pod
  phase is a third value of `olcAccessLogPurge` that upstream OpenLDAP does not
  have. A stock build refuses it, the operator falls back to the two-value form
  and reports `AccesslogPurgeConverged=False/PurgePhaseUnsupported` naming the
  version you need — nothing breaks, but nothing rotates.
- **It couples your write endpoint to each provider's internal purge
  schedule.** Rotating the write target of a multi-provider cluster is ordinary
  OpenLDAP; doing it in step with the purge is not something conventional
  deployments do, and field experience is thin everywhere.
- **Its benefit is conditional on your clients.** A deployment whose write
  connections outlive `leadTime` measures *worse* with this on than off.

Known use cases are narrow: migrations, and bulk provisioning with short-lived
write clients. **Feedback wanted, positive or negative** — what you ran and what
you measured. ADR-037's seventeenth amendment lists the arms nobody has run.

User guide: `docs/PURGE-ROTATION.md`. Design record: ADR-037 (still `Proposed`,
deliberately).

### Note on the default slapd image

The default `slapd` image carries our `accesslog-purge-phase` patch on top of
OpenLDAP 2.7.1, built from the vendored packaging fork in
`images/openldap-deb/` and versioned `2.7.1-0+slaptain3` (`slapd -VV`). It is a
behaviour patch, not packaging, and `debian/changelog` records what it changes
and why — including one deliberate change to stock behaviour it could not avoid.

### Also in this release

- The ITS#9398 investigation: `docs/ITS-9398-REPRODUCTION.md` (the bug in pure
  slapd terms, for upstream) and `docs/ITS-9398-HANDLING.md` (how the ecosystem
  copes, and the mitigation options). ADR-021's stated reason for defaulting to
  OpenLDAP 2.7 is **retracted**: the storm is ITS#9398, open since 2020 and
  fixed in no release including master — not ITS#9580. The packaging and LMDB
  reasons for 2.7 stand on their own.
- ADR-039 (two tiers of support) and ADR-040 (data-plane image identity), both
  `Proposed`.
