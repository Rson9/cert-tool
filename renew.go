package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ==========================================
// Unattended renewal: cert renew
// ==========================================

// renewalJob describes one certificate that is due for renewal.
type renewalJob struct {
	meta   *certMeta
	expiry time.Time
	result provisionResult
}

// handleRenew renews every managed certificate that is due. It is designed to
// run unattended from cron and is idempotent: certificates that are not close
// to expiry are left untouched unless --force is given.
func handleRenew(args []string, cfg *Config) {
	days := 30
	force := false
	quiet := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--days":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &days)
				i++
			}
		case strings.HasPrefix(arg, "--days="):
			fmt.Sscanf(arg[strings.Index(arg, "=")+1:], "%d", &days)
		case arg == "--force":
			force = true
		case arg == "--quiet" || arg == "-q":
			quiet = true
		}
	}
	if days < 1 {
		days = 30
	}

	if !quiet {
		fmt.Printf("%s (renewing certificates that expire within %d days)\n", yellow(bold("Certificate renewal")), days)
	}

	jobs := collectRenewals(cfg, days, force)
	if len(jobs) == 0 {
		if !quiet {
			fmt.Println(green("✅ All certificates are up to date."))
		}
		return
	}

	fmt.Printf("Renewing %d certificate(s) with %d worker(s)...\n", len(jobs), currentWorkers())
	sem := make(chan struct{}, currentWorkers())
	var wg sync.WaitGroup

	for i := range jobs {
		wg.Add(1)
		go func(job *renewalJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fmt.Printf("   [%s] renewing (expires %s)...\n", job.meta.Domain, job.expiry.Format("2006-01-02"))
			resource, err := obtainCertificate(issueRequest{
				Domains:     append([]string{job.meta.Domain}, job.meta.SANs...),
				DNSProvider: job.meta.DNSProvider,
				WebrootDir:  cfg.WebrootDir,
				Email:       cfg.Email,
				AccountDir:  defaultAccountDir,
				Staging:     job.meta.Staging,
			})
			if err != nil {
				job.result = provisionResult{Domain: job.meta.Domain, Err: err}
				return
			}

			certPath, keyPath := certFilePaths(cfg.CertDir, job.meta.Domain)
			if err := writePrivilegedFile(keyPath, resource.PrivateKey, 0600); err != nil {
				job.result = provisionResult{Domain: job.meta.Domain, Err: err}
				return
			}
			if err := writePrivilegedFile(certPath, resource.Certificate, 0644); err != nil {
				job.result = provisionResult{Domain: job.meta.Domain, Err: err}
				return
			}
			job.meta.IssuedAt = time.Now().UTC()
			job.meta.CertURL = resource.CertURL
			_ = saveCertMeta(cfg.CertDir, job.meta)

			job.result = provisionResult{Domain: job.meta.Domain, Action: "renewed"}
			if expiry, err := certPEMExpiry(resource.Certificate); err == nil {
				job.result.Expiry = expiry.UTC().Format("2006-01-02")
			}
		}(&jobs[i])
	}
	wg.Wait()

	failures := 0
	for _, job := range jobs {
		if job.result.Err != nil {
			failures++
			fmt.Printf("   %s %s: %v\n", red("✗"), job.meta.Domain, job.result.Err)
		} else {
			fmt.Printf("   %s %s: renewed, valid until %s\n", green("✓"), job.meta.Domain, job.result.Expiry)
		}
	}

	if failures == 0 {
		if err := reloadNginx(); err != nil {
			fmt.Printf("%s nginx reload failed: %v\n", yellow("Warning:"), err)
		}
	}

	if failures > 0 {
		fmt.Printf("%s renewal finished with %d failure(s)\n", red(bold("❌")), failures)
		os.Exit(1)
	}
	if !quiet {
		fmt.Println(green(bold("✅ Renewal complete.")))
	}
}

// collectRenewals lists certificates that are due for renewal.
func collectRenewals(cfg *Config, days int, force bool) []renewalJob {
	entries, err := os.ReadDir(cfg.CertDir)
	if err != nil {
		return nil
	}

	var jobs []renewalJob
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		domain := strings.TrimSuffix(entry.Name(), ".meta.json")
		meta := loadCertMeta(cfg.CertDir, domain)
		if meta == nil {
			continue
		}

		certPath, _ := certFilePaths(cfg.CertDir, meta.Domain)
		expiry, err := certNotAfter(certPath)
		if err != nil {
			// Metadata without a readable certificate: always re-issue.
			jobs = append(jobs, renewalJob{meta: meta})
			continue
		}

		remaining := time.Until(*expiry)
		if force || remaining <= time.Duration(days)*24*time.Hour {
			jobs = append(jobs, renewalJob{meta: meta, expiry: *expiry})
		}
	}
	return jobs
}

// ==========================================
// Renewal cron job (via /etc/cron.d)
// ==========================================

const renewalCronPath = "/etc/cron.d/cert-tool"

// installRenewalCron installs a daily cron job that runs "cert renew". Using
// /etc/cron.d (instead of editing crontabs) keeps the job visible, isolated
// and impossible to clobber with other cron entries.
func installRenewalCron(binaryPath string) error {
	content := fmt.Sprintf(`# Managed by cert-tool — daily certificate renewal.
# Certificates that are not within 30 days of expiry are skipped,
# so this job is safe to run as often as you like.
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MAILTO=""

17 3 * * * root %s renew --quiet >> /var/log/cert-tool-renew.log 2>&1
`, binaryPath)
	return writePrivilegedFile(renewalCronPath, []byte(content), 0644)
}
