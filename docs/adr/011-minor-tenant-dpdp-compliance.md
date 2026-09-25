# ADR 011: Minor Tenant Protection, Guardian Verification & DPDP Compliance

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
Under India's Digital Personal Data Protection Act (DPDP Act 2023), processing personal data of children (< 18 years) requires verifiable parental or guardian consent. In PG and student hostel accommodations, minor students frequently reside with parental support. Furthermore, behavioral tracking and gamification directed at minors are subject to strict regulatory constraints.

## Decisions

### 1. Guardian KYC & Verifiable Consent
- Tenants under 18 years must provide DigiLocker-verified guardian KYC (Aadhaar/PAN verification) and verifiable guardian consent.
- Payment notifications and invoice copies are CC'd or routed to the verified guardian's contact details.

### 2. Gamification Isolation for Minors
- Gamification mechanics (streaks, leaderboards, reward points, behavior scores) are automatically disabled for minor tenants (`age < 18`).
- Financial operations (rent generation, due tracking, invoice payment) remain strictly standard and transparent.

### 3. Data Minimization & Retention
- Guardian identity artifacts are retained strictly for lease tenure and statutory proof requirements, then scheduled for secure purge.

## Consequences
- Full alignment with Indian DPDP Act provisions for children's data.
- Protection against unauthorized contracts with minors.
- Clean separation between gamification features and core financial tenant profiles.
