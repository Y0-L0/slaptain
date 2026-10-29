# ADR-042: CI runs on Namespace runners

**Status:** Accepted
**Date:** 2026-10-29

## Context

CI compiles the e2e suite and never runs it.
The suite needs real Kubernetes clusters with routed pod networks between them
(resilience, S3 backup and restore, migration, a three-site replication mesh),
so it runs only in one maintainer's lab.
The image build compiles OpenLDAP three times in a row and takes 12 minutes on GitHub-hosted runners.

## Options considered

- **GitHub-hosted runners.** No clusters on demand, so the e2e suite stays in the lab.
- **Self-hosted runners in the lab.** This makes the lab part of CI, which is the dependency we want to remove.
- **Namespace runners.** GitHub Actions runners plus short-lived Kubernetes clusters on demand, with the same workflow syntax.

## Decision

Every job runs on `runs-on: namespace-profile-yo-lo`.
`.github/actionlint.yaml` declares the label, because actionlint rejects runner labels it does not know.

## Consequences

- A workflow change is one `runs-on` line per job, and moving back is the same edit.
- Running the e2e suite in CI becomes possible; doing it is separate work.
- The first fork run had an empty Go cache and says nothing about speed. A warm-cache run is the first that counts.
- Workflows with a `schedule:` trigger (CodeQL, security) start disabled in a fork and need `gh workflow enable`.
