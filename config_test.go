package contentkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchemasRefuseSharedQueues(t *testing.T) {
	scope := &Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	got, err := Config{Scope: scope, Database: DatabaseConfig{Schema: "site"}, Media: &MediaConfig{}}.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	if got != (Schemas{Catalog: "site", River: "site_river", Accounts: "contentkit_accounts", MediaWorker: "site_media_worker"}) {
		t.Fatalf("defaults: %+v", got)
	}
	if _, err := (Config{Scope: scope, Database: DatabaseConfig{Schema: "site"}, Accounts: &AccountsConfig{Schema: "site"}}).Schemas(); err != nil {
		t.Fatalf("catalog and account tables may share a schema: %v", err)
	}
	for name, c := range map[string]Config{
		"river in the catalog":        {Database: DatabaseConfig{Schema: "site", RiverSchema: "site"}},
		"accounts in river":           {Database: DatabaseConfig{Schema: "site"}, Accounts: &AccountsConfig{Schema: "site_river"}},
		"media worker is river":       {Database: DatabaseConfig{Schema: "site"}, Media: &MediaConfig{WorkerSchema: "site_river"}},
		"media worker in accounts":    {Database: DatabaseConfig{Schema: "site"}, Accounts: &AccountsConfig{Schema: "shared"}, Media: &MediaConfig{WorkerSchema: "shared"}},
		"media worker in the catalog": {Database: DatabaseConfig{Schema: "site"}, Media: &MediaConfig{WorkerSchema: "site"}},
	} {
		c.Scope = scope
		if _, err := c.Schemas(); err == nil || !strings.Contains(err.Error(), "both schema") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClickHouseTLS(t *testing.T) {
	if c, err := (ClickHouseConfig{}).tlsConfig(); c != nil || err != nil {
		t.Fatalf("plain: %v, %v", c, err)
	}
	if _, err := (ClickHouseConfig{CAFile: "ca.pem"}).tlsConfig(); err == nil {
		t.Fatal("a CA without TLS was accepted")
	}
	if c, err := (ClickHouseConfig{TLS: true}).tlsConfig(); err != nil || c == nil || c.RootCAs != nil {
		t.Fatalf("system roots: %v, %v", c, err)
	}
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (ClickHouseConfig{TLS: true, CAFile: junk}).tlsConfig(); err == nil {
		t.Fatal("a CA file without a certificate was accepted")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := (ClickHouseConfig{TLS: true, CAFile: ca}).tlsConfig(); err != nil || c.RootCAs == nil {
		t.Fatalf("CA file: %v, %v", c, err)
	}
}
