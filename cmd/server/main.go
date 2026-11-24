package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

var (
	db        *sql.DB
	rdb       *redis.Client
	jwtSecret []byte
	ctx       = context.Background()
	hub       = newHub()
)

type Claims struct {
	UserID    int64  `json:"uid"`
	Username  string `json:"username"`
	Expires   int64  `json:"exp"`
	SessionID string `json:"sid"`
}

type Client struct {
	conn *websocket.Conn
	send chan []byte
	room string
	user string
}

type Hub struct {
	mu    sync.RWMutex
	rooms map[string]map[*Client]bool
}

const globalPresenceChannel = "presence:global"

func newHub() *Hub {
	return &Hub{rooms: make(map[string]map[*Client]bool)}
}

func (h *Hub) add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[c.room] == nil {
		h.rooms[c.room] = make(map[*Client]bool)
	}
	h.rooms[c.room][c] = true
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms[c.room], c)
	if len(h.rooms[c.room]) == 0 {
		delete(h.rooms, c.room)
	}
	close(c.send)
}

func (h *Hub) broadcast(room string, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[room] {
		select {
		case c.send <- payload:
		default:
			// A slow client will be disconnected by its write loop.
		}
	}
}

func (h *Hub) closeUser(room, username string) {
	h.mu.RLock()
	clients := make([]*Client, 0)
	for c := range h.rooms[room] {
		if c.user == username {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range clients {
		_ = c.conn.Close()
	}
}

func (h *Hub) online(room string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	seen := map[string]bool{}
	var users []string

	for c := range h.rooms[room] {
		if !seen[c.user] {
			seen[c.user] = true
			users = append(users, c.user)
		}
	}
	return users
}

func (h *Hub) onlineAll() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	seen := map[string]bool{}
	var users []string
	for _, clients := range h.rooms {
		for c := range clients {
			if !seen[c.user] {
				seen[c.user] = true
				users = append(users, c.user)
			}
		}
	}
	sort.Strings(users)
	return users
}

func (h *Hub) broadcastAll(payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, clients := range h.rooms {
		for c := range clients {
			select {
			case c.send <- payload:
			default:
			}
		}
	}
}

type Event struct {
	Type    string      `json:"type"`
	Room    string      `json:"room"`
	Payload interface{} `json:"payload"`
}

type Message struct {
	ID        int64      `json:"id"`
	Username  string     `json:"username"`
	Text      string     `json:"text"`
	CreatedAt time.Time  `json:"created_at"`
	Reactions []Reaction `json:"reactions,omitempty"`
	Edited    bool       `json:"edited,omitempty"`
	Delivered bool       `json:"delivered,omitempty"`
	Read      bool       `json:"read,omitempty"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Mine  bool   `json:"mine"`
}

type MessagePage struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
}

type DirectConversation struct {
	ID                  int64  `json:"id"`
	PeerUsername        string `json:"peer_username"`
	DisappearingSeconds int    `json:"disappearing_seconds"`
	BlockedByMe         bool   `json:"blocked_by_me"`
	BlockedMe           bool   `json:"blocked_me"`
}

type Member struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type RoomRequest struct {
	RoomCode string `json:"room_code"`
	Password string `json:"password"`
}

type CreateRoomRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type RoleRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type KickRequest struct {
	Username string `json:"username"`
}

type WSMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func main() {
	_ = stdlib.Driver{}

	jwtSecret = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) < 32 {
		log.Fatal("JWT_SECRET must be at least 32 characters")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	var err error
	db, err = sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)

	if err = db.Ping(); err != nil {
		log.Fatal("PostgreSQL connection failed: ", err)
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://redis:6379"
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatal("Invalid REDIS_URL: ", err)
	}
	rdb = redis.NewClient(opt)
	if err = rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("Redis connection failed: ", err)
	}

	if err = migrate(); err != nil {
		log.Fatal("Database migration failed: ", err)
	}

	go redisSubscriber()
	go cleanupExpiredMessages()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", registerHandler)
	mux.HandleFunc("/api/login", loginHandler)
	mux.HandleFunc("/api/auth/providers", oauthProvidersHandler)
	mux.HandleFunc("/api/auth/exchange", oauthExchangeHandler)
	mux.HandleFunc("/auth/github", oauthAuthHandler("github"))
	mux.HandleFunc("/auth/github/callback", oauthCallbackHandler("github"))
	mux.HandleFunc("/auth/google", oauthAuthHandler("google"))
	mux.HandleFunc("/auth/google/callback", oauthCallbackHandler("google"))
	mux.HandleFunc("/api/logout", logoutHandler)
	mux.HandleFunc("/api/logout-all", logoutAllHandler)
	mux.HandleFunc("/api/ws-ticket", wsTicketHandler)
	mux.HandleFunc("/api/presence/ws-ticket", presenceWSTicketHandler)
	mux.HandleFunc("/api/rooms", roomsHandler)
	mux.HandleFunc("/api/rooms/leave", leaveRoomHandler)
	mux.HandleFunc("/api/rooms/create", createRoomHandler)
	mux.HandleFunc("/api/rooms/join", joinRoomHandler)
	mux.HandleFunc("/api/rooms/history", historyHandler)
	mux.HandleFunc("/api/messages/edit", editMessageHandler)
	mux.HandleFunc("/api/rooms/members", membersHandler)
	mux.HandleFunc("/api/rooms/role", roleHandler)
	mux.HandleFunc("/api/rooms/kick", kickHandler)
	mux.HandleFunc("/api/rooms/message/delete", deleteMessageHandler)
	mux.HandleFunc("/api/dms", directConversationsHandler)
	mux.HandleFunc("/api/dms/history", directHistoryHandler)
	mux.HandleFunc("/api/dms/delivered", directDeliveredHandler)
	mux.HandleFunc("/api/dms/read", directReadHandler)
	mux.HandleFunc("/api/dms/settings", directSettingsHandler)
	mux.HandleFunc("/api/dms/start", startDirectConversationHandler)
	mux.HandleFunc("/api/dms/ws-ticket", directWSTicketHandler)
	mux.HandleFunc("/api/dms/key", directKeyHandler)
	mux.HandleFunc("/api/dms/message/delete", deleteDirectMessageHandler)
	mux.HandleFunc("/api/users/block", blockUserHandler)
	mux.HandleFunc("/api/users/blocks", blockedUsersHandler)
	mux.HandleFunc("/api/users/report", reportUserHandler)
	mux.HandleFunc("/api/attachments", attachmentsHandler)
	mux.HandleFunc("/api/messages/reactions", reactionsHandler)
	mux.HandleFunc("/ws", websocketHandler)
	mux.HandleFunc("/health/live", liveHandler)
	mux.HandleFunc("/health/ready", readyHandler)
	mux.HandleFunc("/admin/test-results", adminTestResultsHandler)
	mux.HandleFunc("/admin/reports", adminReportsHandler)

	mux.Handle("/", http.FileServer(http.Dir("./web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Println("IPChat listening on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, securityHeaders(rateLimit(cors(mux)))))
}

func cleanupExpiredMessages() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		rows, err := db.Query(`DELETE FROM messages
			WHERE direct_conversation_id IS NOT NULL
				AND expires_at IS NOT NULL AND expires_at<=NOW()
			RETURNING id,direct_conversation_id`)
		if err != nil {
			log.Printf("could not delete expired direct messages: %v", err)
		} else {
			type expiredMessage struct {
				id             int64
				conversationID int64
			}
			var expired []expiredMessage
			for rows.Next() {
				var item expiredMessage
				if err = rows.Scan(&item.id, &item.conversationID); err != nil {
					log.Printf("could not read expired direct message: %v", err)
					break
				}
				expired = append(expired, item)
			}
			if err = rows.Err(); err != nil {
				log.Printf("could not read expired direct messages: %v", err)
			}
			if err = rows.Close(); err != nil {
				log.Printf("could not close expired message results: %v", err)
			}
			for _, item := range expired {
				publish(directChannel(item.conversationID), "message:deleted", map[string]int64{"id": item.id})
			}
		}
		if _, err = db.Exec(`DELETE FROM attachments WHERE expires_at IS NOT NULL AND expires_at<=NOW()`); err != nil {
			log.Printf("could not delete expired attachments: %v", err)
		}
		<-ticker.C
	}
}

// adminTestResultsHandler serves the generated test report only to the configured
// administrator. Configure ADMIN_USERNAME and ADMIN_PASSWORD_HASH (bcrypt hash).
func adminTestResultsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !requireAdmin(w, r) {
		return
	}
	data, err := os.ReadFile("test-results/index.html")
	if err != nil {
		http.Error(w, "test report has not been generated; run scripts/run-tests.ps1", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	adminUser := os.Getenv("ADMIN_USERNAME")
	adminHash := os.Getenv("ADMIN_PASSWORD_HASH")
	user, password, ok := r.BasicAuth()
	if adminUser == "" || adminHash == "" || !ok || user != adminUser ||
		bcrypt.CompareHashAndPassword([]byte(adminHash), []byte(password)) != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="IPChat Admin"`)
		http.Error(w, "administrator authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

func adminReportsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !requireAdmin(w, r) {
		return
	}
	rows, err := db.Query(`SELECT reports.id,reporter.username,reported.username,
		reports.reason,reports.created_at
		FROM user_reports reports
		JOIN users reporter ON reporter.id=reports.reporter_id
		JOIN users reported ON reported.id=reports.reported_id
		ORDER BY reports.created_at DESC LIMIT 200`)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load reports"})
		return
	}
	defer rows.Close()
	reports := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id int64
		var reporter, reported, reason string
		var createdAt time.Time
		if err = rows.Scan(&id, &reporter, &reported, &reason, &createdAt); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load reports"})
			return
		}
		reports = append(reports, map[string]interface{}{
			"id": id, "reporter": reporter, "reported": reported,
			"reason": reason, "created_at": createdAt,
		})
	}
	if err = rows.Err(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load reports"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, reports)
}

func migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			username VARCHAR(40) UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS user_blocks (
			blocker_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			blocked_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (blocker_id <> blocked_id),
			PRIMARY KEY(blocker_id,blocked_id)
		)`,
		`CREATE TABLE IF NOT EXISTS user_reports (
			id BIGSERIAL PRIMARY KEY,
			reporter_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			reported_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			reason VARCHAR(500) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (reporter_id <> reported_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_reports_created_at ON user_reports(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS rooms (
			room_code VARCHAR(16) PRIMARY KEY,
			room_name VARCHAR(80) NOT NULL DEFAULT 'Chat Room',
			owner_id BIGINT REFERENCES users(id),
			password_hash TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS room_name VARCHAR(80) NOT NULL DEFAULT 'Chat Room'`,
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS owner_id BIGINT REFERENCES users(id)`,
		`ALTER TABLE rooms ADD COLUMN IF NOT EXISTS e2ee_salt TEXT`,
		`CREATE TABLE IF NOT EXISTS room_members (
			room_code VARCHAR(16) NOT NULL REFERENCES rooms(room_code) ON DELETE CASCADE,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role VARCHAR(20) NOT NULL DEFAULT 'member'
				CHECK (role IN ('owner','maintainer','member')),
			muted_until TIMESTAMPTZ,
			joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY(room_code, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id BIGSERIAL PRIMARY KEY,
			room_code VARCHAR(16) NOT NULL REFERENCES rooms(room_code) ON DELETE CASCADE,
			username VARCHAR(40) NOT NULL,
			body TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_room_id
			ON messages(room_code, id DESC)`,
		`CREATE TABLE IF NOT EXISTS direct_conversations (
			id BIGSERIAL PRIMARY KEY,
			user_one BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			user_two BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (user_one < user_two),
			UNIQUE(user_one,user_two)
		)`,
		`ALTER TABLE direct_conversations ADD COLUMN IF NOT EXISTS disappearing_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ALTER COLUMN room_code DROP NOT NULL`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS direct_conversation_id BIGINT
			REFERENCES direct_conversations(id) ON DELETE CASCADE`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS user_id BIGINT REFERENCES users(id) ON DELETE SET NULL`,
		`DO $$ BEGIN
			ALTER TABLE messages ADD CONSTRAINT messages_one_conversation
			CHECK ((room_code IS NULL) <> (direct_conversation_id IS NULL));
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_messages_dm_id
			ON messages(direct_conversation_id,id DESC)`,
		`CREATE TABLE IF NOT EXISTS user_dm_keys (
			user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			public_key TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS message_reactions (
			message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			emoji VARCHAR(16) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY(message_id,user_id,emoji)
		)`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id BIGSERIAL PRIMARY KEY,
			room_code VARCHAR(16) REFERENCES rooms(room_code) ON DELETE CASCADE,
			direct_conversation_id BIGINT REFERENCES direct_conversations(id) ON DELETE CASCADE,
			uploader_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			data BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK ((room_code IS NULL) <> (direct_conversation_id IS NULL))
		)`,
		`CREATE TABLE IF NOT EXISTS user_sessions (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			expires_at TIMESTAMPTZ NOT NULL,
			revoked_at TIMESTAMPTZ
		)`,
		`ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL`,
		`CREATE TABLE IF NOT EXISTS oauth_identities (
			provider VARCHAR(20) NOT NULL,
			provider_user_id TEXT NOT NULL,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY(provider, provider_user_id),
			UNIQUE(provider, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_states (
			state_hash TEXT PRIMARY KEY,
			provider VARCHAR(20) NOT NULL,
			pkce_verifier TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_login_tickets (
			ticket_hash TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
			action VARCHAR(80) NOT NULL,
			room_code VARCHAR(16),
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS encryption_version INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS edited_at TIMESTAMPTZ`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS dm_read_at TIMESTAMPTZ`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS dm_delivered_at TIMESTAMPTZ`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ`,
		`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func originAllowed(origin, host string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return false
	}
	if strings.EqualFold(parsed.Host, host) {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if strings.TrimSpace(allowed) != "" && strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	return false
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !originAllowed(origin, r.Host) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(self), microphone=(self), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: https://media.tenor.com https://media.giphy.com https://i.giphy.com; media-src 'self' blob:; connect-src 'self' ws: wss:")
		next.ServeHTTP(w, r)
	})
}

var (
	rateMu      sync.Mutex
	rateBuckets = map[string][]time.Time{}
)

func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limited := r.URL.Path == "/api/login" || r.URL.Path == "/api/register" ||
			r.URL.Path == "/api/rooms/join" || r.URL.Path == "/api/ws-ticket"
		limited = limited || r.URL.Path == "/api/presence/ws-ticket" ||
			r.URL.Path == "/api/auth/exchange" ||
			r.URL.Path == "/auth/github" || r.URL.Path == "/auth/google" ||
			r.URL.Path == "/api/dms/start" || r.URL.Path == "/api/dms/ws-ticket" ||
			r.URL.Path == "/api/attachments" || r.URL.Path == "/api/users/block" ||
			r.URL.Path == "/api/users/report"
		if !limited {
			next.ServeHTTP(w, r)
			return
		}
		key := r.RemoteAddr + ":" + r.URL.Path
		now := time.Now()
		rateMu.Lock()
		events := rateBuckets[key]
		cutoff := now.Add(-time.Minute)
		filtered := events[:0]
		for _, event := range events {
			if event.After(cutoff) {
				filtered = append(filtered, event)
			}
		}
		if len(filtered) >= 30 {
			rateMu.Unlock()
			w.Header().Set("Retry-After", "60")
			respond(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		rateBuckets[key] = append(filtered, now)
		rateMu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func liveHandler(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]string{"status": "alive"})
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	if err := rdb.Ping(ctx).Err(); err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "ready"})
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	_, _ = db.Exec(`UPDATE user_sessions SET revoked_at=NOW()
		WHERE id=$1 AND user_id=$2`, c.SessionID, c.UserID)
	respond(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func logoutAllHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	_, _ = db.Exec(`UPDATE user_sessions SET revoked_at=NOW()
		WHERE user_id=$1 AND revoked_at IS NULL`, c.UserID)
	respond(w, http.StatusOK, map[string]string{"status": "all sessions revoked"})
}

func respond(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readJSON(r *http.Request, dst interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

func validUsername(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password)) == nil
	}
	var memory uint32
	var timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return hmac.Equal(got, want)
}

func makeToken(c Claims) (string, error) {
	c.Expires = time.Now().Add(24 * time.Hour).Unix()
	if c.SessionID == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		c.SessionID = base64.RawURLEncoding.EncodeToString(b)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature, nil
}

func parseToken(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid token")
	}
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte(parts[0]))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return nil, errors.New("invalid token signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err = json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Expires < time.Now().Unix() {
		return nil, errors.New("token expired")
	}
	return &c, nil
}

func requestClaims(r *http.Request) (*Claims, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, errors.New("missing bearer token")
	}
	c, err := parseToken(strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return nil, err
	}
	var revoked sql.NullTime
	var expires time.Time
	err = db.QueryRow(`SELECT expires_at, revoked_at FROM user_sessions
		WHERE id=$1 AND user_id=$2`, c.SessionID, c.UserID).Scan(&expires, &revoked)
	if err != nil || revoked.Valid || expires.Before(time.Now()) {
		return nil, errors.New("session revoked or expired")
	}
	return c, nil
}

func issueSession(id int64, username string) (string, error) {
	token, err := makeToken(Claims{UserID: id, Username: username})
	if err != nil {
		return "", err
	}
	claims, err := parseToken(token)
	if err != nil {
		return "", err
	}
	_, err = db.Exec(`INSERT INTO user_sessions(id,user_id,expires_at)
		VALUES($1,$2,$3)`, claims.SessionID, id, time.Unix(claims.Expires, 0))
	if err != nil {
		return "", err
	}
	return token, nil
}

type oauthProviderConfig struct {
	clientID     string
	clientSecret string
	authURL      string
	tokenURL     string
}

func oauthConfig(provider string) oauthProviderConfig {
	switch provider {
	case "github":
		return oauthProviderConfig{
			clientID: os.Getenv("GITHUB_CLIENT_ID"), clientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			authURL: "https://github.com/login/oauth/authorize", tokenURL: "https://github.com/login/oauth/access_token",
		}
	case "google":
		return oauthProviderConfig{
			clientID: os.Getenv("GOOGLE_CLIENT_ID"), clientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			authURL: "https://accounts.google.com/o/oauth2/v2/auth", tokenURL: "https://oauth2.googleapis.com/token",
		}
	default:
		return oauthProviderConfig{}
	}
}

func oauthProvidersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	providers := map[string]bool{}
	for _, provider := range []string{"github", "google"} {
		config := oauthConfig(provider)
		providers[provider] = config.clientID != "" && config.clientSecret != ""
	}
	respond(w, http.StatusOK, providers)
}

func oauthRedirectBase() (string, error) {
	base := strings.TrimRight(os.Getenv("OAUTH_REDIRECT_BASE"), "/")
	if base == "" {
		return "", errors.New("OAuth sign-in is not configured")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("OAuth redirect base is invalid")
	}
	return base, nil
}

func oauthCallbackURL(provider string) (string, error) {
	base, err := oauthRedirectBase()
	if err != nil {
		return "", err
	}
	return base + "/auth/" + provider + "/callback", nil
}

func randomOAuthValue() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func oauthValueHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthSecureCookie() bool {
	return strings.HasPrefix(strings.ToLower(os.Getenv("OAUTH_REDIRECT_BASE")), "https://")
}

func oauthAuthHandler(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		config := oauthConfig(provider)
		if config.clientID == "" || config.clientSecret == "" {
			http.Error(w, "sign-in provider is not configured", http.StatusServiceUnavailable)
			return
		}
		callback, err := oauthCallbackURL(provider)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		state, err := randomOAuthValue()
		if err != nil {
			http.Error(w, "could not start sign-in", http.StatusInternalServerError)
			return
		}
		verifier, err := randomOAuthValue()
		if err != nil {
			http.Error(w, "could not start sign-in", http.StatusInternalServerError)
			return
		}
		challengeHash := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
		if _, err = db.Exec(`DELETE FROM oauth_states WHERE expires_at<=NOW()`); err != nil {
			http.Error(w, "could not start sign-in", http.StatusInternalServerError)
			return
		}
		_, err = db.Exec(`INSERT INTO oauth_states(state_hash,provider,pkce_verifier,expires_at)
			VALUES($1,$2,$3,NOW() + INTERVAL '10 minutes')`,
			oauthValueHash(state), provider, verifier)
		if err != nil {
			http.Error(w, "could not start sign-in", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "ipchat_oauth_state", Value: state, Path: "/auth/",
			HttpOnly: true, Secure: oauthSecureCookie(), SameSite: http.SameSiteLaxMode,
			MaxAge: 600,
		})
		query := url.Values{
			"client_id": {config.clientID}, "redirect_uri": {callback},
			"response_type": {"code"}, "state": {state},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		}
		if provider == "github" {
			query.Set("scope", "read:user user:email")
		} else {
			query.Set("scope", "openid email profile")
		}
		http.Redirect(w, r, config.authURL+"?"+query.Encode(), http.StatusFound)
	}
}

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
}

type oauthProfile struct {
	ID            string
	Username      string
	Email         string
	EmailVerified bool
}

func exchangeOAuthCode(provider, code, verifier string) (string, error) {
	config := oauthConfig(provider)
	callback, err := oauthCallbackURL(provider)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"client_id": {config.clientID}, "client_secret": {config.clientSecret},
		"code": {code}, "redirect_uri": {callback}, "code_verifier": {verifier},
		"grant_type": {"authorization_code"},
	}
	request, err := http.NewRequest(http.MethodPost, config.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provider token endpoint returned %s", response.Status)
	}
	var result oauthTokenResponse
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Error != "" || result.AccessToken == "" {
		return "", errors.New("provider did not return an access token")
	}
	return result.AccessToken, nil
}

func oauthGetJSON(endpoint, accessToken string, dst interface{}) error {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "IPChat")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider profile endpoint returned %s", response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(dst)
}

func fetchOAuthProfile(provider, accessToken string) (oauthProfile, error) {
	if provider == "google" {
		var profile struct {
			Sub           string `json:"sub"`
			Name          string `json:"name"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
		}
		err := oauthGetJSON("https://openidconnect.googleapis.com/v1/userinfo", accessToken, &profile)
		return oauthProfile{ID: profile.Sub, Username: profile.Name, Email: profile.Email, EmailVerified: profile.EmailVerified}, err
	}
	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := oauthGetJSON("https://api.github.com/user", accessToken, &profile); err != nil {
		return oauthProfile{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := oauthGetJSON("https://api.github.com/user/emails", accessToken, &emails); err != nil {
		return oauthProfile{}, err
	}
	email := ""
	for _, candidate := range emails {
		if candidate.Primary && candidate.Verified {
			email = candidate.Email
			break
		}
	}
	if email == "" {
		for _, candidate := range emails {
			if candidate.Verified {
				email = candidate.Email
				break
			}
		}
	}
	name := profile.Login
	if profile.Name != "" {
		name = profile.Name
	}
	return oauthProfile{ID: fmt.Sprint(profile.ID), Username: name, Email: email, EmailVerified: email != ""}, nil
}

func oauthUsernameCandidate(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	candidate := strings.Trim(b.String(), "_-")
	if len(candidate) < 3 {
		candidate = "user_" + candidate
	}
	if len(candidate) > 40 {
		candidate = candidate[:40]
	}
	return candidate
}

func getOrCreateOAuthUser(provider string, profile oauthProfile) (int64, string, error) {
	if profile.ID == "" || !profile.EmailVerified || profile.Email == "" {
		return 0, "", errors.New("a verified email address is required")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	var userID int64
	var username string
	err = tx.QueryRow(`SELECT u.id,u.username FROM oauth_identities oi
		JOIN users u ON u.id=oi.user_id WHERE oi.provider=$1 AND oi.provider_user_id=$2`,
		provider, profile.ID).Scan(&userID, &username)
	if err == nil {
		return userID, username, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}

	base := oauthUsernameCandidate(profile.Username)
	for attempt := 0; attempt < 8; attempt++ {
		candidate := base
		if attempt > 0 {
			suffix, randomErr := randomOAuthValue()
			if randomErr != nil {
				return 0, "", randomErr
			}
			candidate = base[:min(len(base), 31)] + "_" + suffix[:8]
		}
		err = tx.QueryRow(`INSERT INTO users(username,password_hash) VALUES($1,NULL)
			ON CONFLICT(username) DO NOTHING RETURNING id`, candidate).Scan(&userID)
		if err == nil {
			username = candidate
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, "", err
		}
	}
	if username == "" {
		return 0, "", errors.New("could not allocate an account name")
	}
	if _, err = tx.Exec(`INSERT INTO oauth_identities(provider,provider_user_id,user_id)
		VALUES($1,$2,$3)`, provider, profile.ID, userID); err != nil {
		return 0, "", err
	}
	return userID, username, tx.Commit()
}

func oauthCallbackHandler(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			http.SetCookie(w, &http.Cookie{Name: "ipchat_oauth_state", Value: "", Path: "/auth/", HttpOnly: true, Secure: oauthSecureCookie(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
		}
		cookie, err := r.Cookie("ipchat_oauth_state")
		state := r.URL.Query().Get("state")
		if err != nil || state == "" || !hmac.Equal([]byte(cookie.Value), []byte(state)) {
			fail()
			return
		}
		var verifier string
		err = db.QueryRow(`DELETE FROM oauth_states WHERE state_hash=$1 AND provider=$2
			AND expires_at>NOW() RETURNING pkce_verifier`, oauthValueHash(state), provider).Scan(&verifier)
		if err != nil {
			fail()
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ipchat_oauth_state", Value: "", Path: "/auth/", HttpOnly: true, Secure: oauthSecureCookie(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
		if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
			redirectOAuthResult(w, r, "oauth_error", "provider_denied")
			return
		}
		accessToken, err := exchangeOAuthCode(provider, r.URL.Query().Get("code"), verifier)
		if err != nil {
			log.Printf("%s OAuth token exchange failed: %v", provider, err)
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
			return
		}
		profile, err := fetchOAuthProfile(provider, accessToken)
		if err != nil {
			log.Printf("%s OAuth profile lookup failed: %v", provider, err)
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
			return
		}
		userID, _, err := getOrCreateOAuthUser(provider, profile)
		if err != nil {
			log.Printf("%s OAuth account lookup failed: %v", provider, err)
			redirectOAuthResult(w, r, "oauth_error", "verified_email_required")
			return
		}
		ticket, err := randomOAuthValue()
		if err != nil {
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
			return
		}
		if _, err = db.Exec(`DELETE FROM oauth_login_tickets WHERE expires_at<=NOW()`); err != nil {
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
			return
		}
		_, err = db.Exec(`INSERT INTO oauth_login_tickets(ticket_hash,user_id,expires_at)
			VALUES($1,$2,NOW() + INTERVAL '2 minutes')`, oauthValueHash(ticket), userID)
		if err != nil {
			redirectOAuthResult(w, r, "oauth_error", "sign_in_failed")
			return
		}
		redirectOAuthResult(w, r, "oauth_ticket", ticket)
	}
}

func redirectOAuthResult(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, "/#"+url.QueryEscape(key)+"="+url.QueryEscape(value), http.StatusFound)
}

func oauthExchangeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		Ticket string `json:"ticket"`
	}
	if readJSON(r, &input) != nil || len(input.Ticket) < 32 || len(input.Ticket) > 128 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid sign-in ticket"})
		return
	}
	var userID int64
	err := db.QueryRow(`DELETE FROM oauth_login_tickets WHERE ticket_hash=$1
		AND expires_at>NOW() RETURNING user_id`, oauthValueHash(input.Ticket)).Scan(&userID)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "sign-in ticket is invalid or expired"})
		return
	}
	var username string
	if err = db.QueryRow(`SELECT username FROM users WHERE id=$1`, userID).Scan(&username); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load account"})
		return
	}
	token, err := issueSession(userID, username)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not create session"})
		return
	}
	respond(w, http.StatusOK, map[string]string{"token": token, "username": username})
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respond(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil || !validUsername(in.Username) ||
		len(in.Password) < 8 || len(in.Password) > 72 {
		respond(w, 400, map[string]string{"error": "Use a valid username and a password of 8–72 characters"})
		return
	}

	hash, err := hashPassword(in.Password)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not hash password"})
		return
	}
	var id int64
	err = db.QueryRow(`INSERT INTO users(username,password_hash)
		VALUES($1,$2) RETURNING id`, strings.TrimSpace(in.Username), hash).Scan(&id)
	if err != nil {
		respond(w, 409, map[string]string{"error": "username already exists"})
		return
	}
	token, err := issueSession(id, strings.TrimSpace(in.Username))
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	respond(w, 201, map[string]interface{}{"token": token, "username": in.Username})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if r.Method != http.MethodPost || readJSON(r, &in) != nil {
		respond(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	var id int64
	var hash string
	err := db.QueryRow(`SELECT id,password_hash FROM users WHERE username=$1`,
		strings.TrimSpace(in.Username)).Scan(&id, &hash)
	if err != nil || !verifyPassword(hash, in.Password) {
		respond(w, 401, map[string]string{"error": "incorrect username or password"})
		return
	}
	token, err := issueSession(id, in.Username)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	respond(w, 200, map[string]interface{}{"token": token, "username": in.Username})
}

func randomCode() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(base64.RawURLEncoding.EncodeToString(b)), nil
}

