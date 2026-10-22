package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// ==========================================
// Shared helpers
// ==========================================

// renderToFile renders a template into a temp file and copies it to destPath
// with safe permissions.
func renderToFile(name, tmplBody, destPath string, data interface{}) error {
	tmpl, err := template.New(name).Parse(tmplBody)
	if err != nil {
		return fmt.Errorf("template error: %w", err)
	}

	tmp, err := os.CreateTemp("", "cert-tool-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmpl.Execute(tmp, data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return writePrivilegedFile(destPath, mustReadFile(tmp.Name()), 0644)
}

func mustReadFile(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// ensureSharedConfigs makes sure the shared ACME challenge server and the
// WebSocket upgrade map are in place, and resolves conflicts with the default
// "welcome" sites shipped by distributions. It is designed to coexist with a
// pre-existing nginx setup: foreign server blocks are never rewritten and the
// challenge endpoint only claims the port 80 default_server when free.
func ensureSharedConfigs(cfg *Config) error {
	// 1. Move aside conflicting distro default sites on port 80 (back up, never
	// delete, and record the move so 'cert uninstall' can restore it).
	if _, err := os.Stat("/etc/nginx/sites-enabled/default"); err == nil {
		fmt.Printf("   %s disabling conflicting default site -> /etc/nginx/sites-enabled/default.disabled-by-cert-tool\n", yellow("Note:"))
		const from = "/etc/nginx/sites-enabled/default"
		const to = "/etc/nginx/sites-enabled/default.disabled-by-cert-tool"
		if err := runPrivileged("mv", from, to); err == nil {
			recordMove(from, to)
		}
	}
	defaultConfPath := filepath.Join(cfg.NginxConfDir, "default.conf")
	if _, err := os.Stat(defaultConfPath); err == nil {
		fmt.Printf("   %s backing up conflicting %s -> %s.bak\n", yellow("Note:"), defaultConfPath, defaultConfPath)
		if err := runPrivileged("mv", defaultConfPath, defaultConfPath+".bak"); err == nil {
			recordMove(defaultConfPath, defaultConfPath+".bak")
		}
	}

	// 2. Shared upgrade map (referenced by every generated site config).
	mapConfPath := filepath.Join(cfg.NginxConfDir, mapConfName)
	if _, err := os.Stat(mapConfPath); err != nil {
		if err := renderToFile("upgrade-map", upgradeMapTemplate, mapConfPath, nil); err != nil {
			return err
		}
	}

	// 3. Shared ACME HTTP-01 challenge endpoint. Claim the port 80
	// default_server only when nothing else does — never hijack an existing
	// setup (a duplicate default_server prevents nginx from starting).
	challengeConfPath := filepath.Join(cfg.NginxConfDir, challengeConfName)
	challengeData := ChallengeConfig{WebrootDir: cfg.WebrootDir, DefaultServer: " default_server"}
	if dump, err := nginxConfigDump(); err == nil {
		if hasDefaultServerOnPort80(dump, challengeConfPath) {
			challengeData.DefaultServer = ""
		}
	}
	if err := renderToFile("challenge", challengeTemplate, challengeConfPath, challengeData); err != nil {
		return err
	}

	// 4. Make sure this nginx actually loads the tool's config directory —
	// otherwise generated configs would silently never take effect.
	if err := verifyConfDirLoaded(cfg, challengeConfPath); err != nil {
		return err
	}

	return reloadNginx()
}

// nginxConfigDump returns the fully expanded nginx configuration (`nginx -T`).
func nginxConfigDump() (string, error) {
	nginxBin, err := findNginx()
	if err != nil {
		return "", err
	}
	return runPrivilegedOutput(nginxBin, "-T")
}

// verifyConfDirLoaded checks that nginx includes the tool's config directory.
func verifyConfDirLoaded(cfg *Config, probePath string) error {
	dump, err := nginxConfigDump()
	if err != nil {
		return nil // nginx missing — a later config test gives a clearer error
	}
	if !confDirIsIncluded(dump, probePath) {
		return fmt.Errorf("your nginx configuration does not load %s (add `include %s/*.conf;` to the http block of your nginx.conf)", cfg.NginxConfDir, cfg.NginxConfDir)
	}
	return nil
}

// ==========================================
// Command: cert add
// ==========================================

func handleAdd(args []string, cfg *Config) {
	spec := SiteSpec{}
	customDomain := cfg.BaseDomain
	extraSANs := []string{}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-p" || arg == "--port":
			if i+1 < len(args) {
				p, _ := strconv.Atoi(args[i+1])
				if p > 0 && p <= 65535 {
					spec.Port = p
				}
				i++
			}
		case strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--port="):
			p, _ := strconv.Atoi(arg[strings.Index(arg, "=")+1:])
			if p > 0 && p <= 65535 {
				spec.Port = p
			}
		case arg == "-u" || arg == "--upstream":
			if i+1 < len(args) {
				spec.Upstream = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-u=") || strings.HasPrefix(arg, "--upstream="):
			spec.Upstream = arg[strings.Index(arg, "=")+1:]
		case arg == "-d" || arg == "--domain":
			if i+1 < len(args) {
				customDomain = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-d=") || strings.HasPrefix(arg, "--domain="):
			customDomain = arg[strings.Index(arg, "=")+1:]
		case arg == "--dns":
			if i+1 < len(args) {
				spec.DNSProvider = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--dns="):
			spec.DNSProvider = arg[strings.Index(arg, "=")+1:]
		case arg == "--san":
			if i+1 < len(args) {
				extraSANs = append(extraSANs, args[i+1])
				i++
			}
		case arg == "--staging":
			spec.Staging = true
		case !strings.HasPrefix(arg, "-") && spec.Domain == "":
			spec.Domain = arg
		}
	}

	if spec.Domain == "" {
		fmt.Println(red("Error: please provide a domain or subdomain"))
		fmt.Println("Usage: cert add <domain|subdomain> [-p <port>] [-u <upstream>] [-d <base-domain>] [--dns <provider>] [--san <domain>] [--staging]")
		os.Exit(1)
	}

	// Expand short subdomains against the base domain.
	if !strings.Contains(spec.Domain, ".") && customDomain == "" {
		fmt.Printf("%s %q is not a fully qualified domain and no base domain is configured\n", red("Error:"), spec.Domain)
		fmt.Printf("Provide a full domain (e.g. cert add %s.example.com) or set a base domain with -d / 'cert config set domain example.com'\n", spec.Domain)
		os.Exit(1)
	}
	_, fullDomain := parseDomain(spec.Domain, customDomain)
	spec.Domain = fullDomain
	for _, san := range extraSANs {
		_, fullSAN := parseDomain(san, customDomain)
		spec.SANs = append(spec.SANs, fullSAN)
	}

	if err := spec.normalize(cfg); err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}

	// An e-mail is optional but recommended (CA expiry reminders).
	if cfg.Email == "" {
		fmt.Printf("%s no ACME e-mail configured — you will not receive expiry reminders (optional: cert config set email you@example.com)\n", yellow("Note:"))
	} else if !validEmail(cfg.Email) {
		fmt.Printf("%s invalid ACME e-mail address configured (current: %q)\n", red("Error:"), cfg.Email)
		os.Exit(1)
	}

	fmt.Println(yellow("=================================================="))
	fmt.Printf("Setting up: %s\n", cyan(bold(spec.Domain)))
	fmt.Printf("Proxy target: %s\n", cyan(spec.Upstream))
	if spec.DNSProvider != "" {
		fmt.Printf("Challenge: DNS-01 (%s)\n", cyan(spec.DNSProvider))
	} else {
		fmt.Println("Challenge: HTTP-01 (shared webroot)")
	}
	fmt.Println(yellow("=================================================="))

	// Refuse to overwrite an existing site silently.
	if _, err := os.Stat(siteConfPath(cfg, spec.Domain)); err == nil {
		fmt.Printf("%s configuration already exists: %s\n", red("Error:"), siteConfPath(cfg, spec.Domain))
		fmt.Printf("To reconfigure it, remove it first: cert remove %s\n", spec.Domain)
		os.Exit(1)
	}

	fmt.Print("1. Preparing directories and ACME challenge endpoint... ")
	if err := ensureTrackedDir(cfg.CertDir, 0755); err != nil {
		fmt.Printf("%s %v\n", red("failed:"), err)
		os.Exit(1)
	}
	if err := ensureTrackedDir(cfg.WebrootDir, 0755); err != nil {
		fmt.Printf("%s %v\n", red("failed:"), err)
		os.Exit(1)
	}
	if err := ensureSharedConfigs(cfg); err != nil {
		fmt.Printf("%s failed to set up the ACME challenge endpoint: %v\n", red("failed:"), err)
		os.Exit(1)
	}
	fmt.Println(green("done"))

	// Coexistence check: if a foreign nginx server block already answers for
	// this domain, HTTP-01 cannot succeed — fail fast with alternatives.
	if err := preflightHTTP01(cfg, spec); err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}

	fmt.Println("2. Issuing certificate and generating the nginx configuration...")
	result := provisionSite(cfg, spec, defaultAccountDir)
	if result.Err != nil {
		fmt.Printf("%s %v\n", red("failed:"), result.Err)
		fmt.Println(yellow("Hint: for HTTP-01 the domain's DNS A record must point to this server and port 80 must be reachable; for DNS-01 check the provider credentials"))
		os.Exit(1)
	}

	fmt.Print("3. Reloading nginx... ")
	if err := reloadNginx(); err != nil {
		fmt.Printf("%s %v\n", red("failed:"), err)
		os.Exit(1)
	}
	fmt.Println(green("done"))

	fmt.Print("4. Ensuring automatic renewal (daily cron job)... ")
	if err := installRenewalCron(certBinaryPath()); err != nil {
		fmt.Printf("%s %v\n", yellow("warning:"), err)
	} else {
		fmt.Println(green("done"))
	}

	fmt.Println()
	fmt.Printf("%s HTTPS reverse proxy for %s is ready! (valid until %s)\n", green(bold("✅")), cyan(bold(spec.Domain)), result.Expiry)
	fmt.Println()
	fmt.Println("Quick test:")
	fmt.Printf("  curl -I https://%s\n", spec.Domain)
}

// ==========================================
// Command: cert remove
// ==========================================

func handleRemove(args []string, cfg *Config) {
	if len(args) == 0 {
		fmt.Println(red("Error: please provide the domain or subdomain to remove"))
		os.Exit(1)
	}

	raw := strings.ToLower(strings.TrimSpace(args[0]))
	if !validDomainSpec(raw) {
		fmt.Printf("%s invalid domain name %q\n", red("Error:"), args[0])
		os.Exit(1)
	}
	_, fullDomain := parseDomain(raw, cfg.BaseDomain)

	fmt.Println(yellow("Removing HTTPS configuration and certificate for " + fullDomain + "..."))

	if err := removeSite(cfg, fullDomain); err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("1. Nginx configuration removed")
	fmt.Println("2. Certificate, key and metadata removed")

	fmt.Print("3. Reloading nginx... ")
	_ = reloadNginx()
	fmt.Println(green("done"))

	fmt.Println()
	fmt.Printf("%s HTTPS configuration for %s has been removed.\n", green(bold("✅")), fullDomain)
}

// ==========================================
// Command: cert list
// ==========================================

func handleList(cfg *Config) {
	fmt.Println(yellow(bold("Configured HTTPS sites:")))
	fmt.Println()

	entries, err := os.ReadDir(cfg.NginxConfDir)
	if err != nil {
		fmt.Printf("%s cannot read nginx config directory %s: %v\n", red("Error:"), cfg.NginxConfDir, err)
		return
	}

	found := false
	serverNameRegex := regexp.MustCompile(`server_name\s+([^;]+);`)
	proxyPassRegex := regexp.MustCompile(`proxy_pass\s+([^;]+);`)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		if entry.Name() == challengeConfName || entry.Name() == mapConfName || entry.Name() == "default.conf" {
			continue
		}

		confPath := filepath.Join(cfg.NginxConfDir, entry.Name())
		contentBytes, err := os.ReadFile(confPath)
		if err != nil {
			continue
		}
		content := string(contentBytes)

		serverNames := serverNameRegex.FindAllStringSubmatch(content, -1)
		fullDomain := ""
		if len(serverNames) > 0 && len(serverNames[0]) > 1 {
			names := strings.Fields(strings.TrimSpace(serverNames[0][1]))
			if len(names) > 0 && names[0] != "_" {
				fullDomain = names[0]
			}
		}
		if fullDomain == "" {
			fullDomain = strings.TrimSuffix(entry.Name(), ".conf")
		}

		upstream := "unknown upstream"
		passMatches := proxyPassRegex.FindAllStringSubmatch(content, -1)
		if len(passMatches) > 0 && len(passMatches[0]) > 1 {
			upstream = strings.TrimSpace(passMatches[0][1])
		}

		certPath, _ := certFilePaths(cfg.CertDir, fullDomain)
		if _, err := os.Stat(certPath); err == nil {
			cmd := exec.Command("openssl", "x509", "-in", certPath, "-noout", "-enddate")
			output, err := cmd.Output()
			if err == nil {
				dateStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(output)), "notAfter="))
				expiry := certExpiry(dateStr)
				if expiry != nil {
					days := int(time.Until(*expiry).Hours() / 24)
					fmt.Printf("  %s %-30s -> %-24s (valid until %s, %d days left)\n", green("✓"), cyan(fullDomain), upstream, dateStr, days)
				} else {
					fmt.Printf("  %s %-30s -> %-24s (valid until %s)\n", green("✓"), cyan(fullDomain), upstream, dateStr)
				}
			} else {
				fmt.Printf("  %s %-30s -> %-24s (certificate present)\n", green("✓"), cyan(fullDomain), upstream)
			}
		} else {
			fmt.Printf("  %s %-30s -> %-24s (%s)\n", red("✗"), cyan(fullDomain), upstream, red("certificate file missing"))
		}
		found = true
	}

	if !found {
		fmt.Println("  (no sites configured yet)")
	}
}

