# IPChat Secure

A lightweight, encrypted, real-time chat platform. Join a room with a **room
code + username + passphrase** — no account signup. Includes realtime
messaging, typing/read receipts, encrypted file sharing, 1:1 video/voice
calling (WebRTC), presence, search, dark mode, PWA support, and a full
admin console with live chat monitoring.

Built to the stack in the project's PRD: **React + Vite + TypeScript +
Tailwind** (frontend) and **Node.js + Express + Socket.IO + PostgreSQL
(Prisma) + Redis** (backend). Docker/Kubernetes manifests are intentionally
**not** included — you said you'll manage deployment yourself.

---

## 1. Project layout

```
ipchat-secure/
├── backend/          Node/Express API + Socket.IO realtime server
│   ├── prisma/schema.prisma
│   └── src/
│       ├── config/        env, logger, Prisma client, Redis client
│       ├── middleware/    auth, rate limiting, error handling
│       ├── routes/        rooms, messages, files, admin
│       ├── sockets/       Socket.IO handlers (chat + WebRTC signaling + admin live-watch)
│       ├── utils/         AES-256-GCM crypto, Argon2id hashing, JWT
│       └── jobs/          auto-delete sweep, admin seed script
└── frontend/          React/Vite/TS/Tailwind SPA
    └── src/
        ├── pages/          JoinPage, ChatPage, AdminLoginPage, AdminDashboardPage
        ├── components/     MessageBubble, ChatComposer, PresenceBar, VideoCallModal, TypingIndicator
        ├── lib/            api client, socket client, WebRTC helper
        └── store/          zustand auth/theme state
```

## 2. How identity & rooms work

- Anyone can **create a room**: they set a room passphrase and get back a
  short shareable `roomCode` (e.g. `amber-falcon-42` — this is the "room IP"
  concept from the PRD: a logical room identifier, not a literal network
  address).
- To **join**, a user submits `roomCode + username + passphrase`.
  - Wrong room passphrase → rejected.
  - New username in that room → a new identity is created, bound to the
    passphrase they used.
  - Existing username → the passphrase must match what that identity
    originally used, so nobody can hijack a username in an active room.
- Message history is scoped to `room + participant`, matching the PRD's
  "history visible only for same room+username+passphrase" requirement.

## 3. Security model (please read this)

- **At rest:** every message and file is encrypted with **AES-256-GCM**
  using a server-held master key. Passphrases are hashed with **Argon2id**
  (OWASP-recommended, memory-hard). Nothing is stored in plaintext.
- **In transit:** run everything behind TLS (see §6) — Socket.IO and the
  REST API both need HTTPS/WSS in production.
- **Important honesty note:** the PRD also asks for an admin panel with
  full access to chat contents. That is fundamentally in tension with true
  end-to-end encryption, where only participants hold the keys. This build
  encrypts at rest and in transit, but the **server holds the decryption
  key**, so admins (and anyone who compromises an admin account or the DB +
  master key) can read messages. If you need genuine zero-knowledge E2E
  later, keys must be derived and held client-side only, and the admin
  "read everything" feature would need to be dropped or replaced with
  participant-consented access. I built it this way because you explicitly
  asked for both; just know the trade-off exists.
- Every admin read of a room's messages, ban, delete, and live-watch is
  written to `AuditLog`.
- Rate limiting on auth/join endpoints blunts passphrase brute-forcing.
  Use a **long, random passphrase** — Argon2id slows attackers down but
  can't save a weak passphrase.

## 4. Scaling to many concurrent users

The architecture is stateless-app + shared-state-in-infra, which is what
lets it scale horizontally:

- **Backend is stateless** — you can run any number of Node instances
  behind a load balancer. No in-process session or socket state that only
  one instance knows about.
- **Socket.IO uses the Redis adapter** (`@socket.io/redis-adapter`), so a
  message sent to a room reaches every user regardless of which instance
  they're connected to.
