package client //nolint:testpackage // Exercise the unexported issuer mapping directly.

import "testing"

func TestIssuerFor(t *testing.T) {
	explicit := "https://public.example.com/custom"
	empty := ""
	for _, tt := range []struct {
		name   string
		url    string
		issuer *string
		want   string
	}{
		{"base", "https://auth.example.com", nil, "https://auth.example.com/auth/v1/"},
		{"base slash", "https://auth.example.com/", nil, "https://auth.example.com/auth/v1/"},
		{"API path", "https://auth.example.com/auth/v1", nil, "https://auth.example.com/auth/v1/"},
		{"API path slash", "https://auth.example.com/auth/v1/", nil, "https://auth.example.com/auth/v1/"},
		{"explicit", "https://auth.example.com", &explicit, explicit},
		{"empty override", "https://auth.example.com", &empty, "https://auth.example.com/auth/v1/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuerFor(tt.url, tt.issuer); got != tt.want {
				t.Fatalf("issuerFor() = %q, want %q", got, tt.want)
			}
		})
	}
}
