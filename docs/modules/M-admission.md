# Module: M-admission

**Owner:** Divakar · **ADR:** [033-slot-invites-and-occupancy.md](../adr/033-slot-invites-and-occupancy.md) · **CONTRACT:** [Join section](../../CONTRACT.md)

## Gate 1 — Journeys (ASD-STE100)

| ID  | Actor          | Precondition                   | Action                                | Expected HTTP | Side effect                             | Test                                          |
| --- | -------------- | ------------------------------ | ------------------------------------- | ------------- | --------------------------------------- | --------------------------------------------- |
| J1  | Pending tenant | Valid slot invite code         | `POST /auth/firebase` + `POST /join`  | 200           | `pending_allocation` tenant; JWT issued | `handlers_join_session_test`                  |
| J2  | Pending tenant | Room full; waitlist on         | `POST /join/waitlist`                 | 201           | FIFO waitlist row                       | `handlers_join_waitlist_test`                 |
| J3  | Owner          | Waitlist head exists; bed free | `POST /owner/waitlist/:room_id/offer` | 201           | Bound slot invite                       | live / manual                                 |
| J4  | Pending tenant | Offered invite                 | `POST /join/waitlist/accept`          | 200           | `join_requests.slot_invite_id` set      | API test                                      |
| J5  | Owner          | Property configured            | `PATCH /owner/onboarding-settings`    | 200           | DB settings row                         | `handlers_owner_onboarding_settings_test`     |
| J6  | Referrer       | First rent paid                | Payment event                         | —             | Referrer `credit_balance_paise`         | `referral/first_rent_live_test` + refund hook |

## Gate 4 — Authz

- Gate 06 matrix: [`route_role_matrix_test.go`](../../internal/api/route_role_matrix_test.go)
- Slot invite IDOR: [`handlers_owner_slot_invites_test.go`](../../internal/api/handlers_owner_slot_invites_test.go)

## Gate 5 — Tests (Karpathy order)

1. Inspect `room_occupancy` and `slot_invites` with SQL after one invite.
2. Overfit: one property, one room, one slot, one join (`slot_invite_consume_live_test`).
3. Matrix all routes; k6 `scripts/loadtest/scenarios/admission_burst.js` under load.

## Gate 6 — Ops

- Jobs: `cmd/slot-invite-expiry`, `cmd/waitlist-offer-expiry`, `cmd/join-pending-expiry`
- Staging smoke: [deploy_and_jobs.md §8](../runbooks/deploy_and_jobs.md#8-admission-staging-smoke-asd-ste100)
