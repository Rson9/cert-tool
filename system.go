package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ==========================================
// Privilege & process helpers
// ==========================================

func isRoot() bool {
	return os.Geteuid() == 0
}

func runPrivileged(name string, args ...string) error {
	var cmd *exec.Cmd
	if isRoot() {
		cmd = exec.Command(name, args...)
	} else {
		cmd = exec.Command("sudo", append([]string{name}, args...)...)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runPrivilegedOutput(name string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if isRoot() {
		cmd = exec.Command(name, args...)
	} else {
		cmd = exec.Command("sudo", append([]string{name}, args...)...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// reloadNginx reloads nginx using whatever service manager is available.
func reloadNginx() error {
	if _, err := exec.LookPath("systemctl"); err == nil {
		if err := runPrivileged("systemctl", "reload", "nginx"); err == nil {
			return nil
		}
		if err := runPrivileged("systemctl", "restart", "nginx"); err == nil {
			return nil
		}
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		if err := runPrivileged("rc-service", "nginx", "reload"); err == nil {
			return nil
		}
	}
	nginxBin, err := findNginx()
	if err == nil {
		return runPrivileged(nginxBin, "-s", "reload")
	}
	return fmt.Errorf("no available method to reload nginx")
}

// findNginx locates the nginx binary.
func findNginx() (string, error) {
	if path, err := exec.LookPath("nginx"); err == nil {
		return path, nil
	}
	candidates := []string{
		"/usr/sbin/nginx",
		"/usr/bin/nginx",
		"/usr/local/nginx/sbin/nginx",
		"/usr/local/bin/nginx",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("nginx binary not found in PATH or common locations")
}

// ==========================================
// Privileged file helpers
// ==========================================
//
// The tool manages root-owned files (nginx config, private keys). These
// helpers work both as root and through sudo, so every code path is safe in
// either mode.

func ensureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err == nil {
		_ = os.Chmod(dir, mode)
		return nil
	}
	return runPrivileged("mkdir", "-p", dir)
}

func writePrivilegedFile(path string, data []byte, mode os.FileMode) error {
	// Try a direct write first, fall back to sudo for root-owned locations.
	if err := os.WriteFile(path, data, mode); err == nil {
		_ = os.Chmod(path, mode)
		return nil
	}

	tmp, err := os.CreateTemp("", "cert-tool-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if err := runPrivileged("cp", tmp.Name(), path); err != nil {
		return err
	}
	return runPrivileged("chmod", fmt.Sprintf("%o", mode), path)
}

func readFilePrivileged(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	}
	out, err := runPrivilegedOutput("cat", path)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

func removePrivilegedFile(path string) error {
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return nil
	}
	return runPrivileged("rm", "-f", path)
}

// dirWritable reports whether the current user can create files in dir.
func dirWritable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".cert-writability-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return true
}

// ==========================================
// Service state (used by the install snapshot & uninstall)
// ==========================================

// serviceIsEnabled reports whether the service starts at boot.
func serviceIsEnabled(name string) bool {
	if _, err := exec.LookPath("systemctl"); err == nil {
		out, err := exec.Command("systemctl", "is-enabled", name).CombinedOutput()
		if err != nil {
			return false
		}
		s := strings.TrimSpace(string(out))
		return s == "enabled" || s == "static" || s == "indirect"
	}
	if _, err := exec.LookPath("rc-update"); err == nil {
		out, _ := exec.Command("rc-update", "show").CombinedOutput()
		return strings.Contains(string(out), name)
	}
	return true // unknown service manager — assume pre-existing, never "restore"
}

// serviceIsActive reports whether the service is currently running.
func serviceIsActive(name string) bool {
	if _, err := exec.LookPath("systemctl"); err == nil {
		return exec.Command("systemctl", "is-active", name).Run() == nil
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		return exec.Command("rc-service", name, "status").Run() == nil
	}
	return true
}

func stopService(name string) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runPrivileged("systemctl", "stop", name)
		return
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		_ = runPrivileged("rc-service", name, "stop")
	}
}

func disableService(name string) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runPrivileged("systemctl", "disable", name)
		return
	}
	if _, err := exec.LookPath("rc-update"); err == nil {
		_ = runPrivileged("rc-update", "del", name)
	}
}

// ==========================================
// Distro detection (package managers)
// ==========================================

type PkgManager struct {
	Name       string
	UpdateCmd  []string
	InstallCmd []string
	RemoveCmd  []string
	QueryCmd   []string // succeeds if the package is already installed
	Packages   []string
	ServiceCmd string
}

// isPackageInstalled reports whether the package is already present, so that
// the install snapshot can tell "ours" from "pre-existing".
func isPackageInstalled(pm *PkgManager, pkg string) bool {
	if pm == nil || len(pm.QueryCmd) == 0 {
		return true // unknown — treat as pre-existing and never remove it
	}
	cmd := exec.Command(pm.QueryCmd[0], append(append([]string{}, pm.QueryCmd[1:]...), pkg)...)
	return cmd.Run() == nil
}

func detectPackageManager() *PkgManager {
	if _, err := exec.LookPath("apt-get"); err == nil {
		return &PkgManager{
			Name:       "apt",
			UpdateCmd:  []string{"apt-get", "update"},
			InstallCmd: []string{"apt-get", "install", "-y"},
			RemoveCmd:  []string{"apt-get", "purge", "-y"},
			QueryCmd:   []string{"dpkg", "-s"},
			Packages:   []string{"nginx", "openssl", "curl", "cron"},
			ServiceCmd: "systemctl",
		}
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return &PkgManager{
			Name:       "dnf",
			UpdateCmd:  nil,
			InstallCmd: []string{"dnf", "install", "-y"},
			RemoveCmd:  []string{"dnf", "remove", "-y"},
			QueryCmd:   []string{"rpm", "-q"},
			Packages:   []string{"nginx", "openssl", "curl", "cronie"},
			ServiceCmd: "systemctl",
		}
	}
	if _, err := exec.LookPath("yum"); err == nil {
		return &PkgManager{
			Name:       "yum",
			UpdateCmd:  nil,
			InstallCmd: []string{"yum", "install", "-y"},
			RemoveCmd:  []string{"yum", "remove", "-y"},
			QueryCmd:   []string{"rpm", "-q"},
			Packages:   []string{"nginx", "openssl", "curl", "cronie"},
			ServiceCmd: "systemctl",
		}
	}
	if _, err := exec.LookPath("apk"); err == nil {
		return &PkgManager{
			Name:       "apk",
			UpdateCmd:  []string{"apk", "update"},
			InstallCmd: []string{"apk", "add"},
			RemoveCmd:  []string{"apk", "del"},
			QueryCmd:   []string{"apk", "info", "-e"},
			Packages:   []string{"nginx", "openssl", "curl", "cronie"},
			ServiceCmd: "rc-service",
		}
	}
	if _, err := exec.LookPath("pacman"); err == nil {
		return &PkgManager{
			Name:       "pacman",
			UpdateCmd:  []string{"pacman", "-Sy"},
			InstallCmd: []string{"pacman", "-S", "--noconfirm"},
			RemoveCmd:  []string{"pacman", "-R", "--noconfirm"},
			QueryCmd:   []string{"pacman", "-Q"},
			Packages:   []string{"nginx", "openssl", "curl", "cronie"},
			ServiceCmd: "systemctl",
		}
	}
	return nil
}