func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

func createRoomHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in CreateRoomRequest
	if readJSON(r, &in) != nil || len(in.Password) < 8 || len(in.Password) > 72 {
		respond(w, 400, map[string]string{"error": "Room password must be 8–72 characters"})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "Chat Room"
	}
	if len(name) > 80 {
		respond(w, 400, map[string]string{"error": "Room name is too long"})
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		respond(w, 500, map[string]string{"error": "password hashing failed"})
		return
	}
	salt, err := randomSalt()
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not initialize encryption"})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	defer tx.Rollback()

	// Lock the account row so concurrent room creation cannot bypass the 15-room cap.
	var userID int64
	if err = tx.QueryRow(`SELECT id FROM users WHERE id=$1 FOR UPDATE`, c.UserID).Scan(&userID); err != nil {
		respond(w, 401, map[string]string{"error": "account not found"})
		return
	}
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM rooms WHERE owner_id=$1`, c.UserID).Scan(&count); err != nil {
		respond(w, 500, map[string]string{"error": "could not check room limit"})
		return
	}
	if count >= 15 {
		respond(w, 409, map[string]string{"error": "You already own the maximum of 15 rooms"})
		return
	}

	code, err := randomCode()
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not generate room code"})
		return
	}
	_, err = tx.Exec(`INSERT INTO rooms(room_code,room_name,owner_id,password_hash,e2ee_salt)
		VALUES($1,$2,$3,$4,$5)`, code, name, c.UserID, hash, salt)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not create room"})
		return
	}
	_, err = tx.Exec(`INSERT INTO room_members(room_code,user_id,role)
		VALUES($1,$2,'owner')`, code, c.UserID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not assign room owner"})
		return
	}
	if err = tx.Commit(); err != nil {
		respond(w, 500, map[string]string{"error": "could not save room"})
		return
	}
	respond(w, 201, map[string]interface{}{
		"room_code": code, "room_name": name, "role": "owner", "e2ee_salt": salt,
	})
}

func joinRoomHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in RoomRequest
	if readJSON(r, &in) != nil {
		respond(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	var hash, name, salt string
	err = db.QueryRow(`SELECT password_hash,room_name,e2ee_salt FROM rooms WHERE room_code=$1`,
		strings.ToUpper(strings.TrimSpace(in.RoomCode))).Scan(&hash, &name, &salt)
	if err != nil || !verifyPassword(hash, in.Password) {
		respond(w, 401, map[string]string{"error": "room code or password is incorrect"})
		return
	}
	if salt == "" {
		salt, err = randomSalt()
		if err != nil {
			respond(w, 500, map[string]string{"error": "could not initialize encryption"})
			return
		}
		_, _ = db.Exec(`UPDATE rooms SET e2ee_salt=$1 WHERE room_code=$2`, salt,
			strings.ToUpper(strings.TrimSpace(in.RoomCode)))
	}
	_, err = db.Exec(`INSERT INTO room_members(room_code,user_id,role)
		VALUES($1,$2,'member') ON CONFLICT(room_code,user_id) DO NOTHING`,
		strings.ToUpper(strings.TrimSpace(in.RoomCode)), c.UserID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not join room"})
		return
	}
	var role string
	_ = db.QueryRow(`SELECT role FROM room_members WHERE room_code=$1 AND user_id=$2`,
		strings.ToUpper(strings.TrimSpace(in.RoomCode)), c.UserID).Scan(&role)
	respond(w, 200, map[string]interface{}{
		"room_code": strings.ToUpper(strings.TrimSpace(in.RoomCode)),
		"room_name": name, "role": role, "e2ee_salt": salt,
	})
}

func roomsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rows, err := db.Query(`SELECT r.room_code,r.room_name,rm.role
		FROM room_members rm JOIN rooms r ON r.room_code=rm.room_code
		WHERE rm.user_id=$1 ORDER BY rm.joined_at DESC`, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load rooms"})
		return
	}
	defer rows.Close()
	type roomSummary struct {
		Code string `json:"room_code"`
		Name string `json:"room_name"`
		Role string `json:"role"`
	}
	var rooms []roomSummary
	for rows.Next() {
		var room roomSummary
		if rows.Scan(&room.Code, &room.Name, &room.Role) == nil {
			rooms = append(rooms, room)
		}
	}
	if rooms == nil {
		rooms = []roomSummary{}
	}
	respond(w, http.StatusOK, rooms)
}

func leaveRoomHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var in struct {
		RoomCode string `json:"room_code"`
	}
	if readJSON(r, &in) != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	room := strings.ToUpper(strings.TrimSpace(in.RoomCode))
	role, err := memberRole(room, c.UserID)
	if err != nil {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a member"})
		return
	}
	if role == "owner" {
		respond(w, http.StatusConflict, map[string]string{"error": "owners must transfer or delete the room"})
		return
	}
	_, err = db.Exec(`DELETE FROM room_members WHERE room_code=$1 AND user_id=$2`, room, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not leave room"})
		return
	}
	publish(room, "member:kicked", map[string]string{"username": c.Username})
	respond(w, http.StatusOK, map[string]string{"status": "left room"})
}

func memberRole(room string, userID int64) (string, error) {
	var role string
	err := db.QueryRow(`SELECT role FROM room_members WHERE room_code=$1 AND user_id=$2`,
		room, userID).Scan(&role)
	return role, err
}

func parseHistoryPage(r *http.Request) (int64, int, error) {
	beforeID := int64(0)
	if raw := r.URL.Query().Get("before_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, errors.New("invalid history cursor")
		}
		beforeID = parsed
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return 0, 0, errors.New("invalid history page size")
		}
		if parsed < limit {
			limit = parsed
		}
	}
	return beforeID, limit, nil
}

func historyHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	beforeID, limit, err := parseHistoryPage(r)
	if err != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	room := strings.ToUpper(r.URL.Query().Get("room"))
	if _, err := memberRole(room, c.UserID); err != nil {
		respond(w, 403, map[string]string{"error": "you are not a member of this room"})
		return
	}
	rows, err := db.Query(`SELECT m.id,m.username,m.body,m.created_at,
		COALESCE((SELECT json_agg(json_build_object('emoji',r.emoji,'count',r.reaction_count,'mine',r.mine))
			FROM (SELECT emoji,COUNT(*)::int AS reaction_count,BOOL_OR(user_id=$3) AS mine
				FROM message_reactions WHERE message_id=m.id GROUP BY emoji) r),'[]'::json),
		m.edited_at IS NOT NULL
		FROM messages m WHERE m.room_code=$1 AND ($2=0 OR m.id<$2)
		ORDER BY m.id DESC LIMIT $4`, room, beforeID, c.UserID, limit+1)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not load messages"})
		return
	}
	defer rows.Close()
	messages := make([]Message, 0, limit+1)
	for rows.Next() {
		var m Message
		var reactions []byte
		if err = rows.Scan(&m.ID, &m.Username, &m.Text, &m.CreatedAt, &reactions, &m.Edited); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load messages"})
			return
		}
		if err = json.Unmarshal(reactions, &m.Reactions); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load message reactions"})
			return
		}
		messages = append(messages, m)
	}
	if err = rows.Err(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load messages"})
		return
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	// Return oldest-to-newest for display.
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	respond(w, 200, MessagePage{Messages: messages, HasMore: hasMore})
}

func membersHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	room := strings.ToUpper(r.URL.Query().Get("room"))
	if _, err = memberRole(room, c.UserID); err != nil {
		respond(w, 403, map[string]string{"error": "not a room member"})
		return
	}
	rows, err := db.Query(`SELECT u.username,rm.role FROM room_members rm
		JOIN users u ON u.id=rm.user_id WHERE rm.room_code=$1 ORDER BY rm.joined_at`,
		room)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not load members"})
		return
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var m Member
		if rows.Scan(&m.Username, &m.Role) == nil {
			members = append(members, m)
		}
	}
	if members == nil {
		members = []Member{}
	}
	respond(w, 200, members)
}

func roleHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in struct {
		RoomRequest
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if readJSON(r, &in) != nil || (in.Role != "maintainer" && in.Role != "member") {
		respond(w, 400, map[string]string{"error": "role must be maintainer or member"})
		return
	}
	room := strings.ToUpper(in.RoomCode)
	actorRole, err := memberRole(room, c.UserID)
	if err != nil || actorRole != "owner" {
		respond(w, 403, map[string]string{"error": "only the owner can change roles"})
		return
	}
	var targetID int64
	err = db.QueryRow(`SELECT id FROM users WHERE username=$1`, in.Username).Scan(&targetID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "user not found"})
		return
	}
	res, err := db.Exec(`UPDATE room_members SET role=$1
		WHERE room_code=$2 AND user_id=$3 AND role <> 'owner'`,
		in.Role, room, targetID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not update role"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		respond(w, 404, map[string]string{"error": "member not found or is the owner"})
		return
	}
	_, _ = db.Exec(`INSERT INTO audit_logs(user_id,action,room_code)
		VALUES($1,$2,$3)`, c.UserID, "room.role.updated:"+in.Role, room)
	respond(w, 200, map[string]string{"status": "role updated"})
}

func kickHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in struct {
		RoomRequest
		Username string `json:"username"`
	}
	if readJSON(r, &in) != nil {
		respond(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	room := strings.ToUpper(in.RoomCode)
	actorRole, err := memberRole(room, c.UserID)
	if err != nil || (actorRole != "owner" && actorRole != "maintainer") {
		respond(w, 403, map[string]string{"error": "insufficient permissions"})
		return
	}
	var targetID int64
	var targetRole string
	err = db.QueryRow(`SELECT u.id,rm.role FROM users u
		JOIN room_members rm ON rm.user_id=u.id
		WHERE u.username=$1 AND rm.room_code=$2`,
		in.Username, room).Scan(&targetID, &targetRole)
	if err != nil {
		respond(w, 404, map[string]string{"error": "member not found"})
		return
	}
	if targetRole == "owner" || (targetRole == "maintainer" && actorRole != "owner") {
		respond(w, 403, map[string]string{"error": "you cannot remove this member"})
		return
	}
	_, err = db.Exec(`DELETE FROM room_members WHERE room_code=$1 AND user_id=$2`,
		room, targetID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not remove member"})
		return
	}
	_, _ = db.Exec(`INSERT INTO audit_logs(user_id,action,room_code)
		VALUES($1,'room.member.kicked',$2)`, c.UserID, room)
	publish(room, "member:kicked", map[string]string{"username": in.Username})
	respond(w, 200, map[string]string{"status": "member removed"})
}

func editMessageHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var input struct {
		RoomCode       string `json:"room_code"`
		ConversationID int64  `json:"conversation_id"`
		MessageID      int64  `json:"message_id"`
		Text           string `json:"text"`
	}
	if readJSON(r, &input) != nil || input.MessageID <= 0 ||
		(len(strings.TrimSpace(input.Text)) == 0) || len(input.Text) > 24<<10 ||
		(strings.TrimSpace(input.RoomCode) != "" && input.ConversationID != 0) ||
		(strings.TrimSpace(input.RoomCode) == "" && input.ConversationID <= 0) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid message edit"})
		return
	}
	roomCode := strings.TrimSpace(input.RoomCode)

	var message Message
	channel := ""
	if roomCode != "" {
		room := strings.ToUpper(roomCode)
		if _, err = memberRole(room, c.UserID); err != nil {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a room member"})
			return
		}
		err = db.QueryRow(`UPDATE messages SET body=$1,edited_at=NOW()
			WHERE id=$2 AND room_code=$3
				AND (user_id=$4 OR (user_id IS NULL AND username=$5))
			RETURNING id,username,created_at,edited_at IS NOT NULL`,
			input.Text, input.MessageID, room, c.UserID, c.Username).
			Scan(&message.ID, &message.Username, &message.CreatedAt, &message.Edited)
		channel = room
	} else {
		member, memberErr := isDirectMember(input.ConversationID, c.UserID)
		if memberErr != nil || !member {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
			return
		}
		err = db.QueryRow(`UPDATE messages SET body=$1,edited_at=NOW()
			WHERE id=$2 AND direct_conversation_id=$3 AND user_id=$4
				AND (expires_at IS NULL OR expires_at>NOW())
			RETURNING id,username,created_at,edited_at IS NOT NULL`,
			input.Text, input.MessageID, input.ConversationID, c.UserID).
			Scan(&message.ID, &message.Username, &message.CreatedAt, &message.Edited)
		channel = directChannel(input.ConversationID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, http.StatusNotFound, map[string]string{"error": "message not found or not yours"})
		return
	}
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not edit message"})
		return
	}
	message.Text = input.Text
	publish(channel, "message:updated", message)
	respond(w, http.StatusOK, message)
}

func deleteMessageHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var in struct {
		RoomCode  string `json:"room_code"`
		MessageID int64  `json:"message_id"`
	}
	if readJSON(r, &in) != nil {
		respond(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	room := strings.ToUpper(in.RoomCode)
	role, err := memberRole(room, c.UserID)
	if err != nil {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a room member"})
		return
	}
	canModerate := role == "owner" || role == "maintainer"
	res, err := db.Exec(`DELETE FROM messages WHERE id=$1 AND room_code=$2
		AND (user_id=$3 OR (user_id IS NULL AND username=$5) OR $4)`,
		in.MessageID, room, c.UserID, canModerate, c.Username)
	if err != nil {
		respond(w, 500, map[string]string{"error": "could not delete message"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		respond(w, 404, map[string]string{"error": "message not found"})
		return
	}
	publish(room, "message:deleted", map[string]int64{"id": in.MessageID})
	respond(w, 200, map[string]string{"status": "message deleted"})
}

func directConversationsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	rows, err := db.Query(`SELECT dc.id,peer.username,dc.disappearing_seconds,
		EXISTS(SELECT 1 FROM user_blocks WHERE blocker_id=$1 AND blocked_id=peer.id),
		EXISTS(SELECT 1 FROM user_blocks WHERE blocker_id=peer.id AND blocked_id=$1)
		FROM direct_conversations dc
		JOIN users peer ON peer.id=CASE WHEN dc.user_one=$1 THEN dc.user_two ELSE dc.user_one END
		LEFT JOIN LATERAL (SELECT created_at FROM messages
			WHERE direct_conversation_id=dc.id ORDER BY id DESC LIMIT 1) latest ON TRUE
		WHERE dc.user_one=$1 OR dc.user_two=$1
		ORDER BY COALESCE(latest.created_at,dc.created_at) DESC`, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load direct chats"})
		return
	}
	defer rows.Close()
	conversations := []DirectConversation{}
	for rows.Next() {
		var conversation DirectConversation
		if err = rows.Scan(&conversation.ID, &conversation.PeerUsername,
			&conversation.DisappearingSeconds, &conversation.BlockedByMe, &conversation.BlockedMe); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load direct chats"})
			return
		}
		conversations = append(conversations, conversation)
	}
	if err = rows.Err(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load direct chats"})
		return
	}
	respond(w, http.StatusOK, conversations)
}

func startDirectConversationHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		Username string `json:"username"`
	}
	if readJSON(r, &input) != nil || !validUsername(input.Username) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "enter a valid username"})
		return
	}
	var peerID int64
	var peerUsername string
	if err = db.QueryRow(`SELECT id,username FROM users WHERE username=$1`,
		strings.TrimSpace(input.Username)).Scan(&peerID, &peerUsername); err != nil {
		respond(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	if peerID == c.UserID {
		respond(w, http.StatusBadRequest, map[string]string{"error": "you cannot start a direct chat with yourself"})
		return
	}
	var blocked bool
	if err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM user_blocks
		WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`,
		c.UserID, peerID).Scan(&blocked); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not check direct message permissions"})
		return
	}
	if blocked {
		respond(w, http.StatusForbidden, map[string]string{"error": "this user is blocked"})
		return
	}
	first, second := c.UserID, peerID
	if first > second {
		first, second = second, first
	}
	var conversationID int64
	err = db.QueryRow(`INSERT INTO direct_conversations(user_one,user_two)
		VALUES($1,$2) ON CONFLICT(user_one,user_two)
		DO UPDATE SET user_one=EXCLUDED.user_one RETURNING id`,
		first, second).Scan(&conversationID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not start direct chat"})
		return
	}
	var disappearingSeconds int
	if err = db.QueryRow(`SELECT disappearing_seconds FROM direct_conversations WHERE id=$1`,
		conversationID).Scan(&disappearingSeconds); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load direct chat settings"})
		return
	}
	respond(w, http.StatusOK, DirectConversation{
		ID: conversationID, PeerUsername: peerUsername, DisappearingSeconds: disappearingSeconds,
	})
}

func blockUserHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	var input struct {
		Username string `json:"username"`
		Blocked  bool   `json:"blocked"`
	}
	if readJSON(r, &input) != nil || !validUsername(input.Username) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "enter a valid username"})
		return
	}
	var targetID int64
	if err = db.QueryRow(`SELECT id FROM users WHERE username=$1`,
		strings.TrimSpace(input.Username)).Scan(&targetID); errors.Is(err, sql.ErrNoRows) {
		respond(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	} else if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load user"})
		return
	}
	if targetID == c.UserID {
		respond(w, http.StatusBadRequest, map[string]string{"error": "you cannot block yourself"})
		return
	}
	if input.Blocked {
		_, err = db.Exec(`INSERT INTO user_blocks(blocker_id,blocked_id) VALUES($1,$2)
			ON CONFLICT(blocker_id,blocked_id) DO NOTHING`, c.UserID, targetID)
	} else {
		_, err = db.Exec(`DELETE FROM user_blocks WHERE blocker_id=$1 AND blocked_id=$2`,
			c.UserID, targetID)
	}
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not update block"})
		return
	}
	var conversationID int64
	err = db.QueryRow(`SELECT id FROM direct_conversations
		WHERE (user_one=$1 AND user_two=$2) OR (user_one=$2 AND user_two=$1)`,
		c.UserID, targetID).Scan(&conversationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not notify direct chat"})
		return
	}
	if err == nil {
		publish(directChannel(conversationID), "block:update", map[string]interface{}{
			"blocker": c.Username, "blocked": strings.TrimSpace(input.Username), "is_blocked": input.Blocked,
		})
	}
	respond(w, http.StatusOK, map[string]bool{"blocked": input.Blocked})
}

func blockedUsersHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	rows, err := db.Query(`SELECT users.username FROM user_blocks
		JOIN users ON users.id=user_blocks.blocked_id
		WHERE user_blocks.blocker_id=$1 ORDER BY users.username`, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load blocked users"})
		return
	}
	defer rows.Close()
	usernames := make([]string, 0)
	for rows.Next() {
		var blockedUsername string
		if err = rows.Scan(&blockedUsername); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load blocked users"})
			return
		}
		usernames = append(usernames, blockedUsername)
	}
	if err = rows.Err(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load blocked users"})
		return
	}
	respond(w, http.StatusOK, usernames)
}

func reportUserHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	var input struct {
		Username string `json:"username"`
		Reason   string `json:"reason"`
	}
	if readJSON(r, &input) != nil || !validUsername(input.Username) || !validReportReason(input.Reason) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "provide a username and a reason of 5 to 500 characters"})
		return
	}
	var targetID int64
	if err = db.QueryRow(`SELECT id FROM users WHERE username=$1`,
		strings.TrimSpace(input.Username)).Scan(&targetID); errors.Is(err, sql.ErrNoRows) {
		respond(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	} else if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load user"})
		return
	}
	if targetID == c.UserID {
		respond(w, http.StatusBadRequest, map[string]string{"error": "you cannot report yourself"})
		return
	}
	if _, err = db.Exec(`INSERT INTO user_reports(reporter_id,reported_id,reason)
		VALUES($1,$2,$3)`, c.UserID, targetID, strings.TrimSpace(input.Reason)); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not submit report"})
		return
	}
	respond(w, http.StatusCreated, map[string]string{
		"status": "report submitted", "privacy": "Encrypted message content is not included.",
	})
}

func validReportReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	length := utf8.RuneCountInString(reason)
	return length >= 5 && length <= 500
}

func isDirectMember(conversationID, userID int64) (bool, error) {
	var member bool
	err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM direct_conversations
		WHERE id=$1 AND (user_one=$2 OR user_two=$2))`, conversationID, userID).Scan(&member)
	return member, err
}

func isDirectBlocked(conversationID, userID int64) (bool, error) {
	var blocked bool
	err := db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM direct_conversations dc JOIN user_blocks b
			ON (b.blocker_id=dc.user_one AND b.blocked_id=dc.user_two)
			OR (b.blocker_id=dc.user_two AND b.blocked_id=dc.user_one)
		WHERE dc.id=$1 AND (dc.user_one=$2 OR dc.user_two=$2))`,
		conversationID, userID).Scan(&blocked)
	return blocked, err
}

func directChannel(conversationID int64) string {
	return "dm:" + strconv.FormatInt(conversationID, 10)
}

func sendClientEvent(client *Client, eventType string, payload interface{}) {
	data, err := json.Marshal(Event{Type: eventType, Room: client.room, Payload: payload})
	if err != nil {
		return
	}
	select {
	case client.send <- data:
	default:
	}
}

func directHistoryHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	beforeID, limit, err := parseHistoryPage(r)
	if err != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	conversationID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || conversationID <= 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid direct chat"})
		return
	}
	member, err := isDirectMember(conversationID, c.UserID)
	if err != nil || !member {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	rows, err := db.Query(`SELECT m.id,m.username,m.body,m.created_at,
		COALESCE((SELECT json_agg(json_build_object('emoji',r.emoji,'count',r.reaction_count,'mine',r.mine))
			FROM (SELECT emoji,COUNT(*)::int AS reaction_count,BOOL_OR(user_id=$3) AS mine
				FROM message_reactions WHERE message_id=m.id GROUP BY emoji) r),'[]'::json),
		m.edited_at IS NOT NULL,COALESCE(m.dm_delivered_at IS NOT NULL,FALSE),
		COALESCE(m.dm_read_at IS NOT NULL,FALSE)
		FROM messages m WHERE m.direct_conversation_id=$1 AND ($2=0 OR m.id<$2)
			AND (m.expires_at IS NULL OR m.expires_at>NOW())
		ORDER BY m.id DESC LIMIT $4`, conversationID, beforeID, c.UserID, limit+1)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load messages"})
		return
	}
	defer rows.Close()
	messages := make([]Message, 0, limit+1)
	for rows.Next() {
		var message Message
		var reactions []byte
		if err = rows.Scan(&message.ID, &message.Username, &message.Text, &message.CreatedAt,
			&reactions, &message.Edited, &message.Delivered, &message.Read); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load messages"})
			return
		}
		if err = json.Unmarshal(reactions, &message.Reactions); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load message reactions"})
			return
		}
		messages = append(messages, message)
	}
	if err = rows.Err(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load messages"})
		return
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	respond(w, http.StatusOK, MessagePage{Messages: messages, HasMore: hasMore})
}

func validDisappearingSeconds(seconds int) bool {
	switch seconds {
	case 0, 86400, 604800, 7776000:
		return true
	default:
		return false
	}
}

func directSettingsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPut {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		ConversationID      int64 `json:"conversation_id"`
		DisappearingSeconds int   `json:"disappearing_seconds"`
	}
	if readJSON(r, &input) != nil || input.ConversationID <= 0 ||
		!validDisappearingSeconds(input.DisappearingSeconds) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid direct chat settings"})
		return
	}
	result, err := db.Exec(`UPDATE direct_conversations SET disappearing_seconds=$1
		WHERE id=$2 AND (user_one=$3 OR user_two=$3)`,
		input.DisappearingSeconds, input.ConversationID, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not update direct chat settings"})
		return
	}
	updated, err := result.RowsAffected()
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not update direct chat settings"})
		return
	}
	if updated == 0 {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	respond(w, http.StatusOK, map[string]int{"disappearing_seconds": input.DisappearingSeconds})
}

func directDeliveredHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		ConversationID int64 `json:"conversation_id"`
		UpToMessageID  int64 `json:"up_to_message_id"`
	}
	if readJSON(r, &input) != nil || input.ConversationID <= 0 || input.UpToMessageID <= 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid delivery receipt"})
		return
	}
	member, err := isDirectMember(input.ConversationID, c.UserID)
	if err != nil || !member {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	rows, err := db.Query(`UPDATE messages SET dm_delivered_at=NOW()
		WHERE direct_conversation_id=$1 AND username<>$2 AND dm_delivered_at IS NULL
			AND (expires_at IS NULL OR expires_at>NOW()) AND id<=$3
		RETURNING id`, input.ConversationID, c.Username, input.UpToMessageID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages delivered"})
		return
	}
	deliveredIDs := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages delivered"})
			return
		}
		deliveredIDs = append(deliveredIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages delivered"})
		return
	}
	if err = rows.Close(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages delivered"})
		return
	}
	if len(deliveredIDs) > 0 {
		publish(directChannel(input.ConversationID), "receipts:delivery", map[string][]int64{
			"message_ids": deliveredIDs,
		})
	}
	respond(w, http.StatusOK, map[string][]int64{"message_ids": deliveredIDs})
}

func directReadHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		ConversationID int64 `json:"conversation_id"`
		UpToMessageID  int64 `json:"up_to_message_id"`
	}
	if readJSON(r, &input) != nil || input.ConversationID <= 0 || input.UpToMessageID < 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid read receipt"})
		return
	}
	member, err := isDirectMember(input.ConversationID, c.UserID)
	if err != nil || !member {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	rows, err := db.Query(`UPDATE messages SET dm_read_at=NOW(),dm_delivered_at=COALESCE(dm_delivered_at,NOW())
		WHERE direct_conversation_id=$1 AND username<>$2 AND dm_read_at IS NULL
			AND (expires_at IS NULL OR expires_at>NOW())
			AND ($3=0 OR id<=$3) RETURNING id`,
		input.ConversationID, c.Username, input.UpToMessageID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages read"})
		return
	}
	var readIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages read"})
			return
		}
		readIDs = append(readIDs, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages read"})
		return
	}
	if err = rows.Close(); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not mark messages read"})
		return
	}
	if len(readIDs) > 0 {
		publish(directChannel(input.ConversationID), "receipts:update", map[string]interface{}{
			"message_ids": readIDs, "reader": c.Username,
		})
	}
	respond(w, http.StatusOK, map[string][]int64{"message_ids": readIDs})
}

func directKeyHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method == http.MethodPut {
		var input struct {
			PublicKey string `json:"public_key"`
		}
		if readJSON(r, &input) != nil || !validDMKey(input.PublicKey) {
			respond(w, http.StatusBadRequest, map[string]string{"error": "invalid public encryption key"})
			return
		}
		_, err = db.Exec(`INSERT INTO user_dm_keys(user_id,public_key) VALUES($1,$2)
			ON CONFLICT(user_id) DO UPDATE SET public_key=EXCLUDED.public_key,updated_at=NOW()`,
			c.UserID, input.PublicKey)
		if err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not save encryption key"})
			return
		}
		respond(w, http.StatusOK, map[string]string{"status": "key saved"})
		return
	}
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if r.URL.Query().Get("dm") == "" {
		var ownKey sql.NullString
		if err = db.QueryRow(`SELECT public_key FROM user_dm_keys WHERE user_id=$1`, c.UserID).Scan(&ownKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load encryption key"})
			return
		}
		respond(w, http.StatusOK, map[string]interface{}{"own_public_key": ownKey.String})
		return
	}
	conversationID, err := strconv.ParseInt(r.URL.Query().Get("dm"), 10, 64)
	if err != nil || conversationID <= 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid direct chat"})
		return
	}
	var peerUsername, peerKey, ownKey string
	err = db.QueryRow(`SELECT peer.username,peer_key.public_key,own_key.public_key
		FROM direct_conversations dc
		JOIN users peer ON peer.id=CASE WHEN dc.user_one=$2 THEN dc.user_two ELSE dc.user_one END
		LEFT JOIN user_dm_keys peer_key ON peer_key.user_id=peer.id
		LEFT JOIN user_dm_keys own_key ON own_key.user_id=$2
		WHERE dc.id=$1 AND (dc.user_one=$2 OR dc.user_two=$2)`,
		conversationID, c.UserID).Scan(&peerUsername, &peerKey, &ownKey)
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load encryption keys"})
		return
	}
	if peerKey == "" || ownKey == "" {
		respond(w, http.StatusConflict, map[string]string{"error": "both participants need to open direct messages to initialize encryption"})
		return
	}
	respond(w, http.StatusOK, map[string]string{
		"peer_username": peerUsername, "peer_public_key": peerKey, "own_public_key": ownKey,
	})
}

func validDMKey(encoded string) bool {
	if len(encoded) < 300 || len(encoded) > 4096 {
		return false
	}
	var key struct {
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	if json.Unmarshal([]byte(encoded), &key) != nil || key.Kty != "RSA" || key.N == "" || key.E == "" {
		return false
	}
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	return err == nil && len(modulus) >= 256
}

func deleteDirectMessageHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		ConversationID int64 `json:"conversation_id"`
		MessageID      int64 `json:"message_id"`
	}
	if readJSON(r, &input) != nil || input.ConversationID <= 0 || input.MessageID <= 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	member, err := isDirectMember(input.ConversationID, c.UserID)
	if err != nil || !member {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	result, err := db.Exec(`DELETE FROM messages WHERE id=$1 AND direct_conversation_id=$2 AND user_id=$3`,
		input.MessageID, input.ConversationID, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not delete message"})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		respond(w, http.StatusNotFound, map[string]string{"error": "message not found or not yours"})
		return
	}
	publish(directChannel(input.ConversationID), "message:deleted", map[string]int64{"id": input.MessageID})
	respond(w, http.StatusOK, map[string]string{"status": "message deleted"})
}

const maxAttachmentBytes = (10 << 20) + 16

func attachmentsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	room := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("room")))
	directValue := r.URL.Query().Get("dm")
	if (room == "") == (directValue == "") {
		respond(w, http.StatusBadRequest, map[string]string{"error": "specify exactly one room or direct chat"})
		return
	}
	directID := int64(0)
	if directValue != "" {
		directID, err = strconv.ParseInt(directValue, 10, 64)
		if err != nil || directID <= 0 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "invalid direct chat"})
			return
		}
	}
	isDirect := directValue != ""
	if room != "" {
		if _, err = memberRole(room, c.UserID); err != nil {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a room member"})
			return
		}
	} else {
		member, memberErr := isDirectMember(directID, c.UserID)
		if memberErr != nil || !member {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
			return
		}
		if r.Method == http.MethodPost {
			blocked, blockErr := isDirectBlocked(directID, c.UserID)
			if blockErr != nil {
				respond(w, http.StatusInternalServerError, map[string]string{"error": "could not check direct message permissions"})
				return
			}
			if blocked {
				respond(w, http.StatusForbidden, map[string]string{"error": "you cannot send attachments while either user has blocked the other"})
				return
			}
		}
	}
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes)
		data, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			respond(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "attachment exceeds the 10 MB limit"})
			return
		}
		if len(data) == 0 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "attachment is empty"})
			return
		}
		var id int64
		if room != "" {
			err = db.QueryRow(`INSERT INTO attachments(room_code,uploader_id,data)
				VALUES($1,$2,$3) RETURNING id`, room, c.UserID, data).Scan(&id)
		} else {
			err = db.QueryRow(`INSERT INTO attachments(direct_conversation_id,uploader_id,data,expires_at)
				SELECT $1,$2,$3,
					CASE WHEN disappearing_seconds>0
						THEN NOW()+disappearing_seconds*INTERVAL '1 second' ELSE NULL END
				FROM direct_conversations WHERE id=$1 RETURNING id`, directID, c.UserID, data).Scan(&id)
		}
		if err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not save attachment"})
			return
		}
		respond(w, http.StatusCreated, map[string]int64{"id": id})
	case http.MethodGet:
		id, parseErr := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if parseErr != nil || id <= 0 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "invalid attachment"})
			return
		}
		var data []byte
		var attachmentRoom sql.NullString
		var attachmentDM sql.NullInt64
		err = db.QueryRow(`SELECT data,room_code,direct_conversation_id FROM attachments
			WHERE id=$1 AND (expires_at IS NULL OR expires_at>NOW())`, id).
			Scan(&data, &attachmentRoom, &attachmentDM)
		if err != nil {
			respond(w, http.StatusNotFound, map[string]string{"error": "attachment not found"})
			return
		}
		if room != "" && (!attachmentRoom.Valid || attachmentRoom.String != room) ||
			isDirect && (!attachmentDM.Valid || attachmentDM.Int64 != directID) {
			respond(w, http.StatusNotFound, map[string]string{"error": "attachment not found"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

var allowedReactions = map[string]bool{
	"👍": true, "❤️": true, "😂": true, "😮": true, "😢": true,
	"🙏": true, "🔥": true, "🎉": true, "👏": true, "👎": true,
}

func reactionsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		MessageID int64  `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if readJSON(r, &input) != nil || input.MessageID <= 0 || !allowedReactions[input.Emoji] {
		respond(w, http.StatusBadRequest, map[string]string{"error": "unsupported reaction"})
		return
	}
	var room sql.NullString
	var directID sql.NullInt64
	err = db.QueryRow(`SELECT room_code,direct_conversation_id FROM messages WHERE id=$1`, input.MessageID).
		Scan(&room, &directID)
	if err != nil {
		respond(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		return
	}
	channel := ""
	if room.Valid {
		if _, err = memberRole(room.String, c.UserID); err != nil {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a room member"})
			return
		}
		channel = room.String
	} else if directID.Valid {
		member, memberErr := isDirectMember(directID.Int64, c.UserID)
		if memberErr != nil || !member {
			respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
			return
		}
		channel = directChannel(directID.Int64)
	} else {
		respond(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		return
	}
	result, err := db.Exec(`DELETE FROM message_reactions WHERE message_id=$1 AND user_id=$2 AND emoji=$3`,
		input.MessageID, c.UserID, input.Emoji)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not update reaction"})
		return
	}
	removed, _ := result.RowsAffected()
	if removed == 0 {
		if _, err = db.Exec(`INSERT INTO message_reactions(message_id,user_id,emoji)
			VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, input.MessageID, c.UserID, input.Emoji); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not update reaction"})
			return
		}
	}
	rows, err := db.Query(`SELECT emoji,COUNT(*)::int,BOOL_OR(user_id=$2)
		FROM message_reactions WHERE message_id=$1 GROUP BY emoji`,
		input.MessageID, c.UserID)
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load reactions"})
		return
	}
	defer rows.Close()
	reactions := []Reaction{}
	for rows.Next() {
		var reaction Reaction
		if err = rows.Scan(&reaction.Emoji, &reaction.Count, &reaction.Mine); err != nil {
			respond(w, http.StatusInternalServerError, map[string]string{"error": "could not load reactions"})
			return
		}
		reactions = append(reactions, reaction)
	}
	payload := map[string]interface{}{"message_id": input.MessageID, "reactions": reactions}
	publish(channel, "reaction:update", payload)
	respond(w, http.StatusOK, payload)
}

