# Module: M1b Delegated Manager Capabilities

**Sprint:** 3 · **ADR:** ADR-032

## Gate 1 — Journeys

| ID | Route | Status |
| --- | --- | --- |
| J1 | `GET /api/owner/managers` | Added |
| J2 | `PUT /api/owner/managers/:id/capabilities` | Added |
| J3 | `POST /api/owner/managers/:id/deactivate` | Added |
| J4 | `GET /api/manager/tenants` (`tenants.view`) | Added |
| J5 | `GET /api/manager/dues` (`dues.view`) | Added |
| J6 | `POST /api/manager/attendance/daily` (`attendance.mark`) | Added |

## Gate 8 — Exit

- [x] Migration 070 + CapabilityRepo grant/revoke
- [x] capability_matrix_test route tags expanded
