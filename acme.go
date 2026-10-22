package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

// ==========================================
// ACME engine (built on github.com/go-acme/lego)
// ==========================================

const (
	defaultAccountDir = "/etc/cert-tool/account"
	accountKeyFile    = "account.key"
	accountRegFile    = "account.json"

	caProduction = lego.LEDirectoryProduction
	caStaging    = lego.LEDirectoryStaging
)

// acmeUser implements registration.User on top of a local account key.
type acmeUser struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }

// issueRequest describes one certificate issuance.
type issueRequest struct {
	Domains     []string // first entry becomes the CN
	DNSProvider string   // empty = HTTP-01 via webroot
	WebrootDir  string   // used for HTTP-01
	Email       string   // ACME account e-mail
	AccountDir  string   // where the ACME account key/registration live
	Staging     bool     // use the Let's Encrypt staging environment
}

// obtainCertificate runs a full ACME order and returns the issued certificate.
// Challenge solving is delegated to the shared webroot (HTTP-01) or to a DNS
// provider (DNS-01, required for wildcard domains).
func obtainCertificate(req issueRequest) (*certificate.Resource, error) {
	user, err := loadACMEUser(req.AccountDir, req.Email)
	if err != nil {
		return nil, err
	}

	config := lego.NewConfig(user)
	config.CADirURL = caProduction
	if req.Staging {
		config.CADirURL = caStaging
	}
	config.Certificate.KeyType = certcrypto.EC256
	config.UserAgent = "cert-tool/" + version

	client, err := lego.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create ACME client: %w", err)
	}

	// Register (idempotent: an existing account key returns the existing account).
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("ACME account registration failed: %w", err)
	}
	user.reg = reg
	if err := saveACMERegistration(req.AccountDir, reg); err != nil {
		return nil, err
	}

	// Set up the challenge solver.
	if req.DNSProvider != "" {
		provider, err := newDNSProvider(req.DNSProvider)
		if err != nil {
			return nil, err
		}
		if err := client.Challenge.SetDNS01Provider(provider); err != nil {
			return nil, fmt.Errorf("cannot use DNS provider %q: %w", req.DNSProvider, err)
		}
	} else {
		if err := client.Challenge.SetHTTP01Provider(newWebrootProvider(req.WebrootDir)); err != nil {
			return nil, fmt.Errorf("cannot use HTTP-01 webroot provider: %w", err)
		}
	}

	resource, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: req.Domains,
		Bundle:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("certificate issuance failed: %w", err)
	}
	return resource, nil
}

// ==========================================
// ACME account storage
// ==========================================

// loadACMEUser loads (or creates) the ACME account key and registration.
// An empty e-mail is allowed: the account is simply registered without a
// contact address (the ACME CA cannot send expiry reminders then).
func loadACMEUser(dir, email string) (*acmeUser, error) {
	if err := ensureDir(dir, 0700); err != nil {
		return nil, err
	}

	key, err := loadOrCreateAccountKey(dir)
	if err != nil {
		return nil, err
	}

	user := &acmeUser{email: email, key: key}
	user.reg = loadACMERegistration(dir)
	return user, nil
}

// loadOrCreateAccountKey reads the account key, creating an ECDSA P-256 key on
// first use. The key identifies the ACME account — losing it loses the account.
func loadOrCreateAccountKey(dir string) (crypto.PrivateKey, error) {
	keyPath := filepath.Join(dir, accountKeyFile)

	if data, err := readFilePrivileged(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("cannot decode account key %s", keyPath)
		}
		if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			return key.(crypto.PrivateKey), nil
		}
		if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
			return key, nil
		}
		return nil, fmt.Errorf("cannot parse account key %s", keyPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writePrivilegedFile(keyPath, pemData, 0600); err != nil {
		return nil, fmt.Errorf("cannot store account key: %w", err)
	}
	return key, nil
}

func loadACMERegistration(dir string) *registration.Resource {
	data, err := readFilePrivileged(filepath.Join(dir, accountRegFile))
	if err != nil {
		return nil
	}
	reg := &registration.Resource{}
	if err := json.Unmarshal(data, reg); err != nil {
		return nil
	}
	if reg.URI == "" {
		return nil
	}
	return reg
}

func saveACMERegistration(dir string, reg *registration.Resource) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return writePrivilegedFile(filepath.Join(dir, accountRegFile), data, 0600)
}

// ==========================================
// HTTP-01 webroot provider
// ==========================================

// webrootProvider serves ACME HTTP-01 challenges from a shared webroot that is
// already exposed by nginx on port 80, so issuance never needs a free port and
// never disturbs running sites.
type webrootProvider struct {
	root string
}

func newWebrootProvider(root string) challenge.Provider {
	return &webrootProvider{root: root}
}

func (w *webrootProvider) challengePath(token string) string {
	return filepath.Join(w.root, ".well-known", "acme-challenge", token)
}

func (w *webrootProvider) Present(domain, token, keyAuth string) error {
	if err := ensureDir(filepath.Dir(w.challengePath(token)), 0755); err != nil {
		return err
	}
	return writePrivilegedFile(w.challengePath(token), []byte(keyAuth), 0644)
}

func (w *webrootProvider) CleanUp(domain, token, keyAuth string) error {
	_ = removePrivilegedFile(w.challengePath(token))
	return nil
}

func (w *webrootProvider) Timeout() (time.Duration, time.Duration) {
	return 60 * time.Second, 2 * time.Second
}

// ==========================================
// Certificate metadata & inspection
// ==========================================

// certMeta is stored next to each certificate so that unattended renewal knows
// exactly how the certificate was obtained.
type certMeta struct {
	Domain      string    `json:"domain"`
	SANs        []string  `json:"sans,omitempty"`
	DNSProvider string    `json:"dns_provider,omitempty"` // empty = HTTP-01
	Staging     bool      `json:"staging,omitempty"`
	IssuedAt    time.Time `json:"issued_at"`
	CertURL     string    `json:"cert_url,omitempty"`
}

func metaPath(certDir, domain string) string {
	return filepath.Join(certDir, certBaseName(domain)+".meta.json")
}

func loadCertMeta(certDir, domain string) *certMeta {
	data, err := readFilePrivileged(metaPath(certDir, domain))
	if err != nil {
		return nil
	}
	meta := &certMeta{}
	if err := json.Unmarshal(data, meta); err != nil {
		return nil
	}
	return meta
}

func saveCertMeta(certDir string, meta *certMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writePrivilegedFile(metaPath(certDir, meta.Domain), data, 0644)
}

// certNotAfter returns the expiry time of a stored certificate.
func certNotAfter(certPath string) (*time.Time, error) {
	data, err := readFilePrivileged(certPath)
	if err != nil {
		return nil, err
	}
	return certPEMExpiry(data)
}

// certPEMExpiry parses the first certificate in a PEM bundle.
func certPEMExpiry(pemData []byte) (*time.Time, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM data found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	notAfter := cert.NotAfter
	return &notAfter, nil
}
