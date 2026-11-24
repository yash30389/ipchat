# Architecture Decisions

## Retain Go and PostgreSQL
The existing implementation is a working Go HTTP/WebSocket service with PostgreSQL and Redis. Replacing it with Node.js and Prisma would create a second implementation and was not a surgical change.

## Trade-offs
- Gorilla WebSocket is adequate for the baseline but less feature-rich than Socket.IO.
- PostgreSQL remains the durable source of truth.
- Redis is used only for event fan-out.
- Argon2id is used for account and room passwords.
- Browser Web Crypto AES-GCM protects message bodies; Web Crypto does not provide an Argon2id primitive. Room passphrases use PBKDF2-SHA256 at 250,000 iterations with a random server-stored salt as a browser-compatible documented fallback. This is not claimed to be Argon2id.
- Room passphrases provide weak authentication/KDF input unless users choose strong passphrases. Lost passphrases prevent decryption.

## Session model
Signed HMAC tokens carry a session ID. PostgreSQL validates session existence, expiry, and revocation.
