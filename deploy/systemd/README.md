# pg-app Background Job Scheduling (Linux systemd Timers)

This directory contains the production `systemd` service and timer definitions for all 11 background jobs running on the self-managed PG-LIVE host.

## Job Schedule Overview

| Job Name | Frequency | IST Trigger Time | UTC Equivalent | Description |
|---|---|---|---|---|
| `pg-billing-cycle` | Daily | 00:05 IST | `18:35 UTC` (prev) | Monthly due generation, credit auto-application, overdue status |
| `pg-cashfree-poll` | Every 15 min | `*:00,15,30,45` | `*:00,15,30,45` | Fallback polling for unconfirmed/pending payment intents |
| `pg-daily-rollup` | Daily | 23:55 IST | `18:25 UTC` | Writes per-property daily collected/due/expense/occupancy rollups (current IST day) |
| `pg-financial-summary` | Daily | 23:45 IST | `18:15 UTC` | Generates daily owner financial digests and tie-outs |
| `pg-gamification-cycle` | Monthly | 1st @ 01:00 IST | `*-*-01 19:30 UTC` | Monthly gamification tier recalculation, streaks, badges |
| `pg-kpi-snapshot` | Daily | 02:00 IST | `20:30 UTC` (prev) | Captures daily occupancy, revenue, and collection KPIs |
| `pg-kyc-expiry` | Daily | 03:00 IST | `21:30 UTC` (prev) | Checks minor-to-major age transitions and expiring KYC records |
| `pg-reminder` | Daily | 09:00 IST | `03:30 UTC` | Dispatches rent reminders (D-3, D-0, D+1, D+7) with catch-up window |
| `pg-slot-invite-expiry` | Every 15 min | `*:05,20,35,50` | `*:05,20,35,50` | Marks open expired slot invites and releases reserved bed occupancy |
| `pg-waitlist-offer-expiry` | Every 15 min | `*:10,25,40,55` | `*:10,25,40,55` | Expires pending waitlist offers past deadline and advances to next waiter |
| `pg-join-pending-expiry` | Daily | 03:30 IST | `22:00 UTC` (prev) | Rejects stale pending join requests exceeding TTL |

## Scheduler of record (systemd vs GitHub Actions)

`billing-cycle` and `reminder` are *also* defined as GitHub Actions cron workflows
(`.github/workflows/billing.yml`, `reminder.yml`) because no server exists yet; Actions is the
live scheduler today. Once these systemd timers are installed on the server, set the repository
variable `SYSTEMD_SCHEDULER` to `true` (Settings -> Secrets and variables -> Actions -> Variables).
The workflows then skip their scheduled runs but remain runnable via `workflow_dispatch`.
Until the variable is set, behaviour is unchanged. Both jobs are idempotent (existing-due check,
unique reminder log) and take a Postgres advisory lock, so an accidental overlap is safe, just wasteful.

## Reliability Features

- **`Persistent=true`**: If the server was rebooted or offline during a scheduled trigger, systemd executes the job immediately upon reboot.
- **Grace / Catch-Up Window**: `pg-reminder` inspects the deduplication log and safely catches up on any missed reminders within a 2-day window without sending duplicates.
- **Service Hardening**: All jobs run under unprivileged user `pgapp` with strict filesystem sandboxing (`ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true`).

## Installation on Server

```bash
sudo bash /opt/pg-app/deploy/systemd/install.sh
```

To inspect active timers:
```bash
systemctl list-timers 'pg-*'
```

To run any job manually on-demand:
```bash
sudo systemctl start pg-reminder.service
journalctl -u pg-reminder.service -n 50 -f
```

## Worker Database Role (ADR-030)

All 9 background job binaries connect with `DATABASE_WORKER_URL`.
In production (`APP_ENV=production`), `DATABASE_WORKER_URL` is mandatory.
It must differ from both `DATABASE_URL` and `DATABASE_MAINT_URL`.
The startup guard `postgres.ValidateWorkerRole` enforces:
- The role name must be `pgapp_worker`.
- The role must not be a superuser (`NOSUPERUSER`).
- The role must not bypass row-level security (`NOBYPASSRLS`).
- The role must not own tables in the `public` schema.

Set `DATABASE_WORKER_URL` in `/opt/pg-app/.env`.

