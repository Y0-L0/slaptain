# Release plan: 0.4.0 and the three releases after it

Written 2026-10-22, at the close of the session that planned it. Nothing here
has been executed. It exists so the next session can confirm, re-litigate or
run it without re-deriving the reasoning — every decision below has an ADR or
a measurement behind it, and the ones that are still open say so.

## Where the tree stands

- `feat/accesslog-purge-phase` — 69 commits ahead of `origin/main`, clean,
  implementation complete and measured. This is 0.4.0.
- `docs/its9398-research` — **MERGED into local `main` 2026-10-22** (`c696fcc`),
  ahead of 0.4.0 rather than in the 0.5.0 slot this plan first gave it: the
  conflict surface against `main` turned out to be two append-at-EOF doc hunks,
  not the three-file tangle expected against the 0.4.0 branch. ADR-021's
  retraction is therefore on `main` now, so `main` no longer states a reason
  for its own default that we know to be wrong. Two notes from the merge: the
  branch's history was **rewritten** first (it carried lab cluster context
  names predating the blacklist entry — `backup/its9398-prerewrite` holds the
  pre-rewrite tip), and putting the documents side by side exposed a
  disagreement about the purge's role in the storm, recorded in `BACKLOG.md`
  rather than resolved.
- `feat/rid-allocation-ledger` — 28 commits, unmerged, 152 behind.
  **Rejected 2026-10-22, will not be merged** (see 0.5.0). Kept as a record.
- `backup/adr-034-prerebase`, `feat/adr-034-startup-only-config`,
  `fix/own-sid-watermark-gap` — all merged into `origin/main` by patch-id;
  safe to delete.
- 20 `worktree-agent-*` branches, 9 with uncommitted leftovers from the purge
  work. `git worktree prune` once 0.4.0 is out.

## 0.4.0 — the purge rotation

Breaking for everyone, including non-adopters: `accesslogPurge` becomes an
object, and a **stored** string fails the typed LIST for the whole result set,
stopping `SlapdDatabase` reconciliation cluster-wide in every namespace. Not a
patch release under any reading. `docs/UPGRADING.md` already carries the
migration under a heading that still says `## Unreleased`.

**Status 2026-10-22: items 1-7 are done; only the tag remains.** The 0.4.0
branch is merged into local `main` and nothing is pushed.

**The multi-site validation, in full.** Three cycles on a three-site mesh,
pod-routed, images at `sha-0582604`.

| cycle | result |
|---|---|
| rotation OFF | 107 passed / 2 failed / 7 skipped |
| rotation ON (`E2E_ROTATION=1`) | 101 passed / 3 failed / 12 skipped |
| `write-rotation` only, after the fixture fix | **6 passed / 0 failed** |

**No product regression in any of them.** The reds, each run to ground:

- *hot-add* and *exporter sidecar* (cycle 1) — collateral. `external_replication`
  removes site-3 from the `SlapdMesh` and restores it, and because peer CA
  volumes live in the pod template that rolls every pod twice; the hot-add spec
  snapshotted mid-roll. The spec predates this branch by a month.
- *dataloss recovery* (cycle 2) — the ITS#9398 storm, in the configuration we
  knowingly ship. See the known-issue entry in `CHANGELOG.md`.
- *exporter sidecar* (cycle 2) — collateral of that storm: ADR-030's probe
  correctly ejected a pod that was no longer answering LDAP. The probe working,
  not failing.
- *write-rotation BeforeAll* (cycle 2) — **the one genuine defect, and it was in
  the test.** Fixed in `129b774`; see cycle 3.

