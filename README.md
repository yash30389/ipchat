# IPChat Production Baseline

This archive is an incremental hardening of the uploaded IPChat project. It preserves the existing Go + PostgreSQL + Redis + browser UI design and adds unit-test scaffolding, an administrator-only HTML test report, stricter origin checks, WebSocket disconnect on member kick, and build hygiene fixes.

## Run locally

1. Install Go 1.23+ and Docker Desktop.
2. Copy `.env.example` to `.env` and keep `.env` private. Never commit it or share real credentials.
3. Replace the placeholders: set `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, and a random `JWT_SECRET` of at least 32 bytes.
4. Configure `ADMIN_USERNAME` and `ADMIN_PASSWORD_HASH`. Generate a bcrypt hash with `go run ./cmd/hashadmin` (the helper reads the password from the terminal; the prompt notes that input is visible).
5. Start: `docker compose up --build -d`.
6. Open `http://localhost:8080`.
7. Admin test report: `http://localhost:8080/admin/test-results` (HTTP Basic authentication).

Do not run `docker compose down -v` on a database containing data; `-v` deletes named volumes.

## Social sign-in (GitHub and Google)

Social sign-in is optional. Create OAuth apps with these exact local callback URLs:

- GitHub: `http://localhost:8080/auth/github/callback`
- Google: `http://localhost:8080/auth/google/callback`

Set `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `GOOGLE_CLIENT_ID`, and
`GOOGLE_CLIENT_SECRET` in the private `.env` file. Set `OAUTH_REDIRECT_BASE` to
the app's origin (for example `http://localhost:8080` locally or your HTTPS
origin in production), then rebuild with `docker compose up --build -d`.
Never put provider secrets in frontend code or share them in chat.

GitHub access requests `read:user` and `user:email`; Google requests
`openid email profile`. New accounts are created only when the provider returns
a verified email. Provider identities are linked by the provider's stable user
ID, not by email, so social sign-in will not silently take over an existing
password account.

## Tests

Run `powershell -ExecutionPolicy Bypass -File .\\scripts\\run-tests.ps1`. The script downloads pinned Go modules first, then executes tests. The generated report is stored in `test-results/index.html`; the JSON event stream and log are stored beside it. The admin endpoint serves that HTML report after Basic authentication.

## Important scope and release status

This is **not yet the complete production IPChat specification**. Password-protected rooms provide basic group conversations with owner/maintainer roles, member management, and moderation; direct messages are one-to-one. Room and direct-message content and attachments are encrypted in the browser before storage; direct-message keys are stored in browser local storage and have no recovery/backup flow. Both chat types support text, emoji, file messages, replies, reactions, history pagination (100 messages per page), and editing/deleting your own messages. Room owners and maintainers may also delete room messages. Direct messages include delivery/read receipts and optional disappearing-message timers (off by default; 24 hours, 7 days, or 90 days for new messages and attachments). Blocks prevent direct messaging and typing in either direction but do not hide shared room messages or erase existing history. Users can report another account; report reasons are visible only through the HTTP Basic-authenticated `/admin/reports` endpoint and do not include decrypted message content. Attachments and recorded voice/video messages are limited to 10 MB. Browser recording requires explicit microphone/camera permission and is limited to 60 seconds for voice and 30 seconds for video. The composer provides native Unicode emoji and a searchable collection of original, locally bundled GIF reactions; selected GIFs are encrypted and sent as attachments. Live calls/screen sharing, email verification, password reset, refresh-token rotation/revocation, notifications, QR invitations, ownership transfer, general retention controls, and a full admin operations console remain out of scope. Integration, cross-device key recovery, and load testing still need separate validation.

Emoji and GIF sharing are grouped in one responsive composer picker. Emoji are standard Unicode characters rendered by the user's platform font; no Instagram artwork is bundled. The send button becomes active only when a chat connection is ready and the message field contains text. Online/offline presence is automatic for authenticated users with an active app connection; users cannot manually set their presence. Disappearing timers apply only to messages and attachments created after the setting is changed. Expired direct messages and attachments are removed by a background cleanup task. Delivery/read receipts and disappearing timers currently apply to direct messages, not group rooms.

The existing SQL migration `migrations/002_persistent_members.sql` is retained for review and is not automatically applied by the current application migration function. Do not run it blindly: it conflicts with the existing `room_members` table definition. A proper versioned migration reconciliation is required before applying it to a database with real data.

## Security notes

- `ALLOWED_ORIGINS` accepts comma-separated exact origins. Same-origin browser use works without setting it.
- WebSocket upgrades use short-lived, single-use tickets in the query string; configure TLS and avoid logging query strings in production.
- Use TLS through a reverse proxy in production.
- The current JWT design has no server-side revocation. Treat it as a development baseline.
- Direct-message private encryption keys are held in browser local storage. Clearing browser data or replacing the key can make old direct messages unreadable; this implementation does not sync keys between devices.
- Do not expose this application to the public internet until the remaining security acceptance suite passes.
