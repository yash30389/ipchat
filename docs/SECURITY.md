# Security

Implemented: Argon2id account and room password hashing, bcrypt migration compatibility, session revocation, one-time WebSocket tickets, exact CORS/origin allowlisting, security headers, body-size defaults, input limits, and basic authentication/rate limiting. Real-time and API operations verify current membership.

Known gaps: refresh-token rotation, MFA, CSRF risk review, formal rate-limit storage in Redis, attachment scanning, SSRF controls, full audit event coverage, MFA, and dependency/container scans. No OWASP-compliant claim is made.