// certExpiry parses an OpenSSL notAfter date such as "Jan 2 15:04:05 2026 GMT".
func certExpiry(dateStr string) *time.Time {
	normalized := strings.Join(strings.Fields(dateStr), " ")
	if t, err := time.Parse("Jan 2 15:04:05 2006 MST", normalized); err == nil {
		return &t
	}
	return nil
}

// ==========================================
// Command: cert test
// ==========================================

func handleTest(args []string, cfg *Config) {
	if len(args) == 0 {
		fmt.Println(red("Error: please provide the domain or subdomain to test"))
		os.Exit(1)
	}
	if !validDomainSpec(args[0]) {
		fmt.Printf("%s invalid domain name %q\n", red("Error:"), args[0])
		os.Exit(1)
	}

	subdomain, fullDomain := parseDomain(args[0], cfg.BaseDomain)
	fmt.Println(yellow("=================================================="))
	fmt.Printf("Testing HTTPS setup: %s\n", cyan(bold(fullDomain)))
	fmt.Println(yellow("=================================================="))
	fmt.Println()

	// 1. nginx configuration syntax.
	fmt.Println("1. Nginx configuration syntax:")
	nginxBin, err := findNginx()
	if err != nil {
		fmt.Printf("   %s %v\n", red("✗"), err)
	} else {
		if out, err := runPrivilegedOutput(nginxBin, "-t"); err != nil {
			fmt.Printf("   %s syntax error: %v\n   %s\n", red("✗"), err, out)
		} else {
			fmt.Printf("   %s nginx configuration is valid\n", green("✓"))
		}
	}
	fmt.Println()

	// 2. Public HTTPS connectivity.
	fmt.Println("2. HTTPS connectivity:")
	cmd := exec.Command("curl", "-I", "--connect-timeout", "5", "https://"+fullDomain)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("   %s request failed: %v\n", red("✗"), err)
		if len(output) > 0 {
			fmt.Printf("   %s\n", strings.TrimSpace(string(output)))
		}
	} else {
		found := false
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "HTTP/") {
				fmt.Printf("   %s response: %s\n", green("✓"), strings.TrimSpace(line))
				found = true
				break
			}
		}
		if !found && len(output) > 0 {
			fmt.Printf("   %s response:\n   %s\n", green("✓"), strings.TrimSpace(string(output)))
		}
	}
	fmt.Println()

	// 3. Upstream backend health.
	confPath := siteConfPath(cfg, fullDomain)
	if _, err := os.Stat(confPath); err != nil {
		confPath = siteConfPath(cfg, subdomain)
	}

	upstream := ""
	if data, err := os.ReadFile(confPath); err == nil {
		re := regexp.MustCompile(`proxy_pass\s+([^;]+);`)
		m := re.FindStringSubmatch(string(data))
		if len(m) > 1 {
			upstream = strings.TrimSpace(m[1])
		}
	}
	if upstream == "" {
		upstream = fmt.Sprintf("http://127.0.0.1:%d", cfg.DefaultPort)
	}

	fmt.Printf("3. Backend service (%s):\n", cyan(upstream))
	var testCmd *exec.Cmd
	if strings.HasPrefix(upstream, "unix:") {
		// Probe unix sockets through curl's --unix-socket support.
		socketPath := strings.TrimPrefix(upstream, "unix:")
		testCmd = exec.Command("curl", "-s", "-I", "--connect-timeout", "3", "--unix-socket", socketPath, "http://localhost/")
	} else {
		testCmd = exec.Command("curl", "-s", "-I", "--connect-timeout", "3", upstream)
	}
	testOut, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("   %s backend not reachable: %v (check that the upstream service is running)\n", red("✗"), err)
		if len(testOut) > 0 {
			fmt.Printf("   %s\n", strings.TrimSpace(string(testOut)))
		}
	} else {
		firstLine := ""
		for _, line := range strings.Split(string(testOut), "\n") {
			if strings.TrimSpace(line) != "" {
				firstLine = strings.TrimSpace(line)
				break
			}
		}
		if firstLine != "" {
			fmt.Printf("   %s backend is reachable: %s\n", green("✓"), firstLine)
		} else {
			fmt.Printf("   %s backend is reachable (connection established)\n", green("✓"))
		}
	}
}

