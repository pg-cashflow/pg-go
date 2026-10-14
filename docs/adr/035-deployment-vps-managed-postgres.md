# ADR-035: Production Deployment (VPS + Managed PostgreSQL)

**Status:** Accepted  
**Date:** 2026-10-14  
**Deciders:** Divakar  
**Builds on:** ADR-030 (job DB role), [`deploy/systemd/`](../../deploy/systemd/)

## Context

The repo ships a modular monolith (`cmd/server`) and nine scheduled job binaries with systemd timers. There is no container runtime in the tree. The ledger database must support backup and point-in-time recovery. A solo builder runs 20+ hours per week on this program.

## Decision

1. **PostgreSQL:** managed service (Neon or RDS) with PITR enabled for production and staging branches.
2. **Compute:** one small Linux VPS (Ubuntu) runs `cmd/server` and systemd timers from `deploy/systemd/`.
3. **Secrets:** three connection URLs in production — `DATABASE_URL` (`pgapp_app`), `DATABASE_MAINT_URL` (`pgapp_maint`), `DATABASE_WORKER_URL` (`pgapp_worker`) per ADR-030.

Cloud Run and Cloud Scheduler are **out of scope** until traffic or team size forces a split.

## Consequences

- **Easier:** matches existing deploy artifacts; Track Q staging unblocks after VPS + staging DB exist.
- **Harder:** horizontal scale requires revisiting this ADR; jobs and API share one host initially.
- **Development:** local `cmd/server` against Neon is valid before VPS exists; production timers require the VPS.

## Action items

1. Provision VPS and install systemd units (`deploy/systemd/install.sh`).
2. Keep [`docs/runbooks/deploy_and_jobs.md`](../runbooks/deploy_and_jobs.md) as the operator runbook (failover drill references TEST_READINESS_SPEC RPO/RTO).
