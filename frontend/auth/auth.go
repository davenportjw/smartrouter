package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
)

type contextKey string

const (
	uidContextKey   contextKey = "uid"
	emailContextKey contextKey = "email"

	// SessionCookieName is the HttpOnly cookie that carries the Firebase session cookie.
	SessionCookieName = "session"
	// SessionDuration is the validity of a Firebase session cookie (5 days).
	SessionDuration = 5 * 24 * time.Hour
	// maxSignInAge bounds how old the Google sign-in behind an ID token may be when minting a session.
	maxSignInAge = 10 * time.Minute
	// googleSignInProvider is the only accepted Firebase sign-in provider.
	googleSignInProvider = "google.com"
)

// FirebaseAuthClient is the subset of the Firebase Admin SDK auth client used here.
// *auth.Client satisfies it; tests may inject a client built against throwaway keys.
type FirebaseAuthClient interface {
	VerifyIDToken(ctx context.Context, idToken string) (*auth.Token, error)
	SessionCookie(ctx context.Context, idToken string, expiresIn time.Duration) (string, error)
	VerifySessionCookie(ctx context.Context, sessionCookie string) (*auth.Token, error)
	VerifySessionCookieAndCheckRevoked(ctx context.Context, sessionCookie string) (*auth.Token, error)
	RevokeRefreshTokens(ctx context.Context, uid string) error
}

// AuthStore handles connection to Firebase Auth service.
type AuthStore struct {
	Client FirebaseAuthClient
	// now is overridable in tests; defaults to time.Now.
	now func() time.Time
}

// Session is the verified identity behind a Firebase session cookie.
type Session struct {
	UID   string
	Email string
}

// onCloudRun reports whether the process is running on Cloud Run.
func onCloudRun() bool {
	return os.Getenv("K_SERVICE") != ""
}

// localDevEnabled reports whether local-development relaxations apply. They can never
// apply on Cloud Run, even if LOCAL_DEV is set there by mistake.
func localDevEnabled() bool {
	return os.Getenv("LOCAL_DEV") == "true" && !onCloudRun()
}

// NewAuthStore initializes the Firebase Admin SDK auth client with Application Default Credentials.
// Outside Cloud Run with LOCAL_DEV=true, an initialization failure is logged and every sign-in
// fails closed instead of aborting startup. There is no authentication bypass.
func NewAuthStore(ctx context.Context) (*AuthStore, error) {
	client, err := newFirebaseAuthClient(ctx)
	if err != nil {
		if localDevEnabled() {
			log.Printf("[Auth] Firebase auth unavailable in local dev (%v); sign-in will fail until ADC is configured", err)
			return &AuthStore{}, nil
		}
		return nil, err
	}
	return &AuthStore{Client: client}, nil
}

func newFirebaseAuthClient(ctx context.Context) (FirebaseAuthClient, error) {
	var conf *firebase.Config
	if projectID := firebaseProjectID(); projectID != "" {
		conf = &firebase.Config{ProjectID: projectID}
	}
	// Uses Google Application Default Credentials (ADC) automatically.
	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize firebase app: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize firebase auth client: %w", err)
	}
	return client, nil
}

func firebaseProjectID() string {
	if v := strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID")); v != "" && v != "your-gcp-project-id" {
		return v
	}
	return strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
}

func (as *AuthStore) clock() time.Time {
	if as.now != nil {
		return as.now()
	}
	return time.Now()
}

func secureCookies() bool {
	return !localDevEnabled()
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// checkIdentityClaims enforces a verified email, Google as the sign-in provider, and the
// configured allowlist. It applies to both ID tokens and session cookies.
func checkIdentityClaims(token *auth.Token) (string, error) {
	if token == nil || strings.TrimSpace(token.UID) == "" {
		return "", errors.New("token is missing uid")
	}
	email, _ := token.Claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", errors.New("token is missing email claim")
	}
	if verified, _ := token.Claims["email_verified"].(bool); !verified {
		return "", errors.New("email is not verified")
	}
	if token.Firebase.SignInProvider != googleSignInProvider {
		return "", fmt.Errorf("sign-in provider %q is not allowed", token.Firebase.SignInProvider)
	}
	allowed := ResolveAllowedDomainsFromEnv()
	if len(allowed) == 0 {
		return "", errAllowlistMissing
	}
	if !isEmailAuthorized(email, allowed) {
		return "", fmt.Errorf("email %s is not authorized", email)
	}
	return email, nil
}

