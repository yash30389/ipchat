# Database Design

PostgreSQL is authoritative. Core tables include users, rooms, room_members, messages, direct_conversations, user_dm_keys, message_reactions, attachments, user_blocks, user_reports, user_sessions, and audit_logs.

Messages belong to exactly one room or direct conversation. Direct conversations store the participant pair in sorted user-ID order, preventing duplicate one-to-one threads. Their `disappearing_seconds` setting is off by default and supports 24-hour, 7-day, or 90-day expiration for new direct messages and attachments. `edited_at` marks changed messages; `dm_delivered_at` and `dm_read_at` record DM receipt state; `expires_at` drives cleanup. History is cursor-paged, with a maximum of 100 messages per page. Expired messages and attachments are deleted by a background task. Public direct-message encryption keys are stored per account; private keys remain in the browser. Reactions are unique per message/user/emoji. Attachments, including recorded voice/video notes, are stored as ciphertext in PostgreSQL and are limited to 10 MB at the API boundary.

`user_blocks` records directional block preferences. A block prevents new direct messages and typing events in either direction, but does not hide shared room messages or remove existing DM history. `user_reports` stores a reporter, reported account, reason, and timestamp; only the configured HTTP Basic-authenticated administrator can read report records through `/admin/reports`. Report records contain no decrypted message content.

Key constraints include unique usernames, unique room membership, allowed member roles, foreign keys, and message/room/direct-chat indexes. Room creation locks the user row and counts owned rooms inside a transaction. Session IDs are primary keys and revoked/expiry state is enforced server-side.

Room and direct-message content is encrypted in the browser before storage. Direct-message encryption uses browser-generated RSA-OAEP key pairs to wrap per-message and per-attachment AES-256-GCM keys. Replies are included in encrypted message payloads. Voice/video notes are recorded only after the user grants browser media permission and are encrypted client-side before upload. The private key is currently browser-local and has no recovery or cross-device synchronization mechanism.

Migration `migrations/002_persistent_members.sql` is incompatible with the existing user-ID membership model and must not be applied automatically. It remains as review evidence only.
