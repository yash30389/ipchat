package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidUsername(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"abc", true}, {"user_01", true}, {"User-01", true},
		{"ab", false}, {strings.Repeat("a", 41), false}, {"bad name", false}, {"éclair", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validUsername(tc.name); got != tc.valid {
				t.Fatalf("validUsername(%q)=%v, want %v", tc.name, got, tc.valid)
			}
		})
	}
}

func TestTokenRoundTripAndTamperDetection(t *testing.T) {
	old := jwtSecret
	jwtSecret = []byte(strings.Repeat("x", 32))
	defer func() { jwtSecret = old }()
	token, err := makeToken(Claims{UserID: 42, Username: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := parseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != 42 || claims.Username != "tester" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if _, err := parseToken(token + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	old := jwtSecret
	jwtSecret = []byte(strings.Repeat("z", 32))
	defer func() { jwtSecret = old }()
	expired := Claims{UserID: 1, Username: "tester", Expires: time.Now().Add(-time.Hour).Unix()}
	data, _ := json.Marshal(expired)
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, jwtSecret)
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := parseToken(payload + "." + signature); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestPasswordHash(t *testing.T) {
	hash, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery" {
		t.Fatal("password was not hashed")
	}
	if !verifyPassword(hash, "correct horse battery") {
		t.Fatal("correct password did not match")
	}
	if verifyPassword(hash, "wrong password") {
		t.Fatal("wrong password matched")
	}
}

func TestRandomRoomCode(t *testing.T) {
	code, err := randomCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < 6 || len(code) > 16 {
		t.Fatalf("unexpected room code length %d", len(code))
	}
	if code != strings.ToUpper(code) {
		t.Fatal("room code must be uppercase")
	}
}

func TestOAuthUsernameCandidate(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Yash R", "Yash_R"},
		{"ab", "user_ab"},
		{"", "user_"},
		{strings.Repeat("a", 48), strings.Repeat("a", 32)},
	}
	for _, tc := range cases {
		if got := oauthUsernameCandidate(tc.input); got != tc.want {
			t.Errorf("oauthUsernameCandidate(%q)=%q, want %q", tc.input, got, tc.want)
		}
		if !validUsername(oauthUsernameCandidate(tc.input)) {
			t.Errorf("oauthUsernameCandidate(%q) returned invalid username", tc.input)
		}
	}
}

func TestOAuthCallbackURLRequiresValidBase(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_BASE", "http://localhost:8080")
	if got, err := oauthCallbackURL("github"); err != nil || got != "http://localhost:8080/auth/github/callback" {
		t.Fatalf("unexpected callback URL %q, error %v", got, err)
	}
	for _, base := range []string{"", "javascript://example.com", "https://example.com/path?query=1"} {
		t.Setenv("OAUTH_REDIRECT_BASE", base)
		if _, err := oauthCallbackURL("google"); err == nil {
			t.Errorf("oauthCallbackURL accepted invalid base %q", base)
		}
	}
}

func TestOAuthProvidersRequireBothCredentials(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "github-client")
	t.Setenv("GITHUB_CLIENT_SECRET", "")
	t.Setenv("GOOGLE_CLIENT_ID", "google-client")
	t.Setenv("GOOGLE_CLIENT_SECRET", "google-secret")
	recorder := httptest.NewRecorder()
	oauthProvidersHandler(recorder, httptest.NewRequest(http.MethodGet, "/api/auth/providers", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("provider status=%d, want %d", recorder.Code, http.StatusOK)
	}
	var providers map[string]bool
	if err := json.Unmarshal(recorder.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	if providers["github"] {
		t.Fatal("GitHub should be disabled unless both credentials are configured")
	}
	if !providers["google"] {
		t.Fatal("Google should be enabled when both credentials are configured")
	}
}

func TestHubAddOnlineRemove(t *testing.T) {
	h := newHub()
	c1 := &Client{room: "ROOM1", user: "alice", send: make(chan []byte, 1)}
	c2 := &Client{room: "ROOM1", user: "alice", send: make(chan []byte, 1)}
	c3 := &Client{room: "ROOM1", user: "bob", send: make(chan []byte, 1)}
	h.add(c1)
	h.add(c2)
	h.add(c3)
	if got := len(h.online("ROOM1")); got != 2 {
		t.Fatalf("online users=%d, want 2 unique users", got)
	}
	h.remove(c1)
	h.remove(c2)
	h.remove(c3)
	if got := len(h.online("ROOM1")); got != 0 {
		t.Fatalf("online users after remove=%d", got)
	}
}

func TestHubOnlineAllAcrossChannels(t *testing.T) {
	h := newHub()
	aliceRoom := &Client{room: "ROOM1", user: "alice", send: make(chan []byte, 1)}
	aliceDM := &Client{room: "dm:1", user: "alice", send: make(chan []byte, 1)}
	bobRoom := &Client{room: "ROOM2", user: "bob", send: make(chan []byte, 1)}
	h.add(aliceRoom)
	h.add(aliceDM)
	h.add(bobRoom)

	if got := strings.Join(h.onlineAll(), ","); got != "alice,bob" {
		t.Fatalf("global online users=%q, want alice,bob", got)
	}
	h.remove(aliceDM)
	if got := strings.Join(h.onlineAll(), ","); got != "alice,bob" {
		t.Fatalf("user with another active socket was marked offline: %q", got)
	}
	h.remove(aliceRoom)
	if got := strings.Join(h.onlineAll(), ","); got != "bob" {
		t.Fatalf("global online users after alice disconnects=%q, want bob", got)
	}
	h.remove(bobRoom)
}

func TestValidDisappearingSeconds(t *testing.T) {
	for _, seconds := range []int{0, 86400, 604800, 7776000} {
		if !validDisappearingSeconds(seconds) {
			t.Errorf("validDisappearingSeconds(%d)=false, want true", seconds)
		}
	}
	for _, seconds := range []int{-1, 1, 3600, 86401} {
		if validDisappearingSeconds(seconds) {
			t.Errorf("validDisappearingSeconds(%d)=true, want false", seconds)
		}
	}
}

func TestParseHistoryPage(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantBefore int64
		wantLimit  int
		wantError  bool
	}{
		{name: "defaults", wantLimit: 100},
		{name: "cursor and limit", query: "?before_id=90&limit=25", wantBefore: 90, wantLimit: 25},
		{name: "limit capped", query: "?limit=500", wantLimit: 100},
		{name: "negative cursor", query: "?before_id=-1", wantError: true},
		{name: "zero limit", query: "?limit=0", wantError: true},
		{name: "malformed limit", query: "?limit=abc", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/history"+test.query, nil)
			before, limit, err := parseHistoryPage(request)
			if (err != nil) != test.wantError {
				t.Fatalf("parseHistoryPage() error=%v, wantError=%v", err, test.wantError)
			}
			if err == nil && (before != test.wantBefore || limit != test.wantLimit) {
				t.Fatalf("parseHistoryPage()=(%d,%d), want (%d,%d)",
					before, limit, test.wantBefore, test.wantLimit)
			}
		})
	}
}

func TestValidReportReason(t *testing.T) {
	for _, reason := range []string{"ab", strings.Repeat("x", 501), "   "} {
		if validReportReason(reason) {
			t.Errorf("validReportReason(%q)=true, want false", reason)
		}
	}
	for _, reason := range []string{"spam report", "abuse report", strings.Repeat("x", 500), strings.Repeat("😀", 500)} {
		if !validReportReason(reason) {
			t.Errorf("validReportReason(%q)=false, want true", reason)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	old := os.Getenv("ALLOWED_ORIGINS")
	_ = os.Setenv("ALLOWED_ORIGINS", "https://chat.example.com,https://admin.example.com")
	defer os.Setenv("ALLOWED_ORIGINS", old)
	cases := []struct {
		origin string
		host   string
		want   bool
	}{
		{"https://app.local", "app.local", true},
		{"https://chat.example.com", "api.example.com", true},
		{"https://chat.example.com.attacker.test", "api.example.com", false},
		{"javascript://app.local", "app.local", false},
		{"https://evil.test", "app.local", false},
		{"", "app.local", true},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			if got := originAllowed(tc.origin, tc.host); got != tc.want {
				t.Fatalf("originAllowed(%q, %q)=%v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}