var errAllowlistMissing = errors.New("no authorized domains are configured in ALLOWED_EMAIL_DOMAINS / ALLOWED_AUTH_DOMAINS")

// VerifySession verifies the request's Firebase session cookie (signature, issuer, audience,
// expiry, revocation and disabled state via the Admin SDK) and re-checks the verified email,
// Google provider and allowlist. It fails closed; identity headers are never consulted.
func (as *AuthStore) VerifySession(r *http.Request) (*Session, error) {
	if as == nil || as.Client == nil {
		return nil, errors.New("firebase auth client not initialized")
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errors.New("missing session cookie")
	}
	token, err := as.Client.VerifySessionCookieAndCheckRevoked(r.Context(), cookie.Value)
	if err != nil {
		return nil, err
	}
	email, err := checkIdentityClaims(token)
	if err != nil {
		return nil, err
	}
	return &Session{UID: token.UID, Email: email}, nil
}

// CreateSession verifies a client-side Google ID token and issues a secure HTTP-only
// Firebase session cookie.
func (as *AuthStore) CreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !verifyOrigin(r) {
		http.Error(w, "Forbidden: CSRF verification failed", http.StatusForbidden)
		return
	}

	idToken := strings.TrimSpace(r.FormValue("idToken"))
	if idToken == "" {
		http.Error(w, "Missing idToken parameter", http.StatusBadRequest)
		return
	}

	if as.Client == nil {
		http.Error(w, "Sign-in is unavailable: authentication is not configured", http.StatusServiceUnavailable)
		return
	}

	decodedToken, err := as.Client.VerifyIDToken(r.Context(), idToken)
	if err != nil {
		log.Printf("[Auth] Error verifying ID token: %v", err)
		http.Error(w, "Invalid ID token", http.StatusUnauthorized)
		return
	}

	authTime := decodedToken.AuthTime
	signInAge := as.clock().Sub(time.Unix(authTime, 0))
	if authTime <= 0 || signInAge > maxSignInAge || signInAge < -time.Minute {
		http.Error(w, "Recent sign-in required; please sign in again", http.StatusUnauthorized)
		return
	}

	email, err := checkIdentityClaims(decodedToken)
	if err != nil {
		if errors.Is(err, errAllowlistMissing) {
			log.Printf("[Auth] Access denied: %v", err)
			http.Error(w, "Authentication system is misconfigured: ALLOWED_EMAIL_DOMAINS must be explicitly set", http.StatusInternalServerError)
			return
		}
		log.Printf("[Auth] Access denied: %v", err)
		http.Error(w, "Access restricted to verified Google accounts on authorized domains or specific email addresses", http.StatusForbidden)
		return
	}

	cookieString, err := as.Client.SessionCookie(r.Context(), idToken, SessionDuration)
	if err != nil {
		log.Printf("[Auth] Error creating session cookie for %s: %v", email, err)
		http.Error(w, "Failed to create session", http.StatusUnauthorized)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    cookieString,
		MaxAge:   int(SessionDuration.Seconds()),
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status": "success"}`))
}

