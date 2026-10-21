package config

import (
	"strings"
	"testing"
)

func TestHTTPAddressDefaultsAndConfiguredBindings(t *testing.T) {
	for _, address := range []string{"", ":8181", "0.0.0.0:8181", "127.0.0.1:8181", "[::]:8181", "[::1]:8181", "[fe80::1%eth0]:8181", "localhost:8181"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("HTTP_ADDR", address)
			got, err := HTTPAddress()
			if err != nil {
				t.Fatal(err)
			}
			want := address
			if want == "" {
				want = ":8080"
			}
			if got != want {
				t.Fatalf("HTTP address=%q, want %q", got, want)
			}
		})
	}
}

func TestHTTPAddressRejectsInvalidBindingsBeforeServing(t *testing.T) {
	for _, address := range []string{":0", ":65536", ":-1", ":+8080", ":http", ":", "localhost", "::1:8181", "https://user:private@host:8181", "host/path:8181", "host name:8181", "[invalid::ip]:8181"} {
		t.Run(address, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("HTTP_ADDR", address)
			_, err := HTTPAddress()
			if err == nil || !strings.HasPrefix(err.Error(), "HTTP_ADDR ") {
				t.Fatalf("invalid HTTP address returned %v", err)
			}
			if _, err := Load(); err == nil {
				t.Fatal("application configuration accepted an invalid HTTP address")
			}
		})
	}
}
