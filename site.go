package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SiteSpec is the desired state of one HTTPS site. It is used by the
// imperative "cert add" command and by the declarative "cert apply" manifest.
type SiteSpec struct {
	Domain      string   `yaml:"domain"`
	Upstream    string   `yaml:"upstream"`
	Port        int      `yaml:"port,omitempty"`
	DNSProvider string   `yaml:"dns_provider,omitempty"`
	SANs        []string `yaml:"sans,omitempty"`
	Staging     bool     `yaml:"staging,omitempty"`
}

// provisionResult reports what happened to one site during a batch run.
type provisionResult struct {
	Domain string
	Action string // "created", "updated", "unchanged"
	Expiry string
	Err    error
}

// normalize validates the spec and fills derived fields (expanded domain,
// upstream URL).
func (s *SiteSpec) normalize(cfg *Config) error {
	if !validDomainSpec(s.Domain) {
		return fmt.Errorf("invalid domain %q (only letters, digits, hyphens, dots and a leading '*.' wildcard are allowed)", s.Domain)
	}
	if !strings.Contains(s.Domain, ".") && strings.TrimSpace(cfg.BaseDomain) == "" {
		return fmt.Errorf("%q is not a fully qualified domain and no base domain is configured", s.Domain)
	}

	// Expand short subdomains against the base domain.
	_, full := parseDomain(s.Domain, cfg.BaseDomain)
	s.Domain = full
	s.DNSProvider = strings.ToLower(strings.TrimSpace(s.DNSProvider))

	for i, san := range s.SANs {
		if !validDomainSpec(san) {
			return fmt.Errorf("invalid SAN %q", san)
		}
		_, fullSAN := parseDomain(san, cfg.BaseDomain)
		s.SANs[i] = fullSAN
	}

	upstream := resolveUpstream(s.Port, s.Upstream, cfg.DefaultPort)
	if !validUpstream(upstream) {
		return fmt.Errorf("invalid upstream %q (expected host[:port], http(s)://host[:port][/path] or unix:/path/to.sock)", upstream)
	}
	s.Upstream = upstream

	if isWildcard(s.Domain) && s.DNSProvider == "" {
		return fmt.Errorf("wildcard domain %q requires DNS-01: add --dns <provider> (or dns_provider in the manifest)", s.Domain)
	}
	return nil
}

// certBaseName maps a domain to a filesystem-safe name (wildcards included).
func certBaseName(domain string) string {
	return strings.ReplaceAll(domain, "*", "_wildcard")
}

// isWildcard reports whether the domain covers a whole subdomain tree.
func isWildcard(domain string) bool {
	return strings.HasPrefix(domain, "*.")
}

// certFilePaths returns the certificate and key paths for a domain.
func certFilePaths(certDir, domain string) (certPath, keyPath string) {
	base := filepath.Join(certDir, certBaseName(domain))
	return base + ".pem", base + ".key"
}

// siteConfPath returns the nginx config path for a domain.
func siteConfPath(cfg *Config, domain string) string {
	return filepath.Join(cfg.NginxConfDir, certBaseName(domain)+".conf")
}

// ==========================================
// Provisioning pipeline
// ==========================================