// ==========================================
// Command: cert check
// ==========================================

func handleCheck(cfg *Config) {
	fmt.Println(yellow(bold("Environment & dependency check:")))
	fmt.Println()

	allPassed := true

	// 1. nginx
	fmt.Print("  [1] Nginx: ")
	if nginxPath, err := findNginx(); err != nil {
		fmt.Printf("%s (not installed or not in PATH)\n", red("✗"))
		allPassed = false
	} else {
		out, _ := exec.Command(nginxPath, "-v").CombinedOutput()
		ver := strings.TrimSpace(string(out))
		fmt.Printf("%s (%s: %s)\n", green("✓"), nginxPath, ver)
	}

	// 2. ACME account
	fmt.Print("  [2] ACME account: ")
	keyPath := filepath.Join(defaultAccountDir, accountKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		fmt.Printf("%s (%s)\n", green("✓"), keyPath)
	} else {
		fmt.Printf("%s (will be created on first certificate request)\n", yellow("!"))
	}

	// 3. OpenSSL
	fmt.Print("  [3] OpenSSL: ")
	if opensslPath, err := exec.LookPath("openssl"); err != nil {
		fmt.Printf("%s (not installed)\n", red("✗"))
		allPassed = false
	} else {
		out, _ := exec.Command(opensslPath, "version").CombinedOutput()
		fmt.Printf("%s (%s)\n", green("✓"), strings.TrimSpace(string(out)))
	}

	// 4. curl
	fmt.Print("  [4] Curl: ")
	if _, err := exec.LookPath("curl"); err != nil {
		fmt.Printf("%s (not installed)\n", red("✗"))
		allPassed = false
	} else {
		fmt.Printf("%s ready\n", green("✓"))
	}

	// 5. Privileges
	fmt.Print("  [5] Privileges: ")
	if isRoot() {
		fmt.Printf("%s (running as root)\n", green("✓"))
	} else {
		cmd := exec.Command("sudo", "-n", "whoami")
		if err := cmd.Run(); err != nil {
			fmt.Printf("%s (running as a regular user, commands will prompt for sudo)\n", yellow("!"))
		} else {
			fmt.Printf("%s (passwordless sudo works)\n", green("✓"))
		}
	}

	// 6. Renewal cron
	fmt.Print("  [6] Renewal cron: ")
	if _, err := os.Stat(renewalCronPath); err == nil {
		fmt.Printf("%s (%s)\n", green("✓"), renewalCronPath)
	} else {
		fmt.Printf("%s (will be installed on the next 'cert add' or 'cert init')\n", yellow("!"))
	}

	// 7. Working directories
	fmt.Println()
	fmt.Println("  Directory status:")
	dirs := []struct {
		desc string
		path string
	}{
		{"certificate dir", cfg.CertDir},
		{"nginx config dir", cfg.NginxConfDir},
		{"ACME webroot", cfg.WebrootDir},
		{"ACME account dir", defaultAccountDir},
	}

	for _, d := range dirs {
		fmt.Printf("    %-18s (%s): ", d.desc, d.path)
		if _, err := os.Stat(d.path); os.IsNotExist(err) {
			fmt.Printf("%s\n", yellow("missing (will be created automatically)"))
		} else {
			fmt.Printf("%s\n", green("ok"))
		}
	}

	fmt.Println()
	fmt.Println("  Current settings:")
	fmt.Printf("    base domain    : %s\n", cyan(cfg.BaseDomain))
	fmt.Printf("    default port   : %d\n", cfg.DefaultPort)
	fmt.Printf("    ACME e-mail    : %s\n", cyan(cfg.Email))

	fmt.Println()
	fmt.Println("  Supported DNS-01 providers: " + cyan(strings.Join(dnsProviderNames(), ", ")))

	fmt.Println()
	if allPassed {
		fmt.Println(green(bold("✅ All core dependencies are available!")))
	} else {
		fmt.Println(red(bold("❌ Some dependencies are missing. Run 'cert init' to install them automatically.")))
	}
}

