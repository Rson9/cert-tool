package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ==========================================
// Nginx templates & data structures
// ==========================================

// NginxConfig holds the data used to render the per-site Nginx config.
type NginxConfig struct {
	FullDomain  string
	CertPath    string
	KeyPath     string
	UpstreamURL string
	WebrootDir  string
	ListenSSL   string // TLS listen directives (adapted to the installed nginx version)
}

const nginxTemplate = `# Managed by cert-tool. Manual changes may be overwritten by "cert add".
# Site: {{.FullDomain}}

server {
    listen 80;
    server_name {{.FullDomain}};

    # ACME HTTP-01 challenge (needed so certificate renewal keeps working)
    location /.well-known/acme-challenge/ {
        root {{.WebrootDir}};
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    {{.ListenSSL}}
    server_name {{.FullDomain}};

    ssl_certificate     {{.CertPath}};
    ssl_certificate_key {{.KeyPath}};

    ssl_session_cache   shared:cert_tool_ssl:10m;
    ssl_session_timeout 1d;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305;
    ssl_prefer_server_ciphers off;

    client_max_body_size 100m;

    location / {
        proxy_pass {{.UpstreamURL}};
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # WebSocket / upgrade support (safe for plain HTTP traffic too)
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $cert_tool_connection_upgrade;

        # Long timeouts keep idle WebSocket connections alive
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
`

// ChallengeConfig holds the data used to render the shared ACME challenge
// endpoint. DefaultServer is only claimed when port 80 has no other
// default_server, so an existing nginx setup is never hijacked.
type ChallengeConfig struct {
	WebrootDir    string
	DefaultServer string // " default_server" or ""
}

const challengeTemplate = `# Managed by cert-tool. Shared ACME HTTP-01 challenge endpoint.
server {
    listen 80{{.DefaultServer}};
    server_name _;

    location /.well-known/acme-challenge/ {
        root {{.WebrootDir}};
    }

    location / {
        return 404;
    }
}
`

// upgradeMapTemplate defines a shared map used by every site config so that
// WebSocket upgrades work while ordinary HTTP requests get "close".
const upgradeMapTemplate = `# Managed by cert-tool. Shared WebSocket "Connection" header map.
map $http_upgrade $cert_tool_connection_upgrade {
    default upgrade;
    ''      close;
}
`

// sslListenDirective returns the TLS listen block matching the installed nginx
// version: nginx >= 1.25.1 deprecates "listen ... http2" in favour of "http2 on;".
func sslListenDirective() string {
	if major, minor, patch, err := nginxVersion(); err == nil {
		if major > 1 || (major == 1 && (minor > 25 || (minor == 25 && patch >= 1))) {
			return "listen 443 ssl;\n    http2 on;"
		}
	}
	// Legacy syntax (nginx < 1.25.1), harmless if detection failed.
	return "listen 443 ssl http2;"
}

// nginxVersion parses the installed nginx version (nginx prints it to stderr).
func nginxVersion() (major, minor, patch int, err error) {
	nginxBin, ferr := findNginx()
	if ferr != nil {
		return 0, 0, 0, ferr
	}
	out, _ := exec.Command(nginxBin, "-v").CombinedOutput()
	re := regexp.MustCompile(`nginx/(\d+)\.(\d+)\.(\d+)`)
	m := re.FindStringSubmatch(string(out))
	if len(m) < 4 {
		return 0, 0, 0, fmt.Errorf("unable to parse nginx version from %q", strings.TrimSpace(string(out)))
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, nil
}

// ==========================================
// Domain / upstream parsing & validation
// ==========================================

var (
	// Labels of letters, digits and hyphens, separated by dots. A single label
	// ("api") is allowed as input and gets expanded against the base domain.
	domainPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	upstreamPattern = regexp.MustCompile(`^(https?://[a-z0-9.\-:\[\]_/]+|unix:/[a-zA-Z0-9._/\-]+)$`)
	emailPattern    = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
)

// validDomainLike reports whether s is a safe, syntactically valid (sub)domain.
func validDomainLike(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 253 {
		return false
	}
	return domainPattern.MatchString(s)
}

// validDomainSpec validates a site domain, allowing a leading "*." wildcard
// (which requires DNS-01 and is enforced elsewhere).
func validDomainSpec(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "*.") {
		s = strings.TrimPrefix(s, "*.")
	}
	return validDomainLike(s)
}

// validUpstream reports whether the upstream URL is safe to embed in the
// generated nginx config (no whitespace, quotes, semicolons or braces).
func validUpstream(s string) bool {
	return upstreamPattern.MatchString(s)
}

// validEmail reports whether s looks like a usable e-mail address.
func validEmail(s string) bool {
	return emailPattern.MatchString(s)
}