func publish(room, eventType string, payload interface{}) {
	data, err := json.Marshal(Event{Type: eventType, Room: room, Payload: payload})
	if err == nil {
		_ = rdb.Publish(ctx, "ipchat:events", data).Err()
	}
}

func redisSubscriber() {
	sub := rdb.Subscribe(ctx, "ipchat:events")
	defer sub.Close()
	ch := sub.Channel()
	for msg := range ch {
		var event Event
		if json.Unmarshal([]byte(msg.Payload), &event) == nil {
			if event.Type == "member:kicked" {
				if payload, ok := event.Payload.(map[string]interface{}); ok {
					if username, ok := payload["username"].(string); ok {
						hub.closeUser(event.Room, username)
					}
				}
			}
			data, _ := json.Marshal(map[string]interface{}{
				"type": event.Type, "payload": event.Payload,
			})
			if event.Type == "presence:global" {
				hub.broadcastAll(data)
			} else {
				hub.broadcast(event.Room, data)
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return originAllowed(origin, r.Host)
	},
}

type websocketTicket struct {
	claims   *Claims
	room     string
	channel  string
	directID int64
	direct   bool
	presence bool
	expires  time.Time
}

var wsTickets = struct {
	sync.Mutex
	items map[string]websocketTicket
}{items: make(map[string]websocketTicket)}

func wsTicketHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	room := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("room")))
	if _, err = memberRole(room, c.UserID); err != nil {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a room member"})
		return
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not create ticket"})
		return
	}
	ticket := base64.RawURLEncoding.EncodeToString(b)
	wsTickets.Lock()
	wsTickets.items[ticket] = websocketTicket{
		claims: c, room: room, channel: room, expires: time.Now().Add(30 * time.Second),
	}
	wsTickets.Unlock()
	respond(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func directWSTicketHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	conversationID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || conversationID <= 0 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "invalid direct chat"})
		return
	}
	member, err := isDirectMember(conversationID, c.UserID)
	if err != nil || !member {
		respond(w, http.StatusForbidden, map[string]string{"error": "not a participant"})
		return
	}
	ticket, err := randomOAuthValue()
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not create ticket"})
		return
	}
	wsTickets.Lock()
	wsTickets.items[ticket] = websocketTicket{
		claims: c, directID: conversationID, direct: true,
		channel: directChannel(conversationID), expires: time.Now().Add(30 * time.Second),
	}
	wsTickets.Unlock()
	respond(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func presenceWSTicketHandler(w http.ResponseWriter, r *http.Request) {
	c, err := requestClaims(r)
	if err != nil {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	ticket, err := randomOAuthValue()
	if err != nil {
		respond(w, http.StatusInternalServerError, map[string]string{"error": "could not create ticket"})
		return
	}
	wsTickets.Lock()
	wsTickets.items[ticket] = websocketTicket{
		claims: c, channel: globalPresenceChannel, presence: true, expires: time.Now().Add(30 * time.Second),
	}
	wsTickets.Unlock()
	respond(w, http.StatusOK, map[string]string{"ticket": ticket})
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	wsTickets.Lock()
	item, ok := wsTickets.items[ticket]
	if ok {
		delete(wsTickets.items, ticket)
	}
	wsTickets.Unlock()
	if !ok || item.expires.Before(time.Now()) {
		http.Error(w, "unauthorized", 401)
		return
	}
	room := item.room
	channel := item.channel
	c := item.claims
	var revoked sql.NullTime
	var expires time.Time
	if err := db.QueryRow(`SELECT expires_at, revoked_at FROM user_sessions
		WHERE id=$1 AND user_id=$2`, c.SessionID, c.UserID).Scan(&expires, &revoked); err != nil ||
		revoked.Valid || expires.Before(time.Now()) {
		http.Error(w, "session revoked or expired", 401)
		return
	}
	if !item.presence && item.direct {
		member, memberErr := isDirectMember(item.directID, c.UserID)
		if memberErr != nil || !member {
			http.Error(w, "not a participant", 403)
			return
		}
	} else if !item.presence {
		if _, err := memberRole(room, c.UserID); err != nil {
			http.Error(w, "not a room member", 403)
			return
		}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &Client{
		conn: conn, send: make(chan []byte, 32),
		room: channel, user: c.Username,
	}
	hub.add(client)
	publish(globalPresenceChannel, "presence:global", hub.onlineAll())
	go client.writePump()

	presence, _ := json.Marshal(map[string]interface{}{
		"type": "presence:list", "payload": hub.onlineAll(),
	})
	client.send <- presence

	defer func() {
		hub.remove(client)
		_ = conn.Close()
		publish(globalPresenceChannel, "presence:global", hub.onlineAll())
	}()

	conn.SetReadLimit(32 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	for {
		var in WSMessage
		if err := conn.ReadJSON(&in); err != nil {
			break
		}
		switch in.Type {
		case "message":
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > 24<<10 {
				continue
			}
			var m Message
			if item.direct {
				blocked, blockErr := isDirectBlocked(item.directID, c.UserID)
				if blockErr != nil {
					sendClientEvent(client, "message:error", map[string]string{"error": "could not check direct message permissions"})
					continue
				}
				if blocked {
					sendClientEvent(client, "message:error", map[string]string{"error": "you cannot message this user while either of you has blocked the other"})
					continue
				}
				err := db.QueryRow(`INSERT INTO messages(direct_conversation_id,user_id,username,body,expires_at)
					SELECT $1,$2,$3,$4,
						CASE WHEN disappearing_seconds>0
							THEN NOW()+disappearing_seconds*INTERVAL '1 second' ELSE NULL END
					FROM direct_conversations WHERE id=$1
					RETURNING id,created_at`,
					item.directID, c.UserID, c.Username, text).Scan(&m.ID, &m.CreatedAt)
				if err != nil {
					continue
				}
			} else {
				var muted sql.NullTime
				err := db.QueryRow(`SELECT muted_until FROM room_members
					WHERE room_code=$1 AND user_id=$2`, room, c.UserID).Scan(&muted)
				if err != nil || (muted.Valid && muted.Time.After(time.Now())) {
					continue
				}
				err = db.QueryRow(`INSERT INTO messages(room_code,user_id,username,body)
					VALUES($1,$2,$3,$4) RETURNING id,created_at`,
					room, c.UserID, c.Username, text).Scan(&m.ID, &m.CreatedAt)
				if err != nil {
					continue
				}
			}
			m.Username, m.Text = c.Username, text
			publish(channel, "message:new", m)

		case "typing:start":
			if item.direct {
				blocked, blockErr := isDirectBlocked(item.directID, c.UserID)
				if blockErr != nil || blocked {
					continue
				}
			}
			publish(channel, "typing:start", map[string]string{"username": c.Username})
		case "typing:stop":
			publish(channel, "typing:stop", map[string]string{"username": c.Username})
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(25 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var _ = fmt.Sprintf
