package config

import "testing"

func TestTelegramURLRequiresHTTPSOutsideLoopback(t *testing.T) {
	for url, want := range map[string]bool{
		"https://example.com":          true,
		"http://127.0.0.1:9999/bot":    true,
		"http://[::1]:9999/bot":        true,
		"http://localhost:9999":        true,
		"http://example.com":           false,
		"http://localhost.example.com": false,
		"http://127.0.0.1@example.com": false,
	} {
		if got := validTelegramURL(url); got != want {
			t.Errorf("validTelegramURL(%q) = %t, want %t", url, got, want)
		}
	}
}
