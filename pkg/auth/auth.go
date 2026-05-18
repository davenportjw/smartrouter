package auth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
)

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
		var err error
		cookieString, err = as.Client.SessionCookie(r.Context(), idToken, expiresIn)
		if err != nil {
			log.Printf("[Auth] Error creating session cookie: %v", err)
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
		ctx := context.WithValue(r.Context(), "uid", uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
