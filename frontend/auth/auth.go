package auth

import (
	"context"
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

const uidContextKey contextKey = "uid"

// AuthStore handles connection to Firebase Auth service.
type AuthStore struct {
	Client *auth.Client
}

// NewAuthStore initializes a connection to the Firebase Auth service or sets up mock for local dev.
func NewAuthStore(ctx context.Context) (*AuthStore, error) {
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"
	if isLocalDev {
		return &AuthStore{Client: nil}, nil
	}

	// Uses Google Application Default Credentials (ADC) automatically
	app, err := firebase.NewApp(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize firebase app: %w", err)
	}

	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize firebase auth client: %w", err)
	}

	return &AuthStore{Client: client}, nil
}

// CreateSession verifies a client-side ID token and issues a secure HTTP-only session cookie.
func (as *AuthStore) CreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID Token from body or header
	idToken := r.FormValue("idToken")
	if idToken == "" {
		http.Error(w, "Missing idToken parameter", http.StatusBadRequest)
		return
	}

	// Set session duration (5 days)
	expiresIn := time.Hour * 24 * 5

	isLocalDev := os.Getenv("LOCAL_DEV") == "true"
	var cookieString string

	if isLocalDev && idToken == "dev-admin-token" {
		cookieString = "dev-admin-session-cookie"
	} else {
		if as.Client == nil {
			http.Error(w, "Firebase auth client not initialized", http.StatusInternalServerError)
			return
		}

		// First, verify the ID token to inspect its claims (specifically email)
		decodedToken, err := as.Client.VerifyIDToken(r.Context(), idToken)
		if err != nil {
			log.Printf("[Auth] Error verifying ID token: %v", err)
			http.Error(w, "Invalid ID token", http.StatusUnauthorized)
			return
		}

		// Retrieve email claim
		emailClaim, ok := decodedToken.Claims["email"]
		if !ok {
			log.Printf("[Auth] Missing email claim in ID token")
			http.Error(w, "Missing email claim in ID token", http.StatusForbidden)
			return
		}
		email, ok := emailClaim.(string)
		if !ok {
			log.Printf("[Auth] Invalid email claim format")
			http.Error(w, "Invalid email claim format", http.StatusForbidden)
			return
		}

		// Restrict authentication access to authorized domains or specific email addresses
		allowedDomainsEnv := os.Getenv("ALLOWED_EMAIL_DOMAINS")
		allowedDomains := []string{"google.com", "cloudadvocacyorg.joonix.net"}
		if allowedDomainsEnv != "" {
			parts := strings.Split(allowedDomainsEnv, ",")
			var trimmed []string
			for _, p := range parts {
				trimmed = append(trimmed, strings.TrimSpace(p))
			}
			if len(trimmed) > 0 {
				allowedDomains = trimmed
			}
		}

		if !isEmailAuthorized(email, allowedDomains) {
			log.Printf("[Auth] Access denied for unauthorized email: %s", email)
			http.Error(w, "Access restricted to authorized domains or specific email addresses", http.StatusForbidden)
			return
		}

		var errCookie error
		cookieString, errCookie = as.Client.SessionCookie(r.Context(), idToken, expiresIn)
		if errCookie != nil {
			log.Printf("[Auth] Error creating session cookie: %v", errCookie)
			http.Error(w, "Failed to create session", http.StatusUnauthorized)
			return
		}
	}

	// Write secure, HTTP-only session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    cookieString,
		MaxAge:   int(expiresIn.Seconds()),
		Path:     "/",
		HttpOnly: true,
		Secure:   !isLocalDev, // Require secure HTTPS only when not in local dev
		SameSite: http.SameSiteLaxMode,
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success"}`))
}

// Logout clears the secure session cookie and redirects.
func (as *AuthStore) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session")
	isLocalDev := os.Getenv("LOCAL_DEV") == "true"

	if err == nil && (!isLocalDev || cookie.Value != "dev-admin-session-cookie") {
		if as.Client != nil {
			// Revoke session on Firebase server side for security
			decoded, err := as.Client.VerifySessionCookie(r.Context(), cookie.Value)
			if err == nil {
				_ = as.Client.RevokeRefreshTokens(r.Context(), decoded.UID)
			}
		}
	}

	// Delete local session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		Secure:   !isLocalDev,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/login", http.StatusFound)
}

// Middleware verifies the session cookie and intercepts unauthorized requests to `/admin/*`.
func (as *AuthStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			// No session cookie, redirect to login
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		isLocalDev := os.Getenv("LOCAL_DEV") == "true"
		var uid string

		if isLocalDev && cookie.Value == "dev-admin-session-cookie" {
			uid = "dev-admin-uid"
		} else {
			if as.Client == nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			// Verify session cookie (checkRevoked=true is recommended)
			decodedToken, err := as.Client.VerifySessionCookieAndCheckRevoked(r.Context(), cookie.Value)
			if err != nil {
				log.Printf("[Auth Middleware] Invalid or revoked session cookie: %v", err)
				// Delete corrupt/revoked cookie and redirect
				http.SetCookie(w, &http.Cookie{
					Name:   "session",
					Value:  "",
					MaxAge: -1,
					Path:   "/",
				})
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			uid = decodedToken.UID
		}

		// Set UID in context so templates/handlers can retrieve user details
		ctx := context.WithValue(r.Context(), uidContextKey, uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isEmailAuthorized checks if the provided email is authorized based on a list of specific emails or domains.
func isEmailAuthorized(email string, allowedList []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, entry := range allowedList {
		entry = strings.ToLower(strings.TrimSpace(entry))
		// Trim leading '@' if user input includes it (e.g., @gmail.com)
		cleanedEntry := strings.TrimPrefix(entry, "@")
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
