package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ==========================================
// Uninstall: restore the server to its pre-installation state
// ==========================================
//
// The tool records an installation snapshot (see state.go) whenever it changes
// pre-existing state: packages it installed, services it started and default
// files it moved aside. Uninstall replays that snapshot in reverse, removes
// everything the tool owns and nothing else.

const renewalLogPath = "/var/log/cert-tool-renew.log"

// addIfExists appends path to list if it exists on disk.
func addIfExists(list *[]string, path string) {
	if _, err := os.Stat(path); err == nil {
		*list = append(*list, path)
	}
}

// uninstallPlan lists every action uninstall would take.
type uninstallPlan struct {
	restore        []movedPath // move back
	removeConfs    []string    // nginx configs owned by the tool
	removeDomains  []string    // certificates owned by the tool
	removePaths    []string    // files/directories owned by the tool
	stopNginx      bool
	disableNginx   bool
	removePackages []string
	removeBinaries []string
	warnings       []string
}

// buildUninstallPlan computes what uninstall would do — without touching anything.
func buildUninstallPlan(cfg *Config, state *installState, keepSites, keepPackages bool) *uninstallPlan {
	plan := &uninstallPlan{}

	// 1. Move pre-existing files back to their original locations.
	for _, moved := range state.MovedPaths {
		if _, err := os.Stat(moved.To); err == nil {
			if _, err := os.Stat(moved.From); os.IsNotExist(err) {
				plan.restore = append(plan.restore, moved)
			} else {
				plan.warnings = append(plan.warnings,
					fmt.Sprintf("cannot restore %s: %s exists again (left untouched)", moved.From, moved.From))
			}
		}
	}

	// 2. Nginx configs owned by the tool (marker or shared helper files).
	if entries, err := os.ReadDir(cfg.NginxConfDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
				continue
			}
			path := filepath.Join(cfg.NginxConfDir, entry.Name())
			isShared := entry.Name() == challengeConfName ||
				(entry.Name() == mapConfName && !keepSites) // sites kept = keep the map they depend on
			if entry.Name() == mapConfName && keepSites {
				continue
			}
			if isShared {
				plan.removeConfs = append(plan.removeConfs, path)
				continue
			}
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), managedMarker) {
				if keepSites {
					continue
				}
				plan.removeConfs = append(plan.removeConfs, path)
			}
		}
	}

	// 3. Certificates issued by the tool (identified by their metadata).
	if !keepSites {
		if entries, err := os.ReadDir(cfg.CertDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
					continue
				}
				domain := strings.TrimSuffix(entry.Name(), ".meta.json")
				if meta := loadCertMeta(cfg.CertDir, domain); meta != nil {
					plan.removeDomains = append(plan.removeDomains, meta.Domain)
				}
			}
		}
	}

	// 4. Tool-owned files and directories (only those that actually exist).
	for _, path := range []string{renewalCronPath, renewalLogPath, globalConfigPath} {
		addIfExists(&plan.removePaths, path)
	}
	for _, dir := range state.DirsCreated {
		addIfExists(&plan.removePaths, dir)
	}
	if keepSites {
		addIfExists(&plan.removePaths, defaultAccountDir)
	}

	// 5. Service state (only what installation changed).
	if state.NginxStartedByUs {
		plan.stopNginx = true
	}
	if state.NginxEnabledByUs {
		plan.disableNginx = true
	}

	// 6. Packages (only those that did NOT exist before installation).
	if !keepPackages && len(state.PackagesInstalled) > 0 {
		plan.removePackages = append(plan.removePackages, state.PackagesInstalled...)
	}

	// 7. Binaries.
	if _, err := os.Stat("/usr/local/bin/cert"); err == nil {
		plan.removeBinaries = append(plan.removeBinaries, "/usr/local/bin/cert")
	}
	if self := certBinaryPath(); self != "/usr/local/bin/cert" {
		plan.removeBinaries = append(plan.removeBinaries, self)
	}

	// 8. Warnings.
	if keepSites {
		plan.warnings = append(plan.warnings,
			"site configs and certificates are kept, but renewal is removed — certificates will expire; renew manually or keep the cron job")
	}
	if !keepSites && len(plan.removeDomains) > 0 {
		plan.warnings = append(plan.warnings,
			"removing certificates takes the corresponding sites offline until you configure new certificates")
	}
	if _, err := os.Stat(defaultAccountDir); err == nil {
		plan.warnings = append(plan.warnings,
			"the ACME account key in /etc/cert-tool/account will be deleted (back it up first to keep your account)")
	}

	return plan
}

func (p *uninstallPlan) empty() bool {
	return len(p.restore) == 0 && len(p.removeConfs) == 0 && len(p.removeDomains) == 0 &&
		len(p.removePaths) == 0 && len(p.removePackages) == 0 && len(p.removeBinaries) == 0 &&
		!p.stopNginx && !p.disableNginx
}

