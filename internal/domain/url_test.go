package domain_test

import (
	"errors"
	"testing"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func TestNormalizeURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "adds https scheme",
			input: "example.com/product/1",
			want:  "https://example.com/product/1",
		},
		{
			name:  "lowercases host and keeps path case",
			input: "https://Example.COM/Product/Ab",
			want:  "https://example.com/Product/Ab",
		},
		{
			name:  "strips utm parameters and fragment",
			input: "https://shop.example.com/p/42?utm_source=tg&utm_campaign=x&color=red#reviews",
			want:  "https://shop.example.com/p/42?color=red",
		},
		{
			name:  "keeps meaningful query parameters",
			input: "https://shop.example.com/p?id=42&size=M",
			want:  "https://shop.example.com/p?id=42&size=M",
		},
		{
			name:  "adds root path",
			input: "https://example.com",
			want:  "https://example.com/",
		},
		{
			name:  "drops basic auth credentials",
			input: "https://user:secret@example.com/p/1",
			want:  "https://example.com/p/1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.NormalizeURL(tc.input)
			if err != nil {
				t.Fatalf("NormalizeURL(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeURLRejects(t *testing.T) {
	t.Parallel()

	// The scraper follows these URLs from a server, so anything that could reach
	// the internal network must be rejected at the front door.
	bad := []string{
		"",
		"   ",
		"ftp://example.com/file",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"http://localhost:8080/admin",
		"http://127.0.0.1/admin",
		"http://10.0.0.5/internal",
		"http://192.168.1.1/router",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/admin",
		"http://router.local/status",
	}

	for _, input := range bad {
		got, err := domain.NormalizeURL(input)
		if err == nil {
			t.Errorf("NormalizeURL(%q) = %q, want error", input, got)
			continue
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("NormalizeURL(%q) error %v does not wrap ErrValidation", input, err)
		}
	}
}

func TestHostOf(t *testing.T) {
	t.Parallel()

	if got := domain.HostOf("https://Shop.Example.com/p/1"); got != "shop.example.com" {
		t.Fatalf("HostOf = %q, want shop.example.com", got)
	}
	if got := domain.HostOf("://broken"); got != "" {
		t.Fatalf("HostOf(broken) = %q, want empty", got)
	}
}
