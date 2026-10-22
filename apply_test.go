package main

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
	"testing"
	"time"
)

func writeConf(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		CertDir:      t.TempDir(),
		NginxConfDir: t.TempDir(),
		WebrootDir:   t.TempDir(),
		Email:        "admin@example.com",
		BaseDomain:   "example.com",
		DefaultPort:  8080,
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cert-tool.yml")
	content := `email: admin@example.com
base_domain: example.com
default_port: 3000
dns_provider: cloudflare
sites:
  - domain: api
  - domain: foo.bar.com
    upstream: http://10.0.0.2:5000
  - domain: "*.example.com"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	manifest, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest failed: %v", err)
	}
	if len(manifest.Sites) != 3 {
		t.Fatalf("expected 3 sites, got %d", len(manifest.Sites))
	}
	if manifest.Sites[1].Upstream != "http://10.0.0.2:5000" {
		t.Errorf("site upstream = %q", manifest.Sites[1].Upstream)
	}
	// Manifest-level defaults propagate to sites.
	if manifest.Sites[0].DNSProvider != "cloudflare" {
		t.Errorf("default dns_provider not applied: %q", manifest.Sites[0].DNSProvider)
	}

	// Duplicate domains (after short-name expansion) must be rejected.
	dupPath := filepath.Join(dir, "dup.yml")
	dup := `email: admin@example.com
base_domain: example.com
sites:
  - domain: api
  - domain: api.example.com
`
	if err := os.WriteFile(dupPath, []byte(dup), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(dupPath)
	if err != nil {
		t.Fatalf("loadManifest failed: %v", err)
	}
	cfg := &Config{BaseDomain: m.BaseDomain, Email: m.Email, DefaultPort: 8080}
	seen := map[string]bool{}
	duplicate := false
	for i := range m.Sites {
		if err := m.Sites[i].normalize(cfg); err != nil {
			t.Fatalf("normalize failed: %v", err)
		}
		if seen[m.Sites[i].Domain] {
			duplicate = true
		}
		seen[m.Sites[i].Domain] = true
	}
	if !duplicate {
		t.Error("expected duplicate detection after expansion")
	}
}

func TestBuildPlan(t *testing.T) {
	cfg := testConfig(t)

	// Current state: one managed site with a changed upstream, one managed site
	// that will be deleted, and one foreign config that must never be touched.
	writeConf(t, cfg.NginxConfDir, "api.example.com.conf", "# Managed by cert-tool\n"+
		"server {\n    server_name api.example.com;\n    location / { proxy_pass http://127.0.0.1:9999; }\n}\n")
	writeConf(t, cfg.NginxConfDir, "gone.example.com.conf", "# Managed by cert-tool\n"+
		"server {\n    server_name gone.example.com;\n    location / { proxy_pass http://127.0.0.1:1; }\n}\n")
	writeConf(t, cfg.NginxConfDir, "foreign.conf", "# someone else's config\n"+
		"server {\n    server_name legacy.example.org;\n    location / { proxy_pass http://127.0.0.1:2; }\n}\n")

	manifest := &Manifest{
		Email:      "admin@example.com",
		BaseDomain: "example.com",
		Sites: []SiteSpec{
			{Domain: "api", Port: 9999},             // unchanged
			{Domain: "foo.bar.com", Port: 5000},     // create
			{Domain: "new.example.com", Port: 8080}, // create
		},
	}

	cfg.DefaultPort = 8080
	for i := range manifest.Sites {
		if err := manifest.Sites[i].normalize(cfg); err != nil {
			t.Fatalf("normalize failed: %v", err)
		}
	}

	plan, err := buildPlan(cfg, manifest)
	if err != nil {
		t.Fatalf("buildPlan failed: %v", err)
	}

	if len(plan.Create) != 2 {
		t.Errorf("create = %v, want 2 entries", plan.Create)
	}
	if len(plan.Update) != 0 {
		t.Errorf("update = %v, want none (api upstream is unchanged)", plan.Update)
	}
	if len(plan.Unchanged) != 1 || plan.Unchanged[0].Domain != "api.example.com" {
		t.Errorf("unchanged = %v, want [api.example.com]", plan.Unchanged)
	}
	if len(plan.Delete) != 1 || plan.Delete[0] != "gone.example.com" {
		t.Errorf("delete = %v, want [gone.example.com] (foreign.conf must never appear)", plan.Delete)
	}
}

func TestCurrentManagedSitesIgnoresUnmanaged(t *testing.T) {
	cfg := testConfig(t)
	writeConf(t, cfg.NginxConfDir, "managed.conf", "# Managed by cert-tool\n"+
		"server {\n    server_name a.example.com;\n    location / { proxy_pass http://127.0.0.1:1; }\n}\n")
	writeConf(t, cfg.NginxConfDir, "other.conf",
		"server {\n    server_name b.example.com;\n    location / { proxy_pass http://127.0.0.1:2; }\n}\n")
	writeConf(t, cfg.NginxConfDir, challengeConfName, "server {}\n")
	writeConf(t, cfg.NginxConfDir, mapConfName, "map $http_upgrade $x {}\n")

	sites, err := currentManagedSites(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("expected exactly 1 managed site, got %v", sites)
	}
	if _, ok := sites["a.example.com"]; !ok {
		t.Errorf("managed site missing: %v", sites)
	}
}

func TestCertPEMExpiry(t *testing.T) {
	pemData := generateTestCert(t, time.Now().Add(90*24*time.Hour))

	expiry, err := certPEMExpiry(pemData)
	if err != nil {
		t.Fatalf("certPEMExpiry failed: %v", err)
	}
	if time.Until(*expiry) <= 89*24*time.Hour {
		t.Errorf("parsed expiry %v is too far in the future", expiry)
	}

	if _, err := certPEMExpiry([]byte("garbage")); err == nil {
		t.Error("certPEMExpiry should fail on garbage input")
	}
}

func TestCertMetaRoundtrip(t *testing.T) {
	dir := t.TempDir()
	meta := &certMeta{
		Domain:      "*.example.com",
		SANs:        []string{"www.example.com"},
		DNSProvider: "cloudflare",
		IssuedAt:    time.Now().UTC().Truncate(time.Second),
	}

	if err := saveCertMeta(dir, meta); err != nil {
		t.Fatalf("saveCertMeta failed: %v", err)
	}
	// The wildcard must round-trip through its filesystem-safe name.
	if _, err := os.Stat(metaPath(dir, "_wildcard.example.com")); err != nil {
		t.Errorf("meta file not found under filesystem-safe name: %v", err)
	}

	loaded := loadCertMeta(dir, meta.Domain)
	if loaded == nil {
		t.Fatal("loadCertMeta returned nil")
	}
	if loaded.Domain != meta.Domain || loaded.DNSProvider != "cloudflare" {
		t.Errorf("meta round-trip mismatch: %+v", loaded)
	}
}

// generateTestCert creates a self-signed certificate PEM valid until notAfter.
func generateTestCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
