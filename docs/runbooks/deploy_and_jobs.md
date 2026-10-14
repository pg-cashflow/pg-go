# Deployment and Background Jobs Runbook

**Date:** 2026-10-14  
**Specification:** Ticket R7, Track R (`track_r_m12_closeout.md`)  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Status:** Ready for initial deployment.

> [!IMPORTANT]
> **Verification Status:**  
> No server exists today. All units, configurations, and commands in this runbook are validated against the codebase.
> Live execution against an active host is **unverified**.

---

## 1. Overview

This runbook defines the deployment process for `pg-app` on one target: a **Linux VPS with systemd** (self-managed virtual private server). No container image is used. Other deployment targets are not documented.

The VPS runs this Go binary architecture:

- Single monolith HTTP server (`cmd/server`) serving Gin REST endpoints and embedded PWA assets.
- Nine scheduled background job binaries (`cmd/*`).
- Standalone migration runner (`cmd/migrate`) executed during deployment.

---

## 2. Environment Variables Matrix

The table below lists all variables supported by `internal/config/validate.go` and `.env.example`.

| Variable Name                     | Required in Production       | Default Value        | Security Invariant / Purpose                                                    |
| --------------------------------- | ---------------------------- | -------------------- | ------------------------------------------------------------------------------- |
| `APP_ENV`                         | **Yes**                      | `development`        | Set to `production` to activate production guards.                              |
| `DATABASE_URL`                    | **Yes**                      | None                 | Application pool connection string (`pgapp_app` role).                          |
| `DATABASE_URL_UNPOOLED`           | **Yes**                      | None                 | Direct connection for `cmd/migrate` (no PgBouncer).                             |
| `DATABASE_MAINT_URL`              | **Yes**                      | `DATABASE_URL`       | Maintenance pool (`pgapp_maint`). Must differ from `DATABASE_URL`.              |
| `DATABASE_WORKER_URL`             | **Yes**                      | `DATABASE_MAINT_URL` | Background job pool (`pgapp_worker`). Must differ from both pools.              |
| `JWT_SECRET`                      | **Yes**                      | None                 | Session token signing. Minimum 32 characters, no placeholder.                   |
| `OTP_HMAC_SECRET`                 | **Yes**                      | None                 | Phone OTP HMAC signing. Minimum 32 characters, distinct from JWT.               |
| `MAGIC_LINK_HMAC_SECRET`          | **Yes**                      | None                 | Payment link signing. Minimum 32 characters, distinct from JWT.                 |
| `MAGIC_LINK_BASE_URL`             | **Yes**                      | None                 | Base URL for tenant payment links (e.g. `https://pay.example.com`).             |
| `ADMIN_EMAIL`                     | **Yes**                      | None                 | Destination for critical dead-letter and outbox failure alerts.                 |
| `PAYOUT_ENCRYPTION_SECRET`        | **Yes** (if payouts enabled) | None                 | AES-GCM bank detail encryption. Minimum 32 chars.                               |
| `PAYOUT_CHECKSUM_SECRET`          | **Yes** (if payouts enabled) | None                 | HMAC checksum for CSV exports. Minimum 32 chars.                                |
| `CF_PAYOUT_CLIENT_ID`             | Conditional                  | None                 | Cashfree Payout client credential.                                              |
| `CF_PAYOUT_CLIENT_SECRET`         | Conditional                  | None                 | Cashfree Payout secret credential.                                              |
| `CF_PAYOUT_FUNDSOURCE_ID`         | Conditional                  | None                 | Cashfree debit account identifier.                                              |
| `CF_PAYOUT_WEBHOOK_SECRET`        | Conditional                  | None                 | Cashfree payout webhook signature key (exempt from 32 chars).                   |
| `CASHFREE_PG_APP_ID`              | Conditional                  | None                 | Payment gateway application identifier.                                         |
| `CASHFREE_PG_SECRET_KEY`          | Conditional                  | None                 | Payment gateway secret key.                                                     |
| `CASHFREE_ENV`                    | Conditional                  | `sandbox`            | Set to `production` for live payment settlement.                                |
| `CASHFREE_PG_WEBHOOK_SECRET`      | No                           | None                 | Webhook override. Falls back to `CASHFREE_PG_SECRET_KEY` (ADR-031).             |
| `KYC_IDENTITY_SECRET`             | Conditional                  | None                 | Aadhaar identity hashing secret. Minimum 32 characters.                         |
| `KYC_DIGILOCKER_REDIRECT_URL`     | Conditional                  | None                 | OAuth callback URL for DigiLocker flow.                                         |
| `METRICS_SECRET`                  | **Yes** in production        | None                 | `X-Metrics-Token` for `/metrics`. Minimum 16 chars. `METRICS_TOKEN` is ignored. |
| `HTTP_ADDR`                       | No                           | `:8080`              | Port and interface binding for the HTTP server.                                 |
| `DATABASE_STATEMENT_TIMEOUT`      | No                           | `10m`                | Database connection statement timeout.                                          |
| `WEBHOOK_TIMESTAMP_TOLERANCE_SEC` | No                           | `300`                | Gateway webhook replay protection window (seconds).                             |