func (p *uninstallPlan) print() {
	for _, moved := range p.restore {
		fmt.Printf("  %s %-18s %s -> %s\n", cyan("restore"), "", moved.To, moved.From)
	}
	for _, conf := range p.removeConfs {
		fmt.Printf("  %s %s\n", red("remove"), conf)
	}
	for _, domain := range p.removeDomains {
		fmt.Printf("  %s certificate for %s\n", red("remove"), domain)
	}
	for _, path := range p.removePaths {
		fmt.Printf("  %s %s\n", red("remove"), path)
	}
	if p.stopNginx {
		fmt.Printf("  %s nginx (it was not running before installation)\n", red("stop"))
	}
	if p.disableNginx {
		fmt.Printf("  %s nginx (it was not enabled at boot before installation)\n", red("disable"))
	}
	for _, pkg := range p.removePackages {
		fmt.Printf("  %s package %s (installed by 'cert init', absent before)\n", red("remove"), pkg)
	}
	for _, bin := range p.removeBinaries {
		fmt.Printf("  %s %s\n", red("remove"), bin)
	}
	for _, warning := range p.warnings {
		fmt.Printf("  %s %s\n", yellow("!"), warning)
	}
}

// handleUninstall restores the server to its pre-installation state.
func handleUninstall(args []string, cfg *Config) {
	yes := false
	dryRun := false
	keepSites := false
	keepPackages := false

	for _, arg := range args {
		switch arg {
		case "--yes", "-y":
			yes = true
		case "--dry-run", "-n":
			dryRun = true
		case "--keep-sites":
			keepSites = true
		case "--keep-packages":
			keepPackages = true
		}
	}

	state := loadState()
	plan := buildUninstallPlan(cfg, state, keepSites, keepPackages)

	fmt.Println(yellow(bold("Uninstall plan (restores the pre-installation state):")))
	fmt.Println()
	plan.print()
	fmt.Println()

	if plan.empty() {
		fmt.Println(green("✅ Nothing to do — this server has no cert-tool installation state."))
		removeBinariesOnly(plan)
		return
	}

	if dryRun {
		fmt.Println(yellow("Dry run — nothing was changed."))
		return
	}

	if !yes {
		fmt.Print("This will restore the server to its pre-installation state. Continue? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, err := reader.ReadString('\n')
		if err != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Aborted — nothing was changed.")
			return
		}
	}

	failures := 0

	// 1. Remove tool-owned nginx configs.
	for _, conf := range plan.removeConfs {
		if err := removePrivilegedFile(conf); err != nil {
			failures++
			fmt.Printf("   %s %s: %v\n", red("✗"), conf, err)
		}
	}

	// 2. Remove issued certificates.
	for _, domain := range plan.removeDomains {
		certPath, keyPath := certFilePaths(cfg.CertDir, domain)
		_ = removePrivilegedFile(certPath)
		_ = removePrivilegedFile(keyPath)
		_ = removePrivilegedFile(metaPath(cfg.CertDir, domain))
		fmt.Printf("   %s certificate for %s\n", green("✓"), domain)
	}

	// 3. Move pre-existing files back.
	for _, moved := range plan.restore {
		if err := runPrivileged("mv", moved.To, moved.From); err != nil {
			failures++
			fmt.Printf("   %s restore %s: %v\n", red("✗"), moved.From, err)
		} else {
			fmt.Printf("   %s restored %s\n", green("✓"), moved.From)
		}
	}

	// 4. Remove tool-owned files and directories.
	for _, path := range plan.removePaths {
		if err := runPrivileged("rm", "-rf", path); err != nil {
			failures++
			fmt.Printf("   %s %s: %v\n", red("✗"), path, err)
		}
	}

	// 5. Restore service state.
	if plan.stopNginx {
		stopService("nginx")
	}
	if plan.disableNginx {
		disableService("nginx")
	}

	// 6. Remove packages that installation added.
	if len(plan.removePackages) > 0 {
		if pm := detectPackageManager(); pm != nil && len(pm.RemoveCmd) > 0 {
			fmt.Printf("   removing packages: %s... ", strings.Join(plan.removePackages, ", "))
			if err := runPrivileged(pm.RemoveCmd[0], append(append([]string{}, pm.RemoveCmd[1:]...), plan.removePackages...)...); err != nil {
				fmt.Printf("%s %v\n", yellow("warning:"), err)
			} else {
				fmt.Println(green("done"))
			}
		}
	}

	// 7. Reload nginx if it is still installed and running.
	if !plan.stopNginx && len(plan.removePackages) == 0 {
		_ = reloadNginx()
	}

	// 8. Remove the binaries (last: we are running from one of them).
	removeBinariesOnly(plan)

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%s uninstall finished with %d failure(s) — please check the output above\n", red(bold("❌")), failures)
		os.Exit(1)
	}
	fmt.Println(green(bold("✅ Uninstalled. The server is back to its pre-installation state.")))
}

// removeBinariesOnly deletes the installed binary (and the running one).
func removeBinariesOnly(plan *uninstallPlan) {
	for _, bin := range plan.removeBinaries {
		_ = removePrivilegedFile(bin)
	}
	if len(plan.removeBinaries) > 0 {
		fmt.Printf("   %s removed %s\n", green("✓"), strings.Join(plan.removeBinaries, ", "))
	}
}