// parseDomain expands a short subdomain against the configured base domain and
// returns both the subdomain label and the fully qualified domain name.
func parseDomain(inputDomain, baseDomain string) (subdomain string, fullDomain string) {
	inputDomain = strings.ToLower(strings.TrimSpace(inputDomain))
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	if strings.Contains(inputDomain, ".") {
		fullDomain = inputDomain
		if baseDomain != "" && strings.HasSuffix(inputDomain, "."+baseDomain) {
			subdomain = strings.TrimSuffix(inputDomain, "."+baseDomain)
		} else {
			parts := strings.Split(inputDomain, ".")
			subdomain = parts[0]
		}
	} else {
		subdomain = inputDomain
		if baseDomain != "" {
			fullDomain = inputDomain + "." + baseDomain
		} else {
			fullDomain = inputDomain
		}
	}
	return subdomain, fullDomain
}

// resolveUpstream builds the upstream proxy target from the given options.
// Supported forms: "host[:port]", "http(s)://host[:port][/path]" and "unix:/path/to.sock".
func resolveUpstream(port int, customUpstream string, defaultPort int) string {
	if customUpstream != "" {
		if strings.HasPrefix(customUpstream, "unix:") || strings.HasPrefix(customUpstream, "http://") || strings.HasPrefix(customUpstream, "https://") {
			return customUpstream
		}
		return "http://" + customUpstream
	}
	if port > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", defaultPort)
}

// ==========================================
// Existing nginx setup inspection (coexistence)
// ==========================================
//
// The tool is designed to coexist with a pre-existing nginx installation:
// it never rewrites foreign server blocks. These helpers parse an
// `nginx -T` dump (the fully expanded configuration) to detect the three
// situations where naive automation would break an existing setup.

// nginxConfigSections splits an `nginx -T` dump into file path → content.
// nginx -T prints a "# configuration file <path>:" header before each file.
func nginxConfigSections(dump string) map[string]string {
	sections := map[string]string{}
	current := ""
	for _, line := range strings.Split(dump, "\n") {
		if strings.HasPrefix(line, "# configuration file ") {
			current = strings.TrimSpace(strings.TrimPrefix(line, "# configuration file "))
			current = strings.TrimSuffix(current, ":")
			continue
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}

// listenIsPort80 reports whether a listen directive binds port 80.
func listenIsPort80(arg string) bool {
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "unix:") {
		return false
	}
	if strings.HasPrefix(arg, "[::]") {
		rest := strings.TrimPrefix(arg, "[::]")
		if rest == "" {
			return true // listen [::];
		}
		return strings.HasPrefix(rest, ":80")
	}
	if idx := strings.LastIndex(arg, ":"); idx >= 0 {
		return arg[idx+1:] == "80"
	}
	if _, err := strconv.Atoi(arg); err == nil {
		return arg == "80" // bare port number: listen 80;
	}
	return true // bare address: listen 0.0.0.0;  → default port 80
}

// hasDefaultServerOnPort80 reports whether any config file other than
// ignoreFile already claims the port 80 default_server. Claiming it twice
// makes nginx refuse to start, so our challenge endpoint must back off.
func hasDefaultServerOnPort80(dump, ignoreFile string) bool {
	listenRe := regexp.MustCompile(`(?m)^\s*listen\s+([^;]+);`)
	for path, content := range nginxConfigSections(dump) {
		if path == ignoreFile {
			continue
		}
		for _, match := range listenRe.FindAllStringSubmatch(content, -1) {
			fields := strings.Fields(match[1])
			if len(fields) == 0 {
				continue
			}
			if !listenIsPort80(fields[0]) {
				continue
			}
			for _, opt := range fields[1:] {
				if opt == "default_server" {
					return true
				}
			}
		}
	}
	return false
}

// findServerNameConflict returns the config file that already serves the given
// domain (exact or wildcard match), ignoring tool-managed files. HTTP-01
// challenges for such domains would hit that server block and fail.
func findServerNameConflict(dump, domain, ignoreFile string) string {
	nameRe := regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	for path, content := range nginxConfigSections(dump) {
		if path == ignoreFile {
			continue
		}
		if strings.Contains(content, managedMarker) {
			continue // our own site configs
		}
		for _, match := range nameRe.FindAllStringSubmatch(content, -1) {
			for _, name := range strings.Fields(match[1]) {
				name = strings.ToLower(name)
				if name == "_" || name == "" {
					continue
				}
				if name == strings.ToLower(domain) {
					return path
				}
				if strings.HasPrefix(name, "*.") && strings.HasSuffix(strings.ToLower(domain), strings.TrimPrefix(name, "*")) {
					return path
				}
			}
		}
	}
	return ""
}

// confDirIsIncluded reports whether the tool's config directory is actually
// loaded by the running nginx configuration.
func confDirIsIncluded(dump, probePath string) bool {
	return strings.Contains(dump, probePath)
}