---

## 3. Database Role Creation

Run these SQL statements once on the PostgreSQL cluster before deploying binaries:

```sql
-- 1. Create runtime application roles
CREATE ROLE pgapp_app WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD 'generate-strong-app-password';
CREATE ROLE pgapp_maint WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD 'generate-strong-maint-password';
CREATE ROLE pgapp_worker WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD 'generate-strong-worker-password';

-- 2. Grant schema usage
GRANT USAGE ON SCHEMA public TO pgapp_app, pgapp_maint, pgapp_worker;

-- 3. Grant table permissions
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO pgapp_app, pgapp_maint, pgapp_worker;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO pgapp_app, pgapp_maint, pgapp_worker;

-- 4. Set default privileges for future migrations
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pgapp_app, pgapp_maint, pgapp_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO pgapp_app, pgapp_maint, pgapp_worker;

-- 5. Revoke access to migrations ledger from runtime roles
REVOKE ALL ON schema_migrations FROM pgapp_app, pgapp_maint, pgapp_worker;

-- 6. Enforce append-only ledger security (Revoke UPDATE and DELETE on immutable records)
REVOKE UPDATE, DELETE ON financial_journal_entries, capital_transactions, expense_payments, payout_records, payout_record_items FROM pgapp_app, pgapp_maint, pgapp_worker;

-- 7. Allow maintenance and worker roles to switch to pgapp_app inside scoped transactions
GRANT pgapp_app TO pgapp_maint;
GRANT pgapp_app TO pgapp_worker;
```

---

## 4. Linux VPS Deployment (systemd)

### 4.1 Prerequisites and User Creation

Create the dedicated system user `pgapp`:

```bash
sudo useradd -r -s /bin/false -d /opt/pg-app -m pgapp
sudo mkdir -p /opt/pg-app
sudo chown pgapp:pgapp /opt/pg-app
```

### 4.2 Environment File Installation

Create `/opt/pg-app/.env` and restrict permissions immediately:

```bash
sudo touch /opt/pg-app/.env
sudo chmod 0600 /opt/pg-app/.env
sudo chown root:root /opt/pg-app/.env
```

Populate `/opt/pg-app/.env` with the values defined in the Environment Variables Matrix.

### 4.3 Database Migration Execution

Execute schema migration before starting the web server. Use the direct unpooled database URL:

```bash
sudo -u pgapp /opt/pg-app/migrate up
```

### 4.4 Sudoers Configuration

Install the restricted sudoers rule for zero-downtime deployment:

```bash
sudo cp deploy/pg-app-deploy-sudoers /etc/sudoers.d/pg-app-deploy
sudo chmod 0440 /etc/sudoers.d/pg-app-deploy
sudo visudo -c -f /etc/sudoers.d/pg-app-deploy
```