- **Presence** (who's online) is stored in Redis sets, not memory, so it's
  correct across instances too.
- **Postgres + Prisma** with cursor-based pagination on message history
  (no expensive `OFFSET` scans as history grows).
- **WebRTC calls are peer-to-peer** — the server only relays signaling
  (SDP/ICE), so video/audio traffic never loads your backend. For calls
  across restrictive NATs/firewalls you'll want to add a TURN server (see
  `frontend/src/lib/webrtc.ts` — there's a comment showing where).
- Sticky sessions aren't required (Redis adapter handles cross-instance
  delivery), but if you terminate WebSockets at a load balancer, make sure
  it supports WebSocket passthrough.

To genuinely serve millions of concurrent users you'll also need, at the
infrastructure layer (your call, since you're managing deployment):
multiple backend replicas behind a load balancer, a managed/clustered
Postgres with read replicas, a Redis cluster, object storage (S3-compatible)
for files instead of local disk, a CDN for the frontend build, and a TURN
service for calls. The code is written so none of that requires app
changes — it's ops/infra scaling on top of an already-horizontal design.

## 5. Local setup (no Docker)

**Prerequisites:** Node.js 20+, a running PostgreSQL instance, a running
Redis instance.

### Backend
```bash
cd backend
cp .env.example .env
# Edit .env: set DATABASE_URL, REDIS_URL, and generate a real master key:
node -e "console.log(require('crypto').randomBytes(32).toString('hex'))"
# paste that into MASTER_ENCRYPTION_KEY

npm install
npm run prisma:generate
npm run prisma:dev        # creates tables
npm run seed:admin        # creates your first admin login (uses SEED_ADMIN_* from .env)
npm run dev                # http://localhost:4000
```

### Frontend
```bash
cd frontend
cp .env.example .env       # defaults point at localhost:4000, adjust if needed
npm install
npm run dev                 # http://localhost:5173
```

Open two browser windows at `http://localhost:5173`, create a room in one,
join it from both with the same room code + passphrase and different
usernames, and chat/call between them. Admin console is at
`http://localhost:5173/admin/login`.

## 6. Deploying yourself (brief pointers, since you're handling this)

- Put Nginx (or your LB of choice) in front, terminate TLS there, proxy
  `/api` to the backend and pass through `Upgrade`/`Connection` headers for
  Socket.IO's WebSocket transport.
- Set `NODE_ENV=production`, real secrets, and a TURN server for calls.
- Point `UPLOAD_DIR` at a mounted volume, or swap `src/routes/files.js` to
  write to S3-compatible storage for multi-instance deployments.
- Run `npm run prisma:migrate` (not `prisma:dev`) against production.

## 7. Feature checklist vs. the PRD

| Requirement | Status |
|---|---|
| Realtime messaging | ✅ Socket.IO |
| Typing / read receipts | ✅ |
| Encrypted files | ✅ AES-256-GCM at rest |
| Room code join (the "room IP" concept) | ✅ |
| Dark mode | ✅ |
| Search | ✅ (per-room, bounded scan) |
| Auto delete | ✅ per-room TTL sweep job |
| Notifications | ⚠️ in-app only (typing/presence/system notices). Push notifications need a service worker + push provider (e.g. Web Push/FCM) — not wired up, ask if you want it added |
| PWA | ✅ manifest + service worker via `vite-plugin-pwa` |
| Video calling | ✅ WebRTC, 1:1, signaled over Socket.IO |
| Admin: full chat access | ✅ (see security trade-off in §3) |
| Admin: live handling | ✅ live message stream + moderation broadcast + ban/delete |
| AES-256-GCM + Argon2id | ✅ |
| Audit logging | ✅ |
| Docker / Kubernetes | ❌ intentionally omitted — you're managing this |

## 8. Known simplifications (so nothing surprises you later)

- Calls are 1:1, not group calls (group calling needs an SFU like mediasoup
  or LiveKit — happy to add if needed).
- Search decrypts a bounded recent window per room rather than maintaining
  a separate search index; fine at room scale, worth revisiting if a single
  room grows to hundreds of thousands of messages.
- Push notifications aren't wired up (see table above).
- No email/SMS — identity is purely room+username+passphrase, by design.
#