// Logout clears the secure session cookie, revokes the user's Firebase sessions, and redirects.
func (as *AuthStore) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" && as.Client != nil {
		// Revoke session on Firebase server side for security
		if decoded, err := as.Client.VerifySessionCookie(r.Context(), cookie.Value); err == nil {
			_ = as.Client.RevokeRefreshTokens(r.Context(), decoded.UID)
		}
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// Middleware verifies the session cookie and intercepts unauthorized requests to `/admin/*`.
func (as *AuthStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stateless CSRF protection: verify origin/referer on state-changing requests
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !verifyOrigin(r) {
				log.Printf("[Auth Middleware] CSRF verification failed for %s %s: Origin/Referer does not match Host %q", r.Method, r.URL.Path, r.Host)
				http.Error(w, "Forbidden: CSRF verification failed", http.StatusForbidden)
				return
			}
		}

		if _, err := r.Cookie(SessionCookieName); err != nil {
			// No session cookie, redirect to login
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		session, err := as.VerifySession(r)
		if err != nil {
			log.Printf("[Auth Middleware] Rejected session cookie: %v", err)
			// Delete corrupt/revoked/unauthorized cookie and redirect
			clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		// Set verified identity in context so templates/handlers can retrieve user details
		ctx := context.WithValue(r.Context(), uidContextKey, session.UID)
		ctx = context.WithValue(ctx, emailContextKey, session.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SessionFromContext returns the verified identity set by Middleware.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	uid, _ := ctx.Value(uidContextKey).(string)
	email, _ := ctx.Value(emailContextKey).(string)
	if uid == "" || email == "" {
		return nil, false
	}
	return &Session{UID: uid, Email: email}, true
}

// HasValidSession checks whether the request carries a valid, verified session cookie.
func (as *AuthStore) HasValidSession(r *http.Request) bool {
	_, err := as.VerifySession(r)
	return err == nil
}

// ResolveAllowedDomainsFromEnv parses comma-separated authorized domains and email addresses
// from ALLOWED_EMAIL_DOMAINS, ALLOWED_AUTH_DOMAINS, ALLOWED_DOMAINS, ADMIN_ALLOWED_DOMAINS, and ALLOWED_EMAILS.
func ResolveAllowedDomainsFromEnv() []string {
	envKeys := []string{
		"ALLOWED_EMAIL_DOMAINS",
		"ALLOWED_AUTH_DOMAINS",
		"ALLOWED_DOMAINS",
		"ADMIN_ALLOWED_DOMAINS",
		"ALLOWED_EMAILS",
	}
	seen := make(map[string]struct{})
	var allowed []string
	for _, key := range envKeys {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			lower := strings.ToLower(trimmed)
			if _, exists := seen[lower]; !exists {
				seen[lower] = struct{}{}
				allowed = append(allowed, trimmed)
			}
		}
	}
	return allowed
}

// isEmailAuthorized checks if the provided email is authorized based on a list of specific emails or domains.
func isEmailAuthorized(email string, allowedList []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, entry := range allowedList {
		entry = strings.ToLower(strings.TrimSpace(entry))
		// Trim leading '@' if user input includes it (e.g., @gmail.com)
		cleanedEntry := strings.TrimPrefix(entry, "@")
		if cleanedEntry == "" {
			continue
		}
		if strings.Contains(cleanedEntry, "@") {
			// Match specific email address exactly
			if email == cleanedEntry {
				return true
			}
		} else {
			// Match entire domain suffix
			if strings.HasSuffix(email, "@"+cleanedEntry) {
				return true
			}
		}
	}
	return false
}

// verifyOrigin validates that the Origin or Referer matches the current Host for CSRF protection.
func verifyOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer == "" {
			return false
		}
		// Parse referer and extract host
		idx := strings.Index(referer, "://")
		if idx == -1 {
			return false
		}
		refHost := referer[idx+3:]
		if end := strings.IndexByte(refHost, '/'); end != -1 {
			refHost = refHost[:end]
		}
		return refHost == r.Host
	}

	// Parse origin and extract host
	idx := strings.Index(origin, "://")
	if idx == -1 {
		return false
	}
	origHost := origin[idx+3:]
	if end := strings.IndexByte(origHost, '/'); end != -1 {
		origHost = origHost[:end]
	}
	return origHost == r.Host
}
