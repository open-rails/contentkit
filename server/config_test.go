package server

import (
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestLoadTransport(t *testing.T) {
	c, err := Load([]byte(`
engine:
  trusted_proxies: [10.0.0.0/8, "fd00::/8"]
  clickhouse: {tls: true, ca_file: /etc/clickhouse/ca.pem}
read_timeout: 30s
idle_timeout: 2m
`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	if !slices.Equal(c.Engine.TrustedProxies, want) || !c.Engine.ClickHouse.TLS || c.Engine.ClickHouse.CAFile != "/etc/clickhouse/ca.pem" ||
		c.ReadTimeout != 30*time.Second || c.IdleTimeout != 2*time.Minute {
		t.Fatalf("loaded %+v", c)
	}
	if _, err := Load([]byte("engine: {trusted_proxies: [not-a-prefix]}"), func(string) string { return "" }); err == nil {
		t.Fatal("a malformed prefix loaded")
	}
}

func TestSecureEndpoint(t *testing.T) {
	for raw, ok := range map[string]bool{"https://openrails.example": true, "http://127.0.0.1:8080": true, "http://localhost": true, "http://[::1]:1": true,
		"http://openrails.example": false, "ftp://openrails.example": false, "openrails.example": false} {
		if err := secureEndpoint(raw); (err == nil) != ok {
			t.Errorf("secureEndpoint(%q) = %v", raw, err)
		}
	}
}
