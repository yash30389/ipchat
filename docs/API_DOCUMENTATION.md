# API Documentation

All JSON API endpoints require `Authorization: Bearer <session-token>` unless otherwise stated.

- `POST /api/register`: username/password registration.
- `POST /api/login`: username/password login.
- `POST /api/logout`: revoke current session.
- `POST /api/logout-all`: revoke all user sessions.
- `POST /api/ws-ticket?room=CODE`: issue a 30-second single-use WebSocket ticket.
- `GET /api/rooms`: list joined rooms.
- `POST /api/rooms/create`: create a room.
- `POST /api/rooms/join`: join with room code/passphrase.
- `POST /api/rooms/leave`: leave a non-owner room.
- `GET /api/rooms/history?room=CODE&before_id=ID&limit=100`: encrypted, cursor-paged history. The response is `{messages, has_more}`; page size is capped at 100.
- `GET /api/rooms/members?room=CODE`: room member list.
- `POST /api/rooms/role`: owner role update.
- `POST /api/rooms/kick`: owner/maintainer removal.
- `POST /api/rooms/message/delete`: delete your own room message, or any message as an owner/maintainer.
- `POST /api/messages/edit`: edit your own encrypted message in a room or DM.
- `GET /api/dms`: list one-to-one conversations, including block state.
- `GET /api/dms/history?id=ID&before_id=ID&limit=100`: cursor-paged encrypted DM history and delivery/read state.
- `POST /api/dms/delivered`: acknowledge that incoming messages reached a participant's client.
- `POST /api/dms/read`: mark incoming messages read.
- `POST /api/dms/message/delete`: delete your own DM message.
- `POST /api/users/block`: block or unblock a username for direct messages.
- `GET /api/users/blocks`: list usernames you have blocked.
- `POST /api/users/report`: report a username with a 5–500 character reason. Encrypted message contents are not included.
- `GET /health/live`: liveness.
- `GET /health/ready`: PostgreSQL/Redis readiness.
- `GET /admin/test-results`: Basic-auth protected report.
- `GET /admin/reports`: Basic-auth protected JSON list of the latest 200 user reports.

Blocking affects direct-message sending and typing only; it does not hide or remove shared room messages. Existing DM history remains available to both participants. Validation limits: username 3–40 ASCII characters; account password 8–72 characters; room name 80 characters; room code 16 characters; WebSocket message ciphertext 24 KiB; history page size 100. Errors use JSON `{"error":"..."}`. Authentication, not HTTP status alone, must be enforced on every operation.