### 4.5 systemd Unit Installation

Install the main service and the 9 background timers:

```bash
# 1. Install main HTTP service
sudo cp deploy/pg-app.service /etc/systemd/system/pg-app.service

# 2. Run background jobs installer
sudo bash deploy/systemd/install.sh

# 3. Reload systemd daemon
sudo systemctl daemon-reload

# 4. Enable and start main service
sudo systemctl enable --now pg-app.service

# 5. Verify timer activations
systemctl list-timers 'pg-*'
```

### 4.6 Cloudflare Tunnel Setup (`cloudflared`)

Install and configure Cloudflare Tunnel to expose the PWA securely without open inbound firewall ports:

```bash
# 1. Authenticate tunnel
cloudflared tunnel login

# 2. Create tunnel named pg-app
cloudflared tunnel create pg-app

# 3. Route hostname to tunnel
cloudflared tunnel route dns pg-app pg.yourdomain.com

# 4. Install configuration file
sudo cp deploy/cloudflared-config.yml /etc/cloudflared/config.yml
# Edit /etc/cloudflared/config.yml with the generated tunnel UUID

# 5. Install and start cloudflared service
sudo cloudflared service install
sudo systemctl enable --now cloudflared
```

---

## 5. First-Boot Verification Checklist

1. **Verify Startup Role Guards:**
   Inspect service logs to confirm that all three roles connected correctly:

   ```bash
   journalctl -u pg-app.service -n 50 --no-pager
   ```

   Confirm absence of role errors:
   - `production maintenance pool requires 'pgapp_maint'`
   - `production app pool requires 'pgapp_app'`

2. **Verify Background Job Execution:**
   Trigger one job manually and inspect the exit code:

   ```bash
   sudo systemctl start pg-reminder.service
   journalctl -u pg-reminder.service -n 30 --no-pager
   ```

   Confirm log entry contains no `workerPool` or role validation errors.

3. **Verify Health Endpoint:**
   ```bash
   curl -i http://127.0.0.1:8080/health
   ```
   Expected response: HTTP 200 OK.

---

## 6. Rollback Procedure

### 6.1 VPS Rollback

1. Stop the application service:
   ```bash
   sudo systemctl stop pg-app.service
   ```
2. Restore previous binary executable from backup:
   ```bash
   sudo cp /opt/pg-app/server.bak /opt/pg-app/server
   ```
3. Restart service:
   ```bash
   sudo systemctl start pg-app.service
   ```

### 6.2 Database Migration Rollback

1. Execute migration down step:
   ```bash
   sudo -u pgapp /opt/pg-app/migrate down 1
   ```
2. Restart application service:
   ```bash
   sudo systemctl restart pg-app.service
   ```

---

## 7. VPS provisioning checklist (ADR-035)

Use this checklist when Neon exists but no compute host exists yet. Estimated time: 2–4 hours.

1. Create a small Ubuntu VPS (1 vCPU, 2 GB RAM is enough at current scale).
2. Create Neon **staging** and **production** branches; enable PITR on production.
3. Run `cmd/migrate` against each branch with `DATABASE_URL_UNPOOLED` before first app boot.
4. Create roles `pgapp_app`, `pgapp_maint`, `pgapp_worker` per section 3 of this runbook.
5. Copy binaries to `/opt/pg-app/` and env file to `/etc/pg-app/env` (no secrets in git).
6. Run `deploy/systemd/install.sh` and enable `pg-app.service` plus job timers.
7. Point `DATABASE_URL`, `DATABASE_MAINT_URL`, and `DATABASE_WORKER_URL` at Neon (three distinct roles).
8. Verify `GET /healthz` returns 200 and one timer job completes with exit code 0.

**Development without VPS:** run `cmd/server` locally against Neon; timers are manual until step 6 completes.
