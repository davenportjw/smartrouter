package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/api/idtoken"
)

// Environment variables that pin which Google OIDC tokens the admin API accepts.
const (
	// EnvOIDCAudiences is a comma-separated list of backend URLs a token's aud claim must match.
	EnvOIDCAudiences = "API_OIDC_AUDIENCES"
	// EnvAllowedCallers is a comma-separated list of service-account emails allowed to call the admin API.
	EnvAllowedCallers = "API_ALLOWED_CALLERS"
)

var (
	errOIDCDisabled     = errors.New("oidc disabled: API_OIDC_AUDIENCES and API_ALLOWED_CALLERS must both be set")
	errOIDCAudience     = errors.New("oidc token not valid for any allowed audience")
	errEmailNotVerified = errors.New("oidc token email is not verified")
	errCallerNotAllowed = errors.New("oidc token caller is not allowed")
)

// validateIDToken is the Google ID token verifier; overridable in tests.
var validateIDToken = idtoken.Validate

// oidcPolicy is the audience + caller allowlist applied to Bearer ID tokens.
type oidcPolicy struct {
	audiences []string
	callers   map[string]struct{}
}

// parseCSV splits a comma-separated list, trimming blanks and dropping empty entries.
func parseCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newOIDCPolicy builds a policy from raw comma-separated audiences and caller emails.
func newOIDCPolicy(audiencesCSV, callersCSV string) oidcPolicy {
	p := oidcPolicy{audiences: parseCSV(audiencesCSV), callers: map[string]struct{}{}}
	for _, c := range parseCSV(callersCSV) {
		p.callers[strings.ToLower(c)] = struct{}{}
	}
	return p
}

// loadOIDCPolicy reads the policy from API_OIDC_AUDIENCES and API_ALLOWED_CALLERS.
func loadOIDCPolicy() oidcPolicy {
	return newOIDCPolicy(os.Getenv(EnvOIDCAudiences), os.Getenv(EnvAllowedCallers))
}

// enabled reports whether OIDC can authenticate at all (both lists non-empty).
func (p oidcPolicy) enabled() bool {
	return len(p.audiences) > 0 && len(p.callers) > 0
}

// checkCaller requires a verified email claim that is in the caller allowlist.
func (p oidcPolicy) checkCaller(claims map[string]interface{}) error {
	verified := false
	switch v := claims["email_verified"].(type) {
	case bool:
		verified = v
	case string:
		verified = strings.EqualFold(v, "true")
	}
	if !verified {
		return errEmailNotVerified
	}
	email, _ := claims["email"].(string)
	if _, ok := p.callers[strings.ToLower(strings.TrimSpace(email))]; !ok || email == "" {
		return fmt.Errorf("%w: %q", errCallerNotAllowed, email)
	}
	return nil
}

// authorize validates the token against each allowed audience, then checks the caller.
// It fails closed when the policy is not fully configured.
func (p oidcPolicy) authorize(ctx context.Context, token string) error {
	if !p.enabled() {
		return errOIDCDisabled
	}
	for _, aud := range p.audiences {
		payload, err := validateIDToken(ctx, token, aud)
		if err != nil || payload == nil {
			continue
		}
		return p.checkCaller(payload.Claims)
	}
	return errOIDCAudience
}
