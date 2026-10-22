package main

import "testing"

func TestParseDomain(t *testing.T) {
	cases := []struct {
		input, base   string
		wantSub, want string
	}{
		{"api", "example.com", "api", "api.example.com"},
		{"api.example.com", "example.com", "api", "api.example.com"},
		{"foo.bar.com", "example.com", "foo", "foo.bar.com"},
		{"*.example.com", "example.com", "*", "*.example.com"},
		{"API.Example.COM", "example.com", "api", "api.example.com"},
		{"api", "", "api", "api"},
		{" dev ", "example.com", "dev", "dev.example.com"},
	}
	for _, c := range cases {
		sub, full := parseDomain(c.input, c.base)
		if sub != c.wantSub || full != c.want {
			t.Errorf("parseDomain(%q, %q) = (%q, %q), want (%q, %q)", c.input, c.base, sub, full, c.wantSub, c.want)
		}
	}
}

func TestValidDomainSpec(t *testing.T) {
	valid := []string{"api", "api.example.com", "*.example.com", "a-b.example.co.uk", "x.io"}
	invalid := []string{"", "*", "*.", "bad;rm -rf /", "a b.com", "exa mple.com", "-bad.example.com", "bad-.example.com", "a..b", "example.com/../../etc"}

	for _, d := range valid {
		if !validDomainSpec(d) {
			t.Errorf("validDomainSpec(%q) = false, want true", d)
		}
	}
	for _, d := range invalid {
		if validDomainSpec(d) {
			t.Errorf("validDomainSpec(%q) = true, want false", d)
		}
	}
}

func TestValidUpstream(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8080",
		"https://10.0.0.2:5000/api",
		"http://localhost:3000",
		"unix:/run/app.sock",
	}
	invalid := []string{
		"http://x; include /etc/passwd",
		"http://x}",
		"ftp://x",
		"http://a b",
		"http://x\"; server_name evil",
	}

	for _, u := range valid {
		if !validUpstream(u) {
			t.Errorf("validUpstream(%q) = false, want true", u)
		}
	}
	for _, u := range invalid {
		if validUpstream(u) {
			t.Errorf("validUpstream(%q) = true, want false", u)
		}
	}
}

func TestValidEmail(t *testing.T) {
	valid := []string{"admin@example.com", "a.b+c@sub.example.co.uk"}
	invalid := []string{"", "admin", "admin@localhost", "a b@example.com", "admin@example.com; rm -rf /"}

	for _, e := range valid {
		if !validEmail(e) {
			t.Errorf("validEmail(%q) = false, want true", e)
		}
	}
	for _, e := range invalid {
		if validEmail(e) {
			t.Errorf("validEmail(%q) = true, want false", e)
		}
	}
}

func TestCertBaseName(t *testing.T) {
	cases := map[string]string{
		"api.example.com": "api.example.com",
		"*.example.com":   "_wildcard.example.com",
		"plain":           "plain",
	}
	for input, want := range cases {
		if got := certBaseName(input); got != want {
			t.Errorf("certBaseName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSiteSpecNormalize(t *testing.T) {
	cfg := &Config{BaseDomain: "example.com", DefaultPort: 8080}

	spec := SiteSpec{Domain: "api"}
	if err := spec.normalize(cfg); err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if spec.Domain != "api.example.com" {
		t.Errorf("short domain not expanded: %q", spec.Domain)
	}
	if spec.Upstream != "http://127.0.0.1:8080" {
		t.Errorf("default upstream = %q", spec.Upstream)
	}

	wildcard := SiteSpec{Domain: "*.example.com"}
	if err := wildcard.normalize(cfg); err == nil {
		t.Error("wildcard without DNS provider should fail")
	}
	wildcard.DNSProvider = "cloudflare"
	if err := wildcard.normalize(cfg); err != nil {
		t.Errorf("wildcard with DNS provider should pass: %v", err)
	}

	injection := SiteSpec{Domain: "api.example.com", Upstream: "http://x; include /etc/passwd"}
	if err := injection.normalize(cfg); err == nil {
		t.Error("injected upstream should be rejected")
	}
}

func TestCertExpiry(t *testing.T) {
	got := certExpiry("Jan  2 15:04:05 2006 GMT")
	if got == nil {
		t.Fatal("certExpiry failed to parse a valid OpenSSL date")
	}
	if got.Day() != 2 || got.Year() != 2006 {
		t.Errorf("certExpiry parsed wrong date: %v", got)
	}
	if certExpiry("not a date") != nil {
		t.Error("certExpiry should return nil for garbage input")
	}
}
