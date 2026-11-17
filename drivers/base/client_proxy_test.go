package base

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

func TestNewRestyClientWithProxy(t *testing.T) {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig(t.TempDir())
	}

	t.Run("empty proxy returns nil", func(t *testing.T) {
		client, err := NewRestyClientWithProxy("  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client != nil {
			t.Fatal("expected nil client for empty proxy")
		}
	})

	t.Run("valid http proxy is applied", func(t *testing.T) {
		const want = "http://127.0.0.1:10808"
		client, err := NewRestyClientWithProxy(want)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		transport, ok := client.GetClient().Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport = %T, want *http.Transport", client.GetClient().Transport)
		}
		got, err := transport.Proxy(&http.Request{
			URL: &url.URL{Scheme: "https", Host: "api-drive.mypikpak.net"},
		})
		if err != nil {
			t.Fatalf("proxy func error: %v", err)
		}
		if got == nil || got.String() != want {
			t.Fatalf("proxy = %v, want %s", got, want)
		}
	})

	t.Run("invalid proxy is rejected", func(t *testing.T) {
		for _, raw := range []string{"not-a-url", "ftp://example.com", "://bad"} {
			if _, err := NewRestyClientWithProxy(raw); err == nil {
				t.Fatalf("expected error for proxy %q", raw)
			}
		}
	})

	t.Run("socks5 proxy is accepted", func(t *testing.T) {
		client, err := NewRestyClientWithProxy("socks5://127.0.0.1:1080")
		if err != nil {
			t.Fatalf("socks5 proxy should be accepted: %v", err)
		}
		if client == nil {
			t.Fatal("expected non-nil client for socks5 proxy")
		}
	})
}
