# Project Audit

> Historical baseline from the original hardening delivery. Current chat feature scope and verification are tracked in `README.md` and `FEATURE_ACCEPTANCE_MATRIX.md`; several items below have since been implemented.

## Original findings
- Existing Go, PostgreSQL, Redis, Gorilla WebSocket, and browser UI implementation is retained.
- Authentication used HMAC tokens, bcrypt passwords, and had no server-side revocation.
- Messages were stored as plaintext and had no end-to-end encryption.
- WebSocket authentication placed a bearer token in the URL.
- Room authorization existed for history, roles, kicks, and message deletion, but the frontend was the only room UI.
- Redis Pub/Sub supported cross-instance events.
- Docker Compose supported PostgreSQL and Redis.
- Missing features included refresh/reuse detection, MFA, email verification, password reset, invitations, attachments, receipts, and full retention.

## Completed in this delivery
- Argon2id password hashing with bcrypt verification compatibility.
- Server-side user-session records and logout/revoke-all support.
- Short-lived, single-use WebSocket tickets.
- Security headers, CORS allowlist, and basic auth and join rate limits.
- Room membership quota remains transactionally enforced.
- AES-256-GCM browser encryption envelopes for messages.
- Per-room public E2EE salt metadata.
- Health liveness/readiness endpoints.
- Protected test-report route.

## Remaining gaps
The following require additional implementation, external services, or environment verification: refresh-token reuse detection, MFA, email workflows, attachments, QR/invitation workflows, delivery/read receipts, edit/retention operations, Kubernetes manifests, integration/browser/load environments, and full migration history.