// ==========================================
// Command: cert dns
// ==========================================

func handleDNS(args []string) {
	fmt.Println(yellow(bold("Supported DNS-01 providers:")))
	fmt.Println()
	names := dnsProviderNames()
	sort.Strings(names)
	for _, name := range names {
		info := dnsProviders[name]
		fmt.Printf("  %-14s %s\n", cyan(name), info.Description)
		fmt.Printf("  %-14s credentials: %s\n", "", info.Credentials)
	}
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  cert add '*.example.com' --dns cloudflare    # wildcard certificate via DNS-01")
	fmt.Println("  cert add api --dns manual                    # any provider, guided TXT record")
}

// ==========================================
// Command: cert install
// ==========================================

func handleInstall() {
	fmt.Println(yellow("Installing cert to the system PATH..."))

	execPath := certBinaryPath()
	targetPath := "/usr/local/bin/cert"
	if execPath == targetPath {
		fmt.Printf("%s already installed at %s\n", green("✓"), targetPath)
		return
	}

	fmt.Print("Copying to " + targetPath + "... ")
	if err := runPrivileged("install", "-m", "755", execPath, targetPath); err != nil {
		fmt.Printf("%s %v\n", red("failed:"), err)
		os.Exit(1)
	}
	fmt.Println(green("done"))

	fmt.Println()
	fmt.Printf("%s cert has been installed to %s\n", green(bold("✅")), targetPath)
	fmt.Println("You can now use it from anywhere: cert help")
}

