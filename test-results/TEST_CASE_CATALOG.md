# IPChat test case catalogue

Status meanings: **PASS** means the listed Go unit test passed in the latest local Docker Go 1.23 run. **NOT RUN** means no integration evidence is available. **NOT IMPLEMENTED** means the feature is absent from this scope.

## Automated unit tests authored

| ID | Test | Coverage | Status |
|---|---|---|---|
| UT-001 | Username validation | allowed characters and length boundaries | PASS |
| UT-002 | Token round-trip and tamper detection | HMAC token signing and integrity | PASS |
| UT-003 | Expired token rejection | expiration enforcement | PASS |
| UT-004 | Password hashing | bcrypt hash and wrong-password rejection | PASS |
| UT-005 | Room code generation | uppercase code and length | PASS |
| UT-006 | Presence hub | unique online users and removal | PASS |
| UT-007 | History pagination | cursor and page-size validation | PASS |
| UT-008 | User reports | reason length validation | PASS |

## Functional / integration acceptance suite

| ID | Area | Required test | Baseline status |
|---|---|---|---|
| INT-001 | Registration | new user, duplicate username, invalid username/password | Temporary registration smoke passed; full negative cases not run |
| INT-002 | Login | valid/invalid credentials and token expiry | NOT RUN — DB integration required |
| INT-003 | Rooms | create/join with correct and wrong passphrase | NOT RUN — DB integration required |
| INT-004 | Room quota | 15 owned rooms allowed, 16th rejected | NOT RUN — DB integration required |
| INT-005 | Concurrency | concurrent room creation cannot bypass quota | NOT RUN — DB integration required |
| INT-006 | Membership | non-member cannot read history or subscribe | NOT RUN — DB/WebSocket integration required |
| INT-007 | Roles | owner/maintainer/member permission matrix | NOT RUN — DB integration required |
| INT-008 | Moderation | maintainers cannot kick owner or another maintainer | NOT RUN — DB integration required |
| INT-009 | Revocation | kicked member's active WebSocket closes | IMPLEMENTED CHANGE — NOT RUN |
| INT-010 | Messages | message persistence, history order and room isolation | Temporary room/DM WebSocket smoke passed; room isolation not fully tested |
| INT-011 | Moderation | author message deletion; owner/maintainer room moderation | Own-message room/DM deletion smoke passed; moderator matrix not run |
| INT-014 | Chat | authorized message edits and cursor-paged history | Temporary room/DM edit and pagination smoke passed |
| INT-015 | Safety | blocking, unblock, and report persistence | Temporary block/unblock and report smoke passed |
| INT-016 | Receipts | delivery and read receipts between two DM clients | Temporary two-account receipt smoke passed |
| INT-012 | Realtime | send/receive, typing and presence | Temporary WebSocket send/receive and presence smoke passed; typing not tested |
| INT-013 | Scaling | Redis Pub/Sub across two API replicas | NOT RUN — multi-instance test required |
| SEC-001 | Origin policy | reject untrusted HTTP and WebSocket origins | IMPLEMENTED CHANGE — NOT RUN |
| SEC-002 | Authorization | IDOR and cross-room access attempts | NOT RUN — security test required |
| SEC-003 | WebSocket auth | missing, invalid, expired and revoked token | NOT RUN — WebSocket test required |
| SEC-004 | Rate limiting | login, registration, room join and message floods | NOT IMPLEMENTED |
| SEC-005 | Session lifecycle | refresh token rotation and logout revocation | NOT IMPLEMENTED |
| SEC-006 | E2EE | browser-side encryption; server stores ciphertext only | IMPLEMENTED — multi-client crypto test not run |
| SEC-007 | Attachments | client-side encryption and authorized downloads | IMPLEMENTED — integration test not run |
| FEAT-001 | Account | email verification and password reset | NOT IMPLEMENTED |
| FEAT-002 | Room | invitations, QR join, ownership transfer and deletion | NOT IMPLEMENTED |
| FEAT-003 | Chat | DM receipts, edits/deletes, replies and reactions | IMPLEMENTED — DB integration not run |
| FEAT-007 | Chat | paginated room and DM history | IMPLEMENTED — UI integration not run |
| FEAT-008 | Safety | DM blocks and administrator-visible user reports | IMPLEMENTED — DB integration not run |
| FEAT-004 | Chat | client-side search over decrypted history | NOT IMPLEMENTED |
| FEAT-005 | Notifications | notification preferences and privacy-safe previews | NOT IMPLEMENTED |
| FEAT-006 | Retention | auto-delete and retention policy | NOT IMPLEMENTED |
| OPS-001 | Resilience | PostgreSQL/Redis restart and reconnect recovery | NOT RUN |
| OPS-002 | Backup | backup and restore verification | NOT RUN |
| OPS-003 | Load | p50/p95/p99 latency, throughput and concurrent sockets | NOT RUN |
| OPS-004 | Deployment | Docker Compose build/start/health checks | PASS — build and `/health/ready` verified |

## Execution

Run `powershell -ExecutionPolicy Bypass -File .\scripts\run-tests.ps1` from the project directory. The script writes the machine-readable Go test stream to `test-results/go-test.jsonl` and regenerates `test-results/index.html`.

The report is available to the configured administrator at `/admin/test-results` using HTTP Basic authentication. Configure `ADMIN_USERNAME` and `ADMIN_PASSWORD_HASH`; generate the hash using `go run ./cmd/hashadmin`.
