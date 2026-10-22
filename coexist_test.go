package main

import (
	"strings"
	"testing"
	"text/template"
)

const nginxDumpSample = `# configuration file /etc/nginx/nginx.conf:
http {
    include /etc/nginx/conf.d/*.conf;
}
# configuration file /etc/nginx/conf.d/user-default.conf:
server {
    listen 80 default_server;
    server_name _;
}
# configuration file /etc/nginx/conf.d/00-acme-challenge.conf:
server {
    listen 80 default_server;
    server_name _;
}
# configuration file /etc/nginx/conf.d/00-cert-tool-map.conf:
map $http_upgrade $cert_tool_connection_upgrade {
    default upgrade;
    ''      close;
}
# configuration file /etc/nginx/conf.d/api.example.com.conf:
# Managed by cert-tool.
server {
    listen 443 ssl;
    server_name api.example.com;
    location / { proxy_pass http://127.0.0.1:8080; }
}
# configuration file /etc/nginx/conf.d/user-blog.conf:
server {
    listen 80;
    server_name blog.example.com *.blog.example.com;
    location / { proxy_pass http://127.0.0.1:9000; }
}
# configuration file /etc/nginx/conf.d/user-ssl.conf:
server {
    listen [::]:443 ssl;
    server_name secure.example.com;
}
`

func TestListenIsPort80(t *testing.T) {
	cases := map[string]bool{
		"80":                   true,
		"0.0.0.0:80":           true,
		"[::]:80":              true,
		"[::]":                 true,
		"0.0.0.0":              true,
		"443":                  false,
		"0.0.0.0:443":          false,
		"[::]:443":             false,
		"unix:/run/nginx.sock": false,
	}
	for arg, want := range cases {
		if got := listenIsPort80(arg); got != want {
			t.Errorf("listenIsPort80(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestHasDefaultServerOnPort80(t *testing.T) {
	// The tool's own challenge conf must be ignored (it is regenerated), so a
	// dump that only contains our own default_server is not a conflict.
	ownOnly := `# configuration file /etc/nginx/conf.d/00-acme-challenge.conf:
server {
    listen 80 default_server;
    server_name _;
}
`
	if hasDefaultServerOnPort80(ownOnly, "/etc/nginx/conf.d/00-acme-challenge.conf") {
		t.Error("our own challenge conf must not count as a conflict")
	}
	// The user's custom default_server holds default_server → conflict.
	if !hasDefaultServerOnPort80(nginxDumpSample, "/etc/nginx/other.conf") {
		t.Error("an existing default_server on port 80 must be detected")
	}

	// No default_server anywhere: safe to claim it.
	dump := `# configuration file /etc/nginx/conf.d/user-blog.conf:
server {
    listen 80;
    server_name blog.example.com;
}
`
	if hasDefaultServerOnPort80(dump, "/etc/nginx/conf.d/00-acme-challenge.conf") {
		t.Error("plain listen 80 without default_server is not a conflict")
	}
	// default_server on a different port is fine.
	dump2 := `# configuration file /etc/nginx/conf.d/user-ssl.conf:
server {
    listen 8443 default_server;
    server_name x.example.com;
}
`
	if hasDefaultServerOnPort80(dump2, "") {
		t.Error("default_server on another port must not conflict")
	}
}

func TestFindServerNameConflict(t *testing.T) {
	// Foreign block serves blog.example.com → conflict.
	if got := findServerNameConflict(nginxDumpSample, "blog.example.com", ""); got != "/etc/nginx/conf.d/user-blog.conf" {
		t.Errorf("conflict for blog.example.com = %q", got)
	}
	// Wildcard match in a foreign block.
	if got := findServerNameConflict(nginxDumpSample, "x.blog.example.com", ""); got != "/etc/nginx/conf.d/user-blog.conf" {
		t.Errorf("wildcard conflict = %q", got)
	}
	// Our own managed block is never a conflict.
	if got := findServerNameConflict(nginxDumpSample, "api.example.com", "/etc/nginx/conf.d/other.conf"); got != "" {
		t.Errorf("managed conf must be ignored, got %q", got)
	}
	// Unknown domain → no conflict.
	if got := findServerNameConflict(nginxDumpSample, "fresh.example.com", ""); got != "" {
		t.Errorf("unexpected conflict %q", got)
	}
	// server_name _ is a placeholder, never a conflict.
	if got := findServerNameConflict(nginxDumpSample, "_.example.com", ""); got == "/etc/nginx/conf.d/user-default.conf" {
		t.Errorf("'_' must never match, got %q", got)
	}
}

func TestConfDirIsIncluded(t *testing.T) {
	if !confDirIsIncluded(nginxDumpSample, "/etc/nginx/conf.d/00-acme-challenge.conf") {
		t.Error("loaded config must be detected")
	}
	if confDirIsIncluded(nginxDumpSample, "/etc/nginx/other/00-acme-challenge.conf") {
		t.Error("non-loaded path must not match")
	}
}

func TestChallengeTemplateRendering(t *testing.T) {
	render := func(data ChallengeConfig) string {
		tmpl, err := template.New("challenge").Parse(challengeTemplate)
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// With the default_server claim.
	out := render(ChallengeConfig{WebrootDir: "/var/www/cert-challenge", DefaultServer: " default_server"})
	if !strings.Contains(out, "listen 80 default_server;") || !strings.Contains(out, "root /var/www/cert-challenge;") {
		t.Errorf("default_server variant rendered wrong:\n%s", out)
	}

	// Backed off (conflict): no default_server.
	out = render(ChallengeConfig{WebrootDir: "/var/www/cert-challenge", DefaultServer: ""})
	if strings.Contains(out, "default_server") || !strings.Contains(out, "listen 80;") {
		t.Errorf("backed-off variant rendered wrong:\n%s", out)
	}
}