// ==========================================
// Command: cert init
// ==========================================

func handleInit(args []string, cfg *Config) {
	fmt.Println(yellow(bold("==================================================")))
	fmt.Println(yellow(bold("Initializing server environment...")))
	fmt.Println(yellow(bold("==================================================")))
	fmt.Println()

	// Parse optional arguments (both "-d example.com" and "-d=example.com").
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-d" || arg == "--domain":
			if i+1 < len(args) {
				cfg.BaseDomain = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-d=") || strings.HasPrefix(arg, "--domain="):
			cfg.BaseDomain = arg[strings.Index(arg, "=")+1:]
		case arg == "-e" || arg == "--email":
			if i+1 < len(args) {
				cfg.Email = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-e=") || strings.HasPrefix(arg, "--email="):
			cfg.Email = arg[strings.Index(arg, "=")+1:]
		case arg == "-p" || arg == "--port":
			if i+1 < len(args) {
				p, _ := strconv.Atoi(args[i+1])
				if p > 0 && p <= 65535 {
					cfg.DefaultPort = p
				}
				i++
			}
		case strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--port="):
			p, _ := strconv.Atoi(arg[strings.Index(arg, "=")+1:])
			if p > 0 && p <= 65535 {
				cfg.DefaultPort = p
			}
		case arg == "--webroot":
			if i+1 < len(args) {
				cfg.WebrootDir = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--webroot="):
			cfg.WebrootDir = arg[strings.Index(arg, "=")+1:]
		}
	}

	// Validate user-provided settings before they reach config files or shells.
	cfg.BaseDomain = strings.ToLower(strings.TrimSpace(cfg.BaseDomain))
	if cfg.BaseDomain != "" && !validDomainLike(cfg.BaseDomain) {
		fmt.Printf("%s invalid base domain %q\n", red("Error:"), cfg.BaseDomain)
		os.Exit(1)
	}
	if cfg.Email == "" && cfg.BaseDomain != "" {
		cfg.Email = "admin@" + cfg.BaseDomain
	}
	if cfg.Email != "" && !validEmail(cfg.Email) {
		fmt.Printf("%s invalid ACME e-mail address: %q\n", red("Error:"), cfg.Email)
		os.Exit(1)
	}
	if cfg.Email == "" {
		fmt.Printf("%s no ACME e-mail configured — expiry reminders are disabled (optional: cert init -e you@example.com)\n", yellow("Note:"))
	}

	// 1. Install system packages.
	fmt.Println("1. Checking and installing OS dependencies (nginx, openssl, curl, cron)...")
	pm := detectPackageManager()
	if pm != nil {
		fmt.Printf("   detected package manager: %s\n", cyan(pm.Name))
		if len(pm.UpdateCmd) > 0 {
			fmt.Print("   refreshing package index... ")
			_ = runPrivileged(pm.UpdateCmd[0], pm.UpdateCmd[1:]...)
			fmt.Println(green("done"))
		}

		installArgs := append(pm.InstallCmd[1:], pm.Packages...)
		fmt.Printf("   installing required packages (%s)... ", strings.Join(pm.Packages, ", "))
		// Snapshot which packages are absent NOW, before installing them:
		// 'cert uninstall' will only ever remove these.
		missing := []string{}
		for _, pkg := range pm.Packages {
			if !isPackageInstalled(pm, pkg) {
				missing = append(missing, pkg)
			}
		}
		if err := runPrivileged(pm.InstallCmd[0], installArgs...); err != nil {
			fmt.Printf("%s (ignored, some packages may already be installed: %v)\n", yellow("warning"), err)
		} else {
			if len(missing) > 0 {
				state := loadState()
				state.PackagesInstalled = missing
				_ = state.save()
			}
			fmt.Println(green("done"))
		}
	} else {
		fmt.Printf("   %s unknown package manager, skipping automatic package installation. Make sure nginx/curl/openssl are available\n", yellow("Note:"))
	}

	// 2. Enable and start nginx (snapshot pre-existing service state first, so
	// 'cert uninstall' can restore it).
	fmt.Print("2. Enabling and starting nginx... ")
	wasEnabled := serviceIsEnabled("nginx")
	wasActive := serviceIsActive("nginx")
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runPrivileged("systemctl", "enable", "nginx")
		_ = runPrivileged("systemctl", "start", "nginx")
	} else if _, err := exec.LookPath("rc-service"); err == nil {
		_ = runPrivileged("rc-update", "add", "nginx")
		_ = runPrivileged("rc-service", "nginx", "start")
	}
	if !wasEnabled || !wasActive {
		state := loadState()
		if !wasEnabled {
			state.NginxEnabledByUs = true
		}
		if !wasActive {
			state.NginxStartedByUs = true
		}
		_ = state.save()
	}
	fmt.Println(green("done"))

	// 3. Create working directories and the ACME account key.
	fmt.Println("3. Creating working directories and the ACME account key...")
	for _, dir := range []string{cfg.CertDir, cfg.NginxConfDir, cfg.WebrootDir, defaultAccountDir} {
		fmt.Printf("   creating %s... ", dir)
		if err := ensureTrackedDir(dir, 0755); err != nil {
			fmt.Printf("%s %v\n", red("failed:"), err)
			os.Exit(1)
		}
		fmt.Println(green("done"))
	}
	if err := ensureDir(defaultAccountDir, 0700); err != nil {
		fmt.Printf("%s cannot create account directory: %v\n", red("failed:"), err)
		os.Exit(1)
	}
	if _, err := loadOrCreateAccountKey(defaultAccountDir); err != nil {
		fmt.Printf("%s cannot create ACME account key: %v\n", red("failed:"), err)
		os.Exit(1)
	}

	// 4. Deploy the shared ACME challenge endpoint.
	fmt.Println("4. Configuring the shared nginx ACME challenge endpoint...")
	if err := ensureSharedConfigs(cfg); err != nil {
		fmt.Printf("   %s %v\n", yellow("warning:"), err)
	} else {
		fmt.Println("   " + green("done"))
	}

	// 5. Install the daily renewal cron job.
	fmt.Print("5. Installing the daily certificate renewal cron job... ")
	if err := installRenewalCron(certBinaryPath()); err != nil {
		fmt.Printf("%s %v\n", yellow("warning:"), err)
	} else {
		fmt.Println(green("done (" + renewalCronPath + ")"))
	}

	// 6. Save the global configuration.
	fmt.Print("6. Saving configuration... ")
	_ = saveConfig(cfg, globalConfigPath)
	fmt.Println(green("done (file: " + globalConfigPath + ")"))

	// 7. Install cert to the system PATH.
	fmt.Println("7. Installing cert to the system PATH...")
	handleInstall()

	fmt.Println()
	fmt.Println(green(bold("🎉 Server environment initialized!")))
	fmt.Println()
	fmt.Println("Active settings:")
	fmt.Printf("  base domain    : %s\n", cyan(cfg.BaseDomain))
	fmt.Printf("  default port   : %d\n", cfg.DefaultPort)
	fmt.Printf("  ACME webroot   : %s\n", cyan(cfg.WebrootDir))
	fmt.Println()
	fmt.Println("Next steps, for example:")
	fmt.Printf("  cert add test           # HTTPS for test.%s, proxying to port %d\n", cfg.BaseDomain, cfg.DefaultPort)
	fmt.Printf("  cert apply -f cert-tool.yml   # or manage all sites declaratively\n")
}
