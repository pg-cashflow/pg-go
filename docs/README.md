# pg-go documentation index

Read this file first. Use short paths below for daily work.

## Start here

1. Copy [`.env.example`](../.env.example) to `.env`.
2. Read [project README](../README.md) for quick start.
3. Read [ARCHITECTURE.md](ARCHITECTURE.md) for system map.
4. Read [CODEBASE.md](CODEBASE.md) for package layout.
5. Read [DEBUGGING.md](DEBUGGING.md) when something fails.

## Architecture and API

| Document | Purpose |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Modular monolith, invariants, flows, DB pools |
| [CODEBASE.md](CODEBASE.md) | Layers, naming, golden test spine |
| [CONTRACT.md](../CONTRACT.md) | HTTP API (Rev 8) |
| [modules/](modules/) | M1–M12 bounded contexts (journeys and gates) |
| [adr/README.md](adr/README.md) | Architecture decision records |
| [finance_roi.md](finance_roi.md) | Finance and ROI overview |

## Operations

| Document | Purpose |
| --- | --- |
| [runbooks/deploy_and_jobs.md](runbooks/deploy_and_jobs.md) | VPS deploy and job binaries |
| [runbooks/dispute_chargeback_runbook.md](runbooks/dispute_chargeback_runbook.md) | Gateway dispute response |

## Quality and production readiness

| Document | Purpose |
| --- | --- |
| [wayfinder/TEST_READINESS_SPECIFICATION.md](wayfinder/TEST_READINESS_SPECIFICATION.md) | Gates, ASD-STE100, Karpathy recipe |
| [wayfinder/map.md](wayfinder/map.md) | Money-handling readiness tracker (audit trail) |

Module deep dives use [modules/_TEMPLATE.md](modules/_TEMPLATE.md). `ARCHITECTURE.md` is the map; module files are the detail.
