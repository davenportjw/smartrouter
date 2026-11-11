package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	firebase "firebase.google.com/go/v4"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// ---------------------------------------------------------------------------
// Test harness: a real Firebase Admin SDK client whose Google endpoints (public
// certificates, accounts:lookup, createSessionCookie) are served by a local TLS
// server that signs with a throwaway RSA key generated per test.
// ---------------------------------------------------------------------------

const testProjectID = "test-project"

type fakeAccount struct {
	disabled   bool
	validSince int64
}

type fakeGoogle struct {
	t        *testing.T
	key      *rsa.PrivateKey
	kid      string
	certPEM  string
	server   *httptest.Server
	mu       sync.Mutex
	accounts map[string]fakeAccount
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "throwaway-test-signer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	fg := &fakeGoogle{
		t:        t,
		key:      key,
		kid:      "test-kid-1",
		certPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		accounts: map[string]fakeAccount{},
	}
	fg.server = httptest.NewTLSServer(http.HandlerFunc(fg.serve))
	t.Cleanup(fg.server.Close)
	return fg
}

func (fg *fakeGoogle) setAccount(uid string, acct fakeAccount) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.accounts[uid] = acct
}

func (fg *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/identitytoolkit/v3/relyingparty/publicKeys",
		r.URL.Path == "/robot/v1/metadata/x509/securetoken@system.gserviceaccount.com":
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_ = json.NewEncoder(w).Encode(map[string]string{fg.kid: fg.certPEM})
	case r.URL.Path == "/v1/projects/"+testProjectID+"/accounts:lookup":
		var body struct {
			LocalID []string `json:"localId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fg.mu.Lock()
		defer fg.mu.Unlock()
		var users []map[string]any
		for _, uid := range body.LocalID {
			acct, ok := fg.accounts[uid]
			if !ok {
				continue
			}
			users = append(users, map[string]any{
				"localId":    uid,
				"disabled":   acct.disabled,
				"validSince": strconv.FormatInt(acct.validSince, 10),
			})
		}
		if len(users) == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"kind":"identitytoolkit#GetAccountInfoResponse"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": users})
	case r.URL.Path == "/v1/projects/"+testProjectID+":createSessionCookie":
		var body struct {
			IDToken string `json:"idToken"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		claims := decodeClaims(fg.t, body.IDToken)
		now := time.Now().Unix()
		claims["iss"] = "https://session.firebase.google.com/" + testProjectID
		claims["iat"] = now
		claims["exp"] = now + int64(SessionDuration.Seconds())
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionCookie": fg.sign(claims, fg.key, fg.kid)})
	case r.URL.Path == "/v1/projects/"+testProjectID+"/accounts:update":
		var body struct {
			LocalID    string `json:"localId"`
			ValidSince string `json:"validSince"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		vs, _ := strconv.ParseInt(body.ValidSince, 10, 64)
		fg.mu.Lock()
		acct := fg.accounts[body.LocalID]
		acct.validSince = vs
		fg.accounts[body.LocalID] = acct
		fg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"localId": body.LocalID})
	default:
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}
}

func decodeClaims(t *testing.T, token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("bad token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return claims
}

func (fg *fakeGoogle) sign(claims map[string]any, key *rsa.PrivateKey, kid string) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		fg.t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type tokenOpts struct {
	uid, email, provider string
	emailVerified        bool
	issuer, audience     string
	authTime, iat, exp   int64
}

func defaultOpts(uid, email string) tokenOpts {
	now := time.Now().Unix()
	return tokenOpts{
		uid: uid, email: email, provider: "google.com", emailVerified: true,
		audience: testProjectID, authTime: now - 30, iat: now - 30, exp: now + 3600,
	}
}

func (o tokenOpts) claims(defaultIssuer string) map[string]any {
	iss := o.issuer
	if iss == "" {
		iss = defaultIssuer
	}
	return map[string]any{
		"iss": iss, "aud": o.audience, "sub": o.uid, "user_id": o.uid,
		"email": o.email, "email_verified": o.emailVerified,
		"auth_time": o.authTime, "iat": o.iat, "exp": o.exp,
		"firebase": map[string]any{"sign_in_provider": o.provider},
	}
}

func (fg *fakeGoogle) idToken(o tokenOpts) string {
	return fg.sign(o.claims("https://securetoken.google.com/"+testProjectID), fg.key, fg.kid)
}

func (fg *fakeGoogle) sessionCookie(o tokenOpts) string {
	return fg.sign(o.claims("https://session.firebase.google.com/"+testProjectID), fg.key, fg.kid)
}

// newStore builds an AuthStore around a real Admin SDK client routed to the fake server.
func (fg *fakeGoogle) newStore(t *testing.T) *AuthStore {
	t.Helper()
	addr := fg.server.Listener.Addr().String()
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only local server
	}
	defer func() { http.DefaultTransport = orig }()

	app, err := firebase.NewApp(context.Background(), &firebase.Config{ProjectID: testProjectID},
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access-token"})))
	if err != nil {
		t.Fatalf("firebase app: %v", err)
	}
	client, err := app.Auth(context.Background())
	if err != nil {
		t.Fatalf("firebase auth: %v", err)
	}
	return &AuthStore{Client: client}
}

func setAllowlist(t *testing.T, v string) {
	for _, k := range []string{"ALLOWED_EMAIL_DOMAINS", "ALLOWED_AUTH_DOMAINS", "ALLOWED_DOMAINS", "ADMIN_ALLOWED_DOMAINS", "ALLOWED_EMAILS"} {
		t.Setenv(k, "")
	}
	t.Setenv("ALLOWED_EMAIL_DOMAINS", v)
}

func sessionRequest(cookie string) *http.Request {
	req := httptest.NewRequest("GET", "/admin/apps", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	}
	return req
}

func loginRequest(idToken string) *http.Request {
	form := url.Values{}
	form.Set("idToken", idToken)
	req := httptest.NewRequest("POST", "http://router.example.com/auth/session", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://router.example.com")
	return req
}

// ---------------------------------------------------------------------------
// Allowlist helpers
// ---------------------------------------------------------------------------

func TestIsEmailAuthorized(t *testing.T) {
	allowedList := []string{
		"corp.example.org",
		"example.com",
		"admin@contractor.example.org",
		"@special-team.example.org",
	}

	tests := []struct {
		name     string
		email    string
		expected bool
	}{
		{"Exact match on allowed domain", "operator@corp.example.org", true},
		{"Exact match on other allowed domain", "user@example.com", true},
		{"Exact match on specific allowed email address", "admin@contractor.example.org", true},
		{"Exact match on domain with leading @ in config", "leader@special-team.example.org", true},
		{"Denied email in disallowed domain", "user@contractor.example.org", false},
		{"Denied email on random domain", "intruder@random.example.net", false},
		{"Denied look-alike suffix domain", "user@notexample.com", false},
		{"Case insensitivity test for email and pattern matching", "Admin@CONTRACTOR.example.org", true},
		{"Case insensitivity test for domains", "Operator@Corp.Example.Org", true},
		{"Whitespace trimming test", "   admin@contractor.example.org   ", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if actual := isEmailAuthorized(tc.email, allowedList); actual != tc.expected {
				t.Errorf("isEmailAuthorized(%q) = %v; want %v", tc.email, actual, tc.expected)
			}
		})
	}
}

func TestResolveAllowedDomainsFromEnv(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "")
	t.Setenv("ALLOWED_AUTH_DOMAINS", "example.com, corp.example.org")
	t.Setenv("ALLOWED_DOMAINS", "corp.example.org, partner.example.net")
	t.Setenv("ADMIN_ALLOWED_DOMAINS", "")
	t.Setenv("ALLOWED_EMAILS", "admin@contractor.example.org")

	domains := ResolveAllowedDomainsFromEnv()
	if len(domains) != 4 {
		t.Fatalf("expected 4 unique allowed entries, got %d: %v", len(domains), domains)
	}
	for _, ok := range []string{"dev@example.com", "eng@corp.example.org", "ops@partner.example.net", "admin@contractor.example.org"} {
		if !isEmailAuthorized(ok, domains) {
			t.Errorf("expected %s to be authorized", ok)
		}
	}
	if isEmailAuthorized("intruder@unauthorized.example.net", domains) {
		t.Errorf("expected intruder to be denied")
	}
}

// ---------------------------------------------------------------------------
// Local-dev escape hatch
// ---------------------------------------------------------------------------

func TestLocalDevNeverAppliesOnCloudRun(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("K_SERVICE", "")
	if !localDevEnabled() || secureCookies() {
		t.Fatalf("expected local dev relaxations off Cloud Run")
	}
	t.Setenv("K_SERVICE", "smartrouter-frontend")
	if localDevEnabled() {
		t.Fatalf("LOCAL_DEV must be ignored when K_SERVICE is set")
	}
	if !secureCookies() {
		t.Fatalf("cookies must be Secure on Cloud Run")
	}
}

func TestNoLocalDevBypassToken(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	t.Setenv("K_SERVICE", "")
	store := &AuthStore{} // no Firebase client: everything must fail closed

	rr := httptest.NewRecorder()
	store.CreateSession(rr, loginRequest("any-made-up-token"))
	if rr.Code == http.StatusOK {
		t.Fatalf("expected sign-in to fail without Firebase, got 200")
	}
	if c := rr.Result().Cookies(); len(c) != 0 {
		t.Fatalf("expected no cookie, got %+v", c)
	}

	rr = httptest.NewRecorder()
	called := false
	store.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).
		ServeHTTP(rr, sessionRequest("any-made-up-cookie"))
	if called || rr.Code != http.StatusFound {
		t.Fatalf("expected redirect without calling handler; code=%d called=%v", rr.Code, called)
	}
}

// ---------------------------------------------------------------------------
// CreateSession (login)
// ---------------------------------------------------------------------------

func TestCreateSession(t *testing.T) {
	t.Setenv("K_SERVICE", "smartrouter-frontend")
	setAllowlist(t, "example.com")
	fg := newFakeGoogle(t)
	store := fg.newStore(t)
	fg.setAccount("uid-ok", fakeAccount{})

	t.Run("Method Not Allowed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		store.CreateSession(rr, httptest.NewRequest("GET", "/auth/session", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", rr.Code)
		}
	})

	t.Run("Missing idToken", func(t *testing.T) {
		req := loginRequest("")
		rr := httptest.NewRecorder()
		store.CreateSession(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", rr.Code)
		}
	})

	t.Run("Cross-origin login rejected", func(t *testing.T) {
		req := loginRequest(fg.idToken(defaultOpts("uid-ok", "admin@example.com")))
		req.Header.Set("Origin", "https://attacker.example.net")
		rr := httptest.NewRecorder()
		store.CreateSession(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d", rr.Code)
		}
	})

	t.Run("Valid Google ID token mints Firebase session cookie", func(t *testing.T) {
		rr := httptest.NewRecorder()
		store.CreateSession(rr, loginRequest(fg.idToken(defaultOpts("uid-ok", "admin@example.com"))))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var session *http.Cookie
		for _, c := range rr.Result().Cookies() {
			if c.Name == SessionCookieName {
				session = c
			}
		}
		if session == nil {
			t.Fatalf("session cookie not set")
		}
		if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
			t.Errorf("unexpected cookie attributes: %+v", session)
		}
		if session.MaxAge != int(SessionDuration.Seconds()) {
			t.Errorf("expected 5-day MaxAge, got %d", session.MaxAge)
		}
		if iss := decodeClaims(t, session.Value)["iss"]; iss != "https://session.firebase.google.com/"+testProjectID {
			t.Errorf("expected Firebase session cookie issuer, got %v", iss)
		}
		// The minted cookie must verify end to end.
		got, err := store.VerifySession(sessionRequest(session.Value))
		if err != nil || got.Email != "admin@example.com" || got.UID != "uid-ok" {
			t.Fatalf("minted cookie failed verification: %+v %v", got, err)
		}
	})

	rejected := []struct {
		name string
		mut  func(o *tokenOpts)
		tok  func(o tokenOpts) string
		code int
	}{
		{"Non-Google provider", func(o *tokenOpts) { o.provider = "password" }, nil, http.StatusForbidden},
		{"Unverified email", func(o *tokenOpts) { o.emailVerified = false }, nil, http.StatusForbidden},
		{"Email not on allowlist", func(o *tokenOpts) { o.email = "intruder@other.example.net" }, nil, http.StatusForbidden},
		{"Stale sign-in (auth_time > 10 min)", func(o *tokenOpts) { o.authTime = time.Now().Add(-11 * time.Minute).Unix() }, nil, http.StatusUnauthorized},
		{"Expired ID token", func(o *tokenOpts) { o.exp = time.Now().Add(-10 * time.Minute).Unix() }, nil, http.StatusUnauthorized},
		{"Wrong audience", func(o *tokenOpts) { o.audience = "other-project" }, nil, http.StatusUnauthorized},
		{"Wrong issuer", func(o *tokenOpts) { o.issuer = "https://securetoken.google.com/other-project" }, nil, http.StatusUnauthorized},
		{"Forged with wrong key", nil, func(o tokenOpts) string {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			return fg.sign(o.claims("https://securetoken.google.com/"+testProjectID), other, fg.kid)
		}, http.StatusUnauthorized},
		{"Session cookie presented as ID token", nil, func(o tokenOpts) string { return fg.sessionCookie(o) }, http.StatusUnauthorized},
	}
	for _, tc := range rejected {
		t.Run("Rejects "+tc.name, func(t *testing.T) {
			o := defaultOpts("uid-ok", "admin@example.com")
			if tc.mut != nil {
				tc.mut(&o)
			}
			tok := fg.idToken(o)
			if tc.tok != nil {
				tok = tc.tok(o)
			}
			rr := httptest.NewRecorder()
			store.CreateSession(rr, loginRequest(tok))
			if rr.Code != tc.code {
				t.Errorf("expected %d, got %d: %s", tc.code, rr.Code, rr.Body.String())
			}
			for _, c := range rr.Result().Cookies() {
				if c.Name == SessionCookieName && c.Value != "" {
					t.Errorf("no session cookie may be set on rejection")
				}
			}
		})
	}

	t.Run("Missing allowlist fails closed", func(t *testing.T) {
		setAllowlist(t, "")
		rr := httptest.NewRecorder()
		store.CreateSession(rr, loginRequest(fg.idToken(defaultOpts("uid-ok", "admin@example.com"))))
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for missing allowlist, got %d", rr.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Per-request session verification
// ---------------------------------------------------------------------------

func TestMiddlewareSessionVerification(t *testing.T) {
	t.Setenv("K_SERVICE", "smartrouter-frontend")
	setAllowlist(t, "example.com")
	fg := newFakeGoogle(t)
	store := fg.newStore(t)
	fg.setAccount("uid-ok", fakeAccount{})
	fg.setAccount("uid-disabled", fakeAccount{disabled: true})
	fg.setAccount("uid-revoked", fakeAccount{validSince: time.Now().Unix()})

	var gotSession *Session
	handler := store.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession, _ = SessionFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	expectRejected := func(t *testing.T, cookie string) {
		t.Helper()
		gotSession = nil
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, sessionRequest(cookie))
		if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/login" {
			t.Fatalf("expected redirect to /login, got %d %q", rr.Code, rr.Header().Get("Location"))
		}
		if gotSession != nil {
			t.Fatalf("handler must not run for rejected session")
		}
		if cookie != "" {
			cleared := false
			for _, c := range rr.Result().Cookies() {
				if c.Name == SessionCookieName && c.MaxAge == -1 && c.Secure && c.HttpOnly {
					cleared = true
				}
			}
			if !cleared {
				t.Fatalf("expected rejected cookie to be cleared")
			}
		}
	}

	t.Run("Valid session passes", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, sessionRequest(fg.sessionCookie(defaultOpts("uid-ok", "Admin@Example.com"))))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		if gotSession == nil || gotSession.UID != "uid-ok" || gotSession.Email != "admin@example.com" {
			t.Fatalf("unexpected session in context: %+v", gotSession)
		}
		if !store.HasValidSession(sessionRequest(fg.sessionCookie(defaultOpts("uid-ok", "admin@example.com")))) {
			t.Fatalf("HasValidSession should accept a valid cookie")
		}
	})

	t.Run("Missing cookie redirects", func(t *testing.T) { expectRejected(t, "") })
	t.Run("Garbage cookie", func(t *testing.T) { expectRejected(t, "stale-invalid-cookie") })

	t.Run("Forged cookie signed with wrong key", func(t *testing.T) {
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		o := defaultOpts("uid-ok", "admin@example.com")
		expectRejected(t, fg.sign(o.claims("https://session.firebase.google.com/"+testProjectID), other, fg.kid))
	})
	t.Run("Unknown kid", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		expectRejected(t, fg.sign(o.claims("https://session.firebase.google.com/"+testProjectID), fg.key, "unknown-kid"))
	})
	t.Run("Wrong issuer", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		o.issuer = "https://session.firebase.google.com/other-project"
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("ID token instead of session cookie", func(t *testing.T) {
		expectRejected(t, fg.idToken(defaultOpts("uid-ok", "admin@example.com")))
	})
	t.Run("Wrong audience", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		o.audience = "other-project"
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("Expired", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		o.iat = time.Now().Add(-6 * 24 * time.Hour).Unix()
		o.authTime = o.iat
		o.exp = time.Now().Add(-time.Hour).Unix()
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("Revoked", func(t *testing.T) {
		o := defaultOpts("uid-revoked", "admin@example.com")
		o.iat = time.Now().Add(-time.Hour).Unix()
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("Disabled account", func(t *testing.T) {
		expectRejected(t, fg.sessionCookie(defaultOpts("uid-disabled", "admin@example.com")))
	})
	t.Run("Deleted account", func(t *testing.T) {
		expectRejected(t, fg.sessionCookie(defaultOpts("uid-missing", "admin@example.com")))
	})
	t.Run("Non-Google provider", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		o.provider = "password"
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("Unverified email", func(t *testing.T) {
		o := defaultOpts("uid-ok", "admin@example.com")
		o.emailVerified = false
		expectRejected(t, fg.sessionCookie(o))
	})
	t.Run("Allowlist re-checked per request", func(t *testing.T) {
		cookie := fg.sessionCookie(defaultOpts("uid-ok", "admin@example.com"))
		setAllowlist(t, "corp.example.org")
		expectRejected(t, cookie)
		setAllowlist(t, "")
		expectRejected(t, cookie)
		setAllowlist(t, "example.com")
	})

	t.Run("Spoofed identity headers are ignored", func(t *testing.T) {
		// Without a cookie, identity headers grant nothing.
		req := sessionRequest("")
		req.Header.Set("X-User-Email", "admin@example.com")
		req.Header.Set("X-User-Id", "uid-ok")
		req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:admin@example.com")
		gotSession = nil
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusFound || gotSession != nil {
			t.Fatalf("identity headers must not authenticate; code=%d", rr.Code)
		}

		// With a valid cookie, the actor comes from the cookie, never the headers.
		fg.setAccount("uid-other", fakeAccount{})
		req = sessionRequest(fg.sessionCookie(defaultOpts("uid-other", "viewer@example.com")))
		req.Header.Set("X-User-Email", "admin@example.com")
		req.Header.Set("X-User-Id", "uid-ok")
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || gotSession == nil || gotSession.UID != "uid-other" || gotSession.Email != "viewer@example.com" {
			t.Fatalf("expected cookie identity, got code=%d session=%+v", rr.Code, gotSession)
		}
	})

	t.Run("CSRF: cross-origin POST rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://router.example.com/admin/apps/create", nil)
		req.Header.Set("Origin", "https://attacker.example.net")
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: fg.sessionCookie(defaultOpts("uid-ok", "admin@example.com"))})
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rr.Code)
		}
	})
}

func TestLogoutRevokesAndClears(t *testing.T) {
	t.Setenv("K_SERVICE", "smartrouter-frontend")
	setAllowlist(t, "example.com")
	fg := newFakeGoogle(t)
	store := fg.newStore(t)
	fg.setAccount("uid-ok", fakeAccount{})

	o := defaultOpts("uid-ok", "admin@example.com")
	o.iat = time.Now().Add(-time.Minute).Unix()
	cookie := fg.sessionCookie(o)
	if !store.HasValidSession(sessionRequest(cookie)) {
		t.Fatalf("precondition: cookie should be valid")
	}

	req := sessionRequest(cookie)
	rr := httptest.NewRecorder()
	store.Logout(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d", rr.Code)
	}
	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected session cookie to be cleared")
	}
	if store.HasValidSession(sessionRequest(cookie)) {
		t.Fatalf("cookie must be rejected after logout revokes sessions")
	}
}
