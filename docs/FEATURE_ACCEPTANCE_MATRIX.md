# Feature Acceptance Matrix

| Feature | Implementation | Verification / limits |
|---|---|---|
| Registration and login | Username/password and configured OAuth providers | Existing Go tests; provider sign-in needs configured credentials |
| Private one-to-one conversations | Direct conversations with client-side encryption | Temporary two-account WebSocket/API smoke passed; browser crypto/key recovery not tested |
| Group conversations | Password-protected rooms | Temporary room WebSocket/API smoke passed |
| Text, emoji, files, replies, reactions, GIFs | Encrypted room/DM payloads and encrypted attachments; responsive categorized emoji picker and six original bundled GIFs | Desktop/mobile picker, GIF filtering, and GIF decoding verified in browser; encrypted GIF sending not exercised to avoid creating test messages |
| Message edit and delete | Authors can edit/delete; room moderators can delete any room message | Own-message edit/delete smoke passed in rooms and DMs; moderator matrix not tested |
| Typing and online/offline presence | WebSocket typing events and authenticated connection presence | Presence smoke passed; typing/reconnection/load behavior needs more coverage |
| Delivery and read receipts | Stored and broadcast for direct messages | Two-account delivery/read smoke passed; group receipts are not implemented |
| History and pagination | Cursor-based pages, capped at 100 messages, for rooms and DMs | Parser unit tests and paged room/DM API smoke passed |
| End-to-end encryption | Browser encryption for rooms, DMs, and attachments | Ciphertext persistence architecture present; direct-message key recovery and cross-device sync are not implemented |
| Block users | Prevents new DMs and typing in either direction; existing history and room messages remain visible | Block, reject-new-chat, and unblock smoke passed |
| Report users | Stores a reason without decrypted content; admin-only report listing | Report submission smoke passed; administrator review workflow is API-only |
| Group admins/member management | Owner/maintainer roles, role assignment, kick and message moderation | Existing implementation; full permission matrix needs integration tests |
| Voice/video messages | Encrypted browser recordings, permission-gated | Device/browser capture and playback need manual tests |
| Live calls/screen sharing | Not implemented | — |
| Docker Compose | Multi-stage image and Compose services | Build and `/health/ready` verified in the development environment |
