# ADR-033: Slot invites and room occupancy reservations

## Status

Accepted (P0-B).

## Context

Flat `properties.invite_code` cannot bind floor/room or freeze rent terms. Admission must reserve beds atomically and survive concurrent joins.

## Decision

1. **`slot_invites`** rows hold immutable terms at issuance (`terms_frozen_at`, `profile_schema_version`).
2. **`room_occupancy.open_invite_reserved`** counts beds held by `open` invites when `reserve_beds_for_open_invites` is true.
3. **Availability** in a transaction: `rooms.capacity - tenant_beds(room) - open_invite_reserved > 0`, unless `allow_over_capacity` with audit.
4. **`properties.occupancy_revision`** increments on bed-affecting writes for dashboard cache busting.
5. **Consume** on `POST /join` (`CompleteOnboarding`): `FOR UPDATE` slot row, mark `consumed`, release reservation, link `join_requests.slot_invite_id`.
6. **Master path** unchanged: `properties.invite_code` with `property_onboarding_settings` defaults.

## Consequences

- Revoke/expiry must decrement `open_invite_reserved`.
- Auto-activate from slot terms is P0-C; P0-B only consumes and links room/terms metadata on the invite row.