// provisionSite creates or updates one site end to end:
// certificate issuance (HTTP-01/DNS-01) → cert install → nginx config →
// config validation. It is safe to run for multiple sites concurrently since
// every site only touches its own files; shared files are handled by the
// caller (ensureSharedConfigs) before workers start.
func provisionSite(cfg *Config, spec SiteSpec, accountDir string) provisionResult {
	result := provisionResult{Domain: spec.Domain}

	confPath := siteConfPath(cfg, spec.Domain)
	certPath, keyPath := certFilePaths(cfg.CertDir, spec.Domain)

	existed := false
	if _, err := os.Stat(confPath); err == nil {
		existed = true
	}

	// 1. Issue (or re-issue) the certificate.
	fmt.Printf("   [%s] requesting certificate...\n", spec.Domain)
	resource, err := obtainCertificate(issueRequest{
		Domains:     append([]string{spec.Domain}, spec.SANs...),
		DNSProvider: spec.DNSProvider,
		WebrootDir:  cfg.WebrootDir,
		Email:       cfg.Email,
		AccountDir:  accountDir,
		Staging:     spec.Staging,
	})
	if err != nil {
		result.Err = err
		return result
	}

	// 2. Install certificate and key.
	if err := writePrivilegedFile(keyPath, resource.PrivateKey, 0600); err != nil {
		result.Err = fmt.Errorf("cannot write private key: %w", err)
		return result
	}
	if err := writePrivilegedFile(certPath, resource.Certificate, 0644); err != nil {
		result.Err = fmt.Errorf("cannot write certificate: %w", err)
		return result
	}

	// 3. Persist issuance metadata (used by unattended renewal).
	meta := &certMeta{
		Domain:      spec.Domain,
		SANs:        spec.SANs,
		DNSProvider: spec.DNSProvider,
		Staging:     spec.Staging,
		IssuedAt:    time.Now().UTC(),
		CertURL:     resource.CertURL,
	}
	if err := saveCertMeta(cfg.CertDir, meta); err != nil {
		result.Err = fmt.Errorf("cannot write certificate metadata: %w", err)
		return result
	}

	// 4. Render the nginx reverse proxy configuration.
	configData := NginxConfig{
		FullDomain:  spec.Domain,
		CertPath:    certPath,
		KeyPath:     keyPath,
		UpstreamURL: spec.Upstream,
		WebrootDir:  cfg.WebrootDir,
		ListenSSL:   sslListenDirective(),
	}
	if err := renderToFile("nginx", nginxTemplate, confPath, configData); err != nil {
		result.Err = fmt.Errorf("cannot write nginx configuration: %w", err)
		return result
	}

	// 5. Validate the nginx configuration before it is reloaded.
	nginxBin, err := findNginx()
	if err != nil {
		result.Err = err
		return result
	}
	if out, err := runPrivilegedOutput(nginxBin, "-t"); err != nil {
		result.Err = fmt.Errorf("nginx config test failed: %v\n%s", err, out)
		return result
	}

	if existed {
		result.Action = "updated"
	} else {
		result.Action = "created"
	}
	if expiry, err := certPEMExpiry(resource.Certificate); err == nil {
		result.Expiry = expiry.UTC().Format("2006-01-02")
	}
	return result
}

// preflightHTTP01 checks that the domain can actually complete an HTTP-01
// challenge through this nginx setup. If a foreign server block already serves
// the domain, the challenge request would hit that block and fail — better to
// say so up front than to burn ACME rate limit on a doomed attempt.
func preflightHTTP01(cfg *Config, spec SiteSpec) error {
	if spec.DNSProvider != "" {
		return nil // DNS-01 does not depend on this server's nginx
	}
	dump, err := nginxConfigDump()
	if err != nil {
		return nil // nginx missing — provisioning reports the real error later
	}
	if path := findServerNameConflict(dump, spec.Domain, siteConfPath(cfg, spec.Domain)); path != "" {
		return fmt.Errorf("domain %q is already served by an existing nginx server block (%s);\n"+
			"     the HTTP-01 challenge would be answered by that block and fail. Either disable that\n"+
			"     server block, or use DNS-01 which does not need port 80: cert add %s --dns <provider>",
			spec.Domain, path, spec.Domain)
	}
	return nil
}

// removeSite deletes everything belonging to a domain: nginx config,
// certificate, key and metadata.
func removeSite(cfg *Config, domain string) error {
	confPath := siteConfPath(cfg, domain)
	if _, err := os.Stat(confPath); err != nil {
		// Also accept removal by short subdomain (expanded against the base domain).
		_, full := parseDomain(domain, cfg.BaseDomain)
		candidate := siteConfPath(cfg, full)
		if _, err := os.Stat(candidate); err == nil {
			confPath, domain = candidate, full
		}
	}
	if _, err := os.Stat(confPath); err != nil {
		return fmt.Errorf("no configuration found for %s", domain)
	}

	if err := removePrivilegedFile(confPath); err != nil {
		return err
	}
	certPath, keyPath := certFilePaths(cfg.CertDir, domain)
	_ = removePrivilegedFile(certPath)
	_ = removePrivilegedFile(keyPath)
	_ = removePrivilegedFile(metaPath(cfg.CertDir, domain))
	return nil
}
