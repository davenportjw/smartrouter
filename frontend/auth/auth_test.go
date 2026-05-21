package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsEmailAuthorized(t *testing.T) {
	allowedList := []string{
		"google.com",
		"cloudadvocacyorg.joonix.net",
		"admin@example.com",
		"@special-team.org",
	}

	tests := []struct {
		name     string
		email    string
		expected bool
	}{
		{
			name:     "Exact match on allowed domain",
			email:    "operator@google.com",
			expected: true,
		},
		{
			name:     "Exact match on other allowed domain",
			email:    "user@cloudadvocacyorg.joonix.net",
			expected: true,
		},
		{
			name:     "Exact match on specific allowed email address",
			email:    "admin@example.com",
			expected: true,
		},
		{
			name:     "Exact match on domain with leading @ in config",
			email:    "leader@special-team.org",
			expected: true,
		},
		{
			name:     "Denied email in disallowed domain",
			email:    "user@example.com",
			expected: false,
		},
		{
			name:     "Denied email on random domain",
			email:    "hacker@random.org",
			expected: false,
		},
		{
			name:     "Case insensitivity test for email and pattern matching",
			email:    "Admin@EXAMPLE.com",
			expected: true,
		},
		{
			name:     "Case insensitivity test for domains",
			email:    "Operator@Google.Com",
			expected: true,
		},
		{
			name:     "Whitespace trimming test",
			email:    "   admin@example.com   ",
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := isEmailAuthorized(tc.email, allowedList)
			if actual != tc.expected {
				t.Errorf("isEmailAuthorized(%q, %v) = %v; want %v", tc.email, allowedList, actual, tc.expected)
			}
		})
	}
}

func TestNewAuthStoreLocalDev(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")

	ctx := context.Background()
	store, err := NewAuthStore(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Client != nil {
		t.Errorf("expected firebase auth client to be nil in local dev mode")
	}
}

func TestCreateSession(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()
	store, _ := NewAuthStore(ctx)

	t.Run("Method Not Allowed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/session", nil)
		rr := httptest.NewRecorder()
		store.CreateSession(rr, req)

		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected status 405, got %d", rr.Code)
		}
	})

	t.Run("Missing idToken", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/session", nil)
		rr := httptest.NewRecorder()
		store.CreateSession(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", rr.Code)
		}
	})

	t.Run("Successful Local Dev Session", func(t *testing.T) {
		form := url.Values{}
		form.Set("idToken", "dev-admin-token")
		req := httptest.NewRequest("POST", "/session", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()

		store.CreateSession(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}

		cookies := rr.Result().Cookies()
		foundSessionCookie := false
		for _, cookie := range cookies {
			if cookie.Name == "session" {
				foundSessionCookie = true
				if cookie.Value != "dev-admin-session-cookie" {
					t.Errorf("unexpected session cookie value: %q", cookie.Value)
				}
				if !cookie.HttpOnly {
					t.Errorf("expected session cookie to be HttpOnly")
				}
			}
		}
		if !foundSessionCookie {
			t.Errorf("session cookie was not set in response")
		}
	})
}

func TestLogout(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()
	store, _ := NewAuthStore(ctx)

	req := httptest.NewRequest("GET", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "dev-admin-session-cookie"})
	rr := httptest.NewRecorder()

	store.Logout(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("expected status 302 (found), got %d", rr.Code)
	}

	// Verify secure cookie was deleted (MaxAge = -1)
	cookies := rr.Result().Cookies()
	foundDeletedCookie := false
	for _, cookie := range cookies {
		if cookie.Name == "session" {
			foundDeletedCookie = true
			if cookie.MaxAge != -1 && cookie.Value != "" {
				t.Errorf("expected session cookie to be deleted, got: %+v", cookie)
			}
		}
	}
	if !foundDeletedCookie {
		t.Errorf("expected deleted session cookie in response")
	}
}

func TestMiddleware(t *testing.T) {
	t.Setenv("LOCAL_DEV", "true")
	ctx := context.Background()
	store, _ := NewAuthStore(ctx)

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Context().Value(uidContextKey)
		if uid != "dev-admin-uid" {
			t.Errorf("expected context uid 'dev-admin-uid', got %v", uid)
		}
		w.WriteHeader(http.StatusOK)
	})

	middleware := store.Middleware(nextHandler)

	t.Run("Missing Cookie Redirects", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/dashboard", nil)
		rr := httptest.NewRecorder()

		middleware.ServeHTTP(rr, req)

		if rr.Code != http.StatusFound {
			t.Errorf("expected status 302, got %d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/login" {
			t.Errorf("expected redirect to /login, got %q", loc)
		}
	})

	t.Run("Valid Session Cookie Passes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/admin/dashboard", nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "dev-admin-session-cookie"})
		rr := httptest.NewRecorder()

		middleware.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}
	})
}
