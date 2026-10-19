# Auth, KYC, Localization, Theme and Performance Audit

Scope: static review of the supplied React/TypeScript PWA and Go backend archives. This is not a claim of a complete penetration test or production certification.

## Verified observations

### Theme and preferences
- The frontend theme provider previously cast any `localStorage` value to `SupportedTheme`. A stale/corrupt value could enter state and produce an invalid document theme.
- Direct localStorage access in theme initialization and the theme-apply callback could throw under storage restrictions and interrupt theme switching.
- The theme transition path uses View Transitions and `flushSync`; that can look polished, but it is not a guarantee of zero latency. Full-root snapshots and animated clipping can cost paint/compositing work on lower-end mobile devices.
- The patch validates the persisted theme, makes theme storage best-effort, validates runtime `setTheme` calls, protects `matchMedia` access, and validates the theme returned by the preference API before updating auth user state.
- Server preference updates are asynchronous. UI should remain optimistic, but a failed write must remain visibly/reliably retryable. The existing pending-key retry logic is best-effort and depends on browser storage being available.

### Localization
- The client declares four supported locales: `en-IN`, `te-IN`, `ta-IN`, `kn-IN`, and uses `react-intl` with English fallback messages.
- Translation dictionaries declare the same key type, but key parity alone does not prove linguistic quality, completeness of translated UI strings, accessibility labels, server-generated messages, emails, push notifications, or third-party Cashfree screens. Run a rendered-string audit by route and role before claiming end-to-end localization.
- The backend locale resolver honors a valid saved user preference, then `Accept-Language`, then `en-IN`. That is appropriate for request negotiation, but persistent user preference should remain authoritative for authenticated user-facing content.
- The language selector's header is hard-coded as a four-language string, which is inconsistent with a fully localized interface and with the single-locale UX. Move it into message catalogs.

### KYC and identity data
- The backend enforces a 5 MiB upload limit in the handler and reads at most 5 MiB + 1 byte in the service. It also has tenant/property ownership checks for owner document retrieval and strips several fraud-sensitive fields from the tenant status response.
- The upload handler trusts the multipart part's declared `Content-Type` and passes it to the KYC service; the service forwards file bytes and filename to the provider without validating file signatures in the inspected path. Client MIME and extension are attacker-controlled. Add server-side allowlisting based on detected file signature (PDF/JPEG/PNG only), reject empty/invalid files, sanitize filenames, and test polyglot/mislabeled payloads before production.
- The tenant rate limiter is PostgreSQL-backed in the production router, which is preferable to process-local rate limits for horizontal replicas. Validate database failure behavior and whether the limiter fails closed for identity-verification endpoints.
- KYC document/photo responses should be checked for `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, a restrictive content disposition, and no shared/CDN caching. The inspected handler calls `c.Data` directly; verify these headers are set by middleware before exposing real identity documents.
- KYC must be tested against real database migrations, Cashfree sandbox failure modes, webhook replay/out-of-order delivery, consent revocation races, expired leases, and property/tenant cross-tenant access attempts. Unit tests alone cannot establish those guarantees.

### Authentication/session boundaries
- Access tokens are held in an in-memory token store and refresh requests use `credentials: include`; that reduces persistent access-token exposure compared with localStorage bearer tokens.
- Review refresh-cookie `Secure`, `HttpOnly`, `SameSite`, CSRF/origin enforcement, rotation/reuse detection, and logout/revocation behavior in deployed configuration. Frontend code cannot prove those server and deployment settings.
- Cross-tab refresh coordination uses `navigator.locks` when available, but the fallback is process-tab-local. Test simultaneous refreshes in browsers that do not support Web Locks and ensure server-side token-family rotation safely handles races.

## Production acceptance gates

1. Run `npm ci`, `npm test`, `npm run lint`, `npm run build`, and `npm run check:error-codes` in CI on the exact commit.
2. Run `go test ./...`, `go test -race ./...`, `go vet ./...`, migration tests, and integration tests against disposable PostgreSQL and Cashfree sandbox mocks.
3. Add adversarial tests for invalid theme values, blocked localStorage, rapid theme changes, system-theme changes, reduced-motion preferences, and API preference writes completing out of order.
4. Build a locale coverage report from rendered screens, including validation errors, empty states, auth/KYC, notifications, date/number/currency formatting, pluralization, and screen-reader labels. Use native-language review; type-level completeness is not translation QA.
5. Add KYC upload signature tests, size-boundary tests, hostile filenames, content-type spoofing, document-cache header tests, and owner/manager cross-property authorization tests.
6. Measure theme-switch interaction latency on a low-end Android device. Target p95 input-to-first-paint under 100 ms; avoid full-root snapshots if traces show long paint or compositing. Do not promise “0 lag”; define measurable budgets.
7. In HA deployments, verify preferences and KYC state are database-authoritative across replicas; never depend on in-memory state for authorization, throttling, revocation, or KYC status.

## Verification limitations

The supplied environment had no installed frontend dependencies (`vitest: not found`) and could not download the Go 1.26 toolchain because outbound network/DNS was unavailable. Therefore, tests and builds were not executed. The source patch is not certified until the CI gates above pass.
