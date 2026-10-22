package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/fatih/color"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

var (
	green  = color.New(color.FgGreen).SprintFunc()
	yellow = color.New(color.FgYellow).SprintFunc()
	red    = color.New(color.FgRed).SprintFunc()
	cyan   = color.New(color.FgCyan).SprintFunc()
	bold   = color.New(color.Bold).SprintFunc()
)

// privilegedCommands manage system state and re-execute themselves through sudo
// so the whole pipeline runs with a single privilege prompt.
var privilegedCommands = map[string]bool{
	"init":      true,
	"add":       true,
	"remove":    true,
	"apply":     true,
	"renew":     true,
	"install":   true,
	"uninstall": true,
	"update":    true,
}

func main() {
	if len(os.Args) < 2 {
		showHelp()
		os.Exit(1)
	}

	command := os.Args[1]
	if privilegedCommands[command] && !isReadOnlyInvocation(command) && needsElevation(command) {
		elevateIfNeeded()
	}

	cfg, _ := loadConfig()

	switch command {
	case "add":
		handleAdd(os.Args[2:], cfg)
	case "remove":
		handleRemove(os.Args[2:], cfg)
	case "apply":
		handleApply(os.Args[2:], cfg)
	case "renew":
		handleRenew(os.Args[2:], cfg)
	case "list":
		handleList(cfg)
	case "test":
		handleTest(os.Args[2:], cfg)
	case "check":
		handleCheck(cfg)
	case "dns":
		handleDNS(os.Args[2:])
	case "init":
		handleInit(os.Args[2:], cfg)
	case "config":
		handleConfig(os.Args[2:], cfg)
	case "install":
		handleInstall()
	case "uninstall":
		handleUninstall(os.Args[2:], cfg)
	case "update":
		handleUpdate(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("cert %s\n", version)
	case "help", "--help", "-h":
		showHelp()
	default:
		fmt.Printf("%s unknown command: %s\n\n", red("Error:"), command)
		showHelp()
		os.Exit(1)
	}
}

// isReadOnlyInvocation reports whether a privileged command was invoked in a
// mode that only reads (e.g. "cert apply --dry-run") and therefore does not
// need root privileges.
func isReadOnlyInvocation(command string) bool {
	if command != "apply" && command != "uninstall" && command != "update" {
		return false
	}
	for _, arg := range os.Args[2:] {
		if arg == "--dry-run" || arg == "-n" || arg == "--check" {
			return true
		}
	}
	return false
}

// needsElevation reports whether the command has to run as root. Binary
// management commands only need privileges when the install location is not
// writable by the current user.
func needsElevation(command string) bool {
	switch command {
	case "update":
		return !dirWritable(filepath.Dir(certBinaryPath()))
	case "install":
		return !dirWritable("/usr/local/bin")
	default:
		return true
	}
}

// elevateIfNeeded re-executes the current command with root privileges via
// sudo (one prompt for the whole run instead of many).
func elevateIfNeeded() {
	if isRoot() {
		return
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		fmt.Printf("%s this command needs root privileges (install sudo or re-run as root)\n", red("Error:"))
		os.Exit(1)
	}

	self := certBinaryPath()

	args := []string{"env"}
	if cfgPath := os.Getenv("CERT_CONFIG"); cfgPath != "" {
		args = append(args, "CERT_CONFIG="+cfgPath)
	}
	args = append(args, self)
	args = append(args, os.Args[1:]...)

	cmd := exec.Command(sudoPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err != nil {
		fmt.Printf("%s failed to gain root privileges: %v\n", red("Error:"), err)
		os.Exit(1)
	}
	os.Exit(0)
}

// certBinaryPath resolves the running binary (used for self re-exec and cron).
func certBinaryPath() string {
	if execPath, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
			return resolved
		}
		return execPath
	}
	return "/usr/local/bin/cert"
}

func showHelp() {
	fmt.Println(yellow(bold("cert - HTTPS certificate & Nginx reverse proxy manager")))
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  cert init [flags]                 Initialize this server (nginx, shared ACME endpoint, renewal cron)")
	fmt.Println("  cert add <domain|subdomain>       Issue a certificate and generate the nginx reverse proxy")
	fmt.Println("  cert apply [flags]                Apply a declarative manifest (cert-tool.yml) with concurrent provisioning")
	fmt.Println("  cert renew [flags]                Renew certificates that are close to expiry (runs daily via cron)")
	fmt.Println("  cert remove <domain|subdomain>    Remove the config, certificate and metadata")
	fmt.Println("  cert list                         List configured sites with certificate expiry")
	fmt.Println("  cert test <domain|subdomain>      Diagnose nginx, HTTPS and the backend service")
	fmt.Println("  cert check                        Check dependencies and current configuration")
	fmt.Println("  cert dns                          List supported DNS-01 providers (for wildcard certificates)")
	fmt.Println("  cert config [show|path|set]       Show or edit the global configuration")
	fmt.Println("  cert install                      Install the cert binary to /usr/local/bin/cert")
	fmt.Println("  cert update [--check]             Update cert to the latest release")
	fmt.Println("  cert uninstall [flags]            Remove everything and restore the pre-installation state")
	fmt.Println("  cert version                      Print the version")
	fmt.Println("  cert help                         Show this help")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  cert init                          # initialize this server (all flags optional)")
	fmt.Println("  cert add api -p 8080               # api.<base domain> -> http://127.0.0.1:8080")
	fmt.Println("  cert add foo.bar.com -p 5000       # any full domain   -> http://127.0.0.1:5000")
	fmt.Println("  cert add '*.example.com' --dns cloudflare   # wildcard certificate (DNS-01)")
	fmt.Println("  cert remove api                    # remove a site")
	fmt.Println("  cert uninstall --dry-run           # see what uninstall would do")
	fmt.Println()
	fmt.Println("See the README for wildcard/DNS-01 details and the declarative manifest (cert-tool.yml).")
}
