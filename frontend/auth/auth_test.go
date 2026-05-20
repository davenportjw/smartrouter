package auth

import "testing"

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