**One delta between the validated tree and the tag.** Three dependabot patch
bumps landed on `origin/main` while the validation ran — aws-sdk-go-v2
1.47.2→1.47.3 (s3 1.114.2→1.114.3) and ginkgo 2.33.0→2.33.1 in both modules —
and were merged in before tagging. They were re-checked by build, vet, the unit
suites (`internal/backup` included, which is the SDK's only consumer),
`helm-lint` and the generated-files gate. They were **not** put through another
mesh cycle; patch releases of an S3 client and a test framework did not seem
worth three more cycles, and that judgement is recorded here rather than left
implicit.

**Two things the validation taught that are not about this release.** The suite
had never been run against a cluster *deployed* with the rotation on, which is
how the fixture gap survived. And a mesh membership change rolls every pod at
every other site — a pod-template roll trigger ADR-013 does not list.

Pre-tag checklist:

1. **Finalise `images/openldap-deb/debian/changelog`** — the top entry is
   `2.7.1-0+slaptain3 UNRELEASED`. Rebuild and confirm `slapd -VV`, because
   that exact string is what `AccesslogPurgeConverged=False/PurgePhaseUnsupported`
   tells operators to check.
2. `docs/UPGRADING.md`: `## Unreleased` → `## v0.4.0`.
3. **Experimental framing** into the `SlapdRotationConfig` field documentation
   and the head of `docs/PURGE-ROTATION.md`, per ADR-039 tier 2. Regenerate
   CRDs (`make operator-manifests`) so it reaches `kubectl explain`.
4. **Release notes.** `publish.yml` builds a hardcoded body plus GitHub's
   `generate_release_notes`; there is no `CHANGELOG.md`. With ~69 narrative
   commits the generated half is unreadable, and nothing currently tells a
   reader that this release carries a breaking change and an experimental
   feature. Add a per-release section to the body: the break and its migration
   pointer, the experimental feature, the non-upstream patch disclosure, and a
   call for feedback naming the arms nobody has run.
5. **ADR-037 stays `Proposed`** — deliberately, and worth a line saying so, since
   four other ADRs are stale in the other direction (004, 005, 006, 007 are all
   implemented and still say Proposed).
6. **Decide what to say about the purge and the storm.** The `CLAUDE.md`
   claim has been narrowed from "the purge has nothing to do with it" to what
   was actually measured, and the open question — does a purge sweep that
   deletes nothing still write `minCSN`? — is in `BACKLOG.md`. Its first half
   is answerable from `accesslog.c:656-684` for the price of one read, and it
   decides whether ADR-037's slot length is also a storm-exposure knob. Cheap
   enough to settle before the release; if it is not settled, the release notes
   should not imply it is.
7. ~~`make helm-lint verify-generated`, then a **multi-site e2e cycle**~~ —
   done 2026-10-22, see the status block above. Run the rotation cycle with
   `E2E_ROTATION=1` or the feature's own specs never execute.
8. Chart `version`/`appVersion` → `0.4.0`, bump commit, then tag.

Deliberately **not** in 0.4.0: any default change, the image scheme, the
rotation decomposition. The release already carries one forced migration.

### How a release lands on `main`

**By rebase, never by merge.** The `protect-main-branch` ruleset rejects a push
carrying a merge commit, and it is a *ruleset* — the classic
`branches/main/protection` API reports `404 Branch not protected`, which is
misleading. v0.4.0 was assembled with three local merges, all of which had to be
linearized at push time.

The order that works:

```bash
git fetch origin                      # the remote moves; dependabot lands often
git rebase origin/main                # linearize; expect conflicts where a
                                      #   long-lived branch meets a moved main
git diff <the commit you validated> HEAD   # MUST be empty — see below
```

**The trap, which cost an afternoon.** Rebase drops merge commits, and with them
any content that existed *only* in a merge resolution. Three edits were lost
that way on v0.4.0 — a narrowed claim in the storm spec, an amendment ordering,
a stray heading — and the rebased branch still built and still passed `go vet`.
The only thing that caught it was diffing the rebased tree against the commit
the mesh validation had actually run against. Make that diff a gate, not a
courtesy, and if it is non-empty restore the difference as its own commit rather
than hand-editing history again.

Related: sanitising a branch for the blacklist means **trees and commit
messages**. The first rewrite on v0.4.0 fixed file contents only, and `pre-push`
— which scans every outgoing commit's message too — caught the rest on the
final check.

### Publishing: the workflow works since 2026-10-25; the hand path stays written down

**Resolved for the images, 2026-10-25.** The ghcr image packages were entitled
by hand (the repository added under *Manage Actions access*), and the failed
`publish-main` run for `70dd083` was re-run: attempt 2 succeeded, and the
registry confirms it by a second route — `sha-70dd083` is listed on every image
package, with the `-ol26` and `-ol26src` variants on the slapd pair, so all nine
images landed and not just the first. That is the first publish ever made by
CI in this repository: all twelve `publish-main` runs before it had failed, and
so had the one `publish.yml` run (v0.4.0).

**The charts, entitled 2026-10-26.** `publish.yml` also runs `make
charts-push`, into four packages of their own — `charts/slaptain`,
`charts/slapd`, `charts/slapd-mesh`, `charts/slapd-toolkit` — and every version
on them, 0.4.0 included, was pushed by hand, so they were created by a personal
token exactly as the image packages were; `publish-main` never touches them, so
the successful run above said nothing about them. They are not under the
`slaptain/` prefix, which is why the repository's Packages page did not list
them. On 2026-10-26 the repository was added to all four (and connected to
them: the repository's Packages page now lists nine packages, charts
included). That entitlement is a settings-page fact and has no run behind it
yet — 0.5.0 is the first tag that can show `publish.yml` reaching the end: the
chart push, the GitHub Release, the `slctl` binaries and the CRD bundle have
never been produced by CI.

Nothing below this line should be deleted before the 0.5.0 tag has run
`publish.yml` to the end. The analysis is the record of why, and the four traps
bind any hand push just the same (a lab registry, a hotfix, a rebuild from a
tag).

`.github/workflows/publish.yml` triggers on `push: tags: v*` and looks complete.
Until 2026-10-25 it was not. It was added on 2026-10-11 (`Ci/GitHub actions (#3)`) and **v0.4.0 was
its first execution ever** — the repository has eight version tags and, before
v0.4.0, zero GitHub Releases. Every release so far was hand-pushed.

It failed at the first `podman push`:

```
denied: permission_denied: write_package
  → ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-init
```

All nine images had **built** by then, so this is not a build problem. The ghcr
packages were created by a personal token, and a package created that way does
not grant the repository's Actions token write access: each package needs the
repo added under *Package settings → Manage Actions access* with the **Write**
role (or the equivalent "inherit access from repository"). `permissions:
packages: write` in the workflow is necessary and not sufficient — it scopes the
token, it does not entitle it to a package it does not own.

Until 2026-10-25 that meant publishing from a workstation. The four traps, in
the order they bite, hold for any push made from one:

1. **`REGISTRY` may already be set in your environment**, in which case the
   Makefile's `REGISTRY ?= ghcr.io/chuck-chuck-chuck-net` never applies — `?=`
   yields to it. A plain `make push` then publishes the *release* somewhere
   else, reports success at every step, and leaves ghcr empty. **Pass
   `REGISTRY=` explicitly on the command line**, where it beats both.
2. **Build from a detached checkout at the tag.** The version is derived from
   `git describe`; even one commit past the tag, `make show-tag` yields image
   `sha-<hash>` and chart `0.0.0-<hash>`, and those are what gets published.
3. **Clear `.stamps` when switching registry.** The stamps are not keyed by
   `REGISTRY`, so a lab build makes `make push` skip the rebuild and then fail
   with "image not known" — or, worse, push something stale.
4. **The exporter has its own version line** (ADR-033). Off an `exporter/vX.Y.Z`
   tag it publishes as `sha-<hash>`, which is correct and will look wrong next
   to nine `0.4.0` images.

The sequence that produced v0.4.0:

```bash
git checkout v0.4.0                       # detached; 1 and 2 above
rm -rf .stamps                            # 3
make show-tag REGISTRY=ghcr.io/chuck-chuck-chuck-net   # expect v0.4.0 / 0.4.0 / 0.4.0
env -u REGISTRY make push          REGISTRY=ghcr.io/chuck-chuck-chuck-net
env -u REGISTRY make charts-package charts-push REGISTRY=ghcr.io/chuck-chuck-chuck-net

# the release the workflow would have made: notes are the CHANGELOG section,
# extracted exactly as publish.yml extracts them
awk -v t="## v0.4.0" '$0==t{f=1;next} f&&/^## /{exit} f{print}' CHANGELOG.md > notes.md
#   ... then append the images / charts / CRD-first / slctl boilerplate
cat charts/operator/crds/*.yaml > dist/slaptain-crds.yaml
cd operator && for os in linux darwin; do for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w'     -o ../dist/slctl-$os-$arch ./cmd/slctl; done; done
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w'   -o ../dist/slctl-windows-amd64.exe ./cmd/slctl
gh release create v0.4.0 --title v0.4.0 --notes-file notes.md --verify-tag dist/*
```

Fixing the workflow was a packages-permission change plus one real run, and
the build was not touched — images 2026-10-25, charts 2026-10-26, see the top
of this section. 0.5.0 is the first tag that can show `publish.yml` running to
the end.

### Git history

Keep it. In this repository commit messages are the reasoning record and
`git log` is the only index for them; squashing narrative commits destroys
more than it tidies. Fold only mechanical corrections of an immediately
preceding commit's own text — `08d696d` (1 insertion, 1 deletion) and
`a2d7297` are the two visible candidates. Correction *chains* such as
`5743aba` → `3d0d2b2` stay separate: those are two distinct findings and the
second is the more valuable record. Realistic saving is 69 → ~65, which is not
worth much churn. The higher-leverage move is item 4.

## 0.5.0 — the keystone ADRs

1. ~~**Merge `docs/its9398-research`.**~~ Done 2026-10-22, before 0.4.0 — see
   "Where the tree stands". The 0.4.0 branch absorbs it on its way to `main`,
   and the one conflict to expect there is `dataloss_recovery_test.go`.
2. **ADR-039 (two tiers of support)** — drafted, `Proposed`. Blocking three
   separate decisions, which is why it comes first.
3. **ADR-040 (data-plane image identity)** — drafted, `Proposed`. Tag scheme,
   the pure/patched alias inversion, OCI labels, born-with pinning, the volume
   preflight, capability probing over version strings.
4. **The ADR-037 rotation decomposition** (seventeenth amendment) as tier-2
   surfaces, with the evidence attached to each.
5. Image work: move `accesslog-purge-phase.patch` into `local-patches/`, build
   the pure 2.7.1 pair, derive and sanitise the OpenLDAP tag component, invert
   the alias. Breaking for anyone pinning `spec.images.*.tag` — own
   `UPGRADING.md` entry.

**Not in this release, or any: `feat/rid-allocation-ledger` is rejected**
(2026-10-22). The approach will not be merged. Its 28 commits stay on the
branch as a record of what was tried; `spec.replication.ridBase` therefore
stays, and ADR-031 Phase 6 does not happen as written. If the RID ledger is
ever revisited it starts from the ADR, not from that branch.

## 0.6.0 — born-with pinning and the preflight

ADR-040 decisions 4 and 5, implemented. This is the prerequisite for **any**
future data-plane default move, in either direction, including 2.7 → 2.8
whenever LMDB next breaks format. Without it, moving the default hands every
cluster that said nothing an image that cannot open its own volume, loudly and
fleet-wide, on operator upgrade.

## 0.7.0 and after — default flips, one per release

ADR-024 R5: one customer per change. Each gets its own `UPGRADING.md` entry,
because the point of spreading them out is that each migration is separately
legible.

- **OpenLDAP 2.6 as the default.** The case, assembled this session: ADR-021's
  headline justification is retracted (on the unmerged branch); 2.7.1 carries
  ITS#10597, a shared `entryUUID` freed from an unlocked path on every search
  completing inside a write, fixed only in master, where 2.6 has no counterpart;
  Debian ships 2.6, which is the mainstream, distro-supported code; the purge
  patch exists for 2.6 too; and ADR-021's own escape clause — no operator
  feature is 2.7-only — still holds. **Against**: adopting 2.7 now buys
  in-place upgradeability until LMDB next breaks format, which is an unknown
  and uncontrolled clock. With born-with pinning the dilemma mostly dissolves —
  the choice moves to cluster birth and is recorded — so this becomes a
  question of which default serves a first-time user better, not which version
  the project bets on. Requires an ADR-021 supersession and, per ADR-039, a
  decision about whether 2.6 is promoted from interop lane to deployment
  target, whose real cost is the e2e matrix.
- **Accesslog checkpoint** as a default (ADR-024 R4/R5, converged-per-pod,
  cheapest of the three).
- **`syncprovSessionlogSource: Accesslog`** as a default. Mechanically sound —
  ADR-034's handover is one offline `slapmodify` decided from live state, never
  a dropped session log, and the "wasteful operation" warning measured absent
  across a flip-and-return. Two things to respect: it lands at each pod's
  **next restart** rather than on operator upgrade, so the fleet changes
  gradually; and ADR-034's amendment records that a dangling value is *latent*,
  because `syncprov_db_open` reaches its backend check only on a non-empty
  database — so the first cluster to discover a mistake is a populated one.

## Standing hazards this plan does not resolve

- **ITS#9398** is red on 2.7.1 in `dataloss_recovery_test.go` and reproduced
  twice more on a three-site mesh. The purge was established as irrelevant to
  it (2026-10-19). It is in the baseline recovery path and nothing here fixes
  it.
- **Mixed-version mesh behaviour** is reasoned, not measured (ADR-027). Any
  move that makes 2.6 and 2.7 both first-class makes this a question somebody
  will actually ask.
- **ADR-036** is `Proposed` with nothing built, and is a prerequisite for the
  backup-accelerated pod rebuild recorded in `docs/BACKLOG.md`.
