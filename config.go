package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ==========================================
// Global configuration constants & model
// ==========================================

const (
	defaultCertDir      = "/etc/nginx/ssl"
	defaultNginxConfDir = "/etc/nginx/conf.d"
	defaultWebrootDir   = "/var/www/cert-challenge"
	defaultPort         = 8080

	globalConfigPath  = "/etc/cert-tool/config.json"
	challengeConfName = "00-acme-challenge.conf"
	mapConfName       = "00-cert-tool-map.conf"
)

// Config holds the tool-wide settings shared by all commands.
type Config struct {
	BaseDomain   string `json:"base_domain"`    // default base domain, e.g. example.com
	CertDir      string `json:"cert_dir"`       // where issued certificates are installed
	NginxConfDir string `json:"nginx_conf_dir"` // nginx conf.d directory
	WebrootDir   string `json:"webroot_dir"`    // ACME HTTP-01 challenge webroot
	Email        string `json:"email"`          // ACME account e-mail
	DefaultPort  int    `json:"default_port"`   // default local upstream port
}

// getConfigFile returns the config file that is (or will be) in use.
func getConfigFile() string {
	if env := os.Getenv("CERT_CONFIG"); env != "" {
		return env
	}
	if _, err := os.Stat(globalConfigPath); err == nil {
		return globalConfigPath
	}
	home, _ := os.UserHomeDir()
	userConfig := filepath.Join(home, ".config/cert-tool/config.json")
	if _, err := os.Stat(userConfig); err == nil {
		return userConfig
	}
	if isRoot() {
		return globalConfigPath
	}
	return userConfig
}

// loadConfig reads the configuration, falling back to sensible defaults.
func loadConfig() (*Config, string) {
	cfg := &Config{
		CertDir:      defaultCertDir,
		NginxConfDir: defaultNginxConfDir,
		WebrootDir:   defaultWebrootDir,
		DefaultPort:  defaultPort,
	}

	confPath := getConfigFile()
	data, err := os.ReadFile(confPath)
	if err == nil {
		if jerr := json.Unmarshal(data, cfg); jerr != nil {
			fmt.Fprintf(os.Stderr, "%s ignoring invalid config file %s: %v\n", yellow("Warning:"), confPath, jerr)
		}
	}

	// Fill in defaults for empty fields.
	if cfg.CertDir == "" {
		cfg.CertDir = defaultCertDir
	}
	if cfg.NginxConfDir == "" {
		cfg.NginxConfDir = defaultNginxConfDir
	}
	if cfg.WebrootDir == "" {
		cfg.WebrootDir = defaultWebrootDir
	}
	if cfg.DefaultPort == 0 {
		cfg.DefaultPort = defaultPort
	}
	cfg.BaseDomain = strings.ToLower(strings.TrimSpace(cfg.BaseDomain))
	if cfg.Email == "" && cfg.BaseDomain != "" {
		cfg.Email = "admin@" + cfg.BaseDomain
	}

	return cfg, confPath
}

// saveConfig persists the configuration with safe permissions.
func saveConfig(cfg *Config, targetPath string) error {
	dir := filepath.Dir(targetPath)
	if err := runPrivileged("mkdir", "-p", dir); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp("", "cert-config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	if err := runPrivileged("cp", tmpFile.Name(), targetPath); err != nil {
		return err
	}
	_ = runPrivileged("chmod", "644", targetPath)
	return nil
}

// ==========================================
// Command: cert config
// ==========================================

func handleConfig(args []string, cfg *Config) {
	_, confPath := loadConfig()

	if len(args) == 0 || args[0] == "show" {
		fmt.Println(yellow("Current configuration (file: " + confPath + ")"))
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(data))
		return
	}

	if args[0] == "path" {
		fmt.Println(confPath)
		return
	}

	if args[0] == "set" {
		if len(args) < 3 {
			fmt.Println(red("Error: usage: cert config set <key> <value>"))
			fmt.Println("Supported keys: domain, port, email, webroot, cert_dir, nginx_conf_dir")
			return
		}
		key := strings.ToLower(args[1])
		val := args[2]

		switch key {
		case "domain", "base_domain":
			val = strings.ToLower(val)
			if !validDomainLike(val) {
				fmt.Println(red("Error: invalid domain: " + val))
				return
			}
			cfg.BaseDomain = val
		case "port", "default_port":
			p, err := strconv.Atoi(val)
			if err != nil || p <= 0 || p > 65535 {
				fmt.Println(red("Error: port must be between 1 and 65535"))
				return
			}
			cfg.DefaultPort = p
		case "email":
			if !validEmail(val) {
				fmt.Println(red("Error: invalid e-mail address: " + val))
				return
			}
			cfg.Email = val
		case "webroot", "webroot_dir":
			cfg.WebrootDir = val
		case "cert_dir":
			cfg.CertDir = val
		case "nginx_conf_dir":
			cfg.NginxConfDir = val
		default:
			fmt.Println(red("Error: unknown config key: " + key))
			return
		}

		if err := saveConfig(cfg, confPath); err != nil {
			fmt.Printf("%s failed to save configuration: %v\n", red("Error:"), err)
			return
		}
		fmt.Println(green("✅ Configuration saved to " + confPath))
		return
	}

	fmt.Println(red("Unknown subcommand. Usage: cert config [show|path|set <key> <value>]"))
}
