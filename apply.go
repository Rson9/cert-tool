package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ==========================================
// Declarative site management: cert apply
// ==========================================

// Manifest is the desired state of the whole server. It is read from
// cert-tool.yml (YAML or JSON) and applied idempotently by "cert apply".
type Manifest struct {
	Email       string     `yaml:"email"`
	BaseDomain  string     `yaml:"base_domain"`
	DefaultPort int        `yaml:"default_port"`
	Staging     bool       `yaml:"staging"`
	DNSProvider string     `yaml:"dns_provider"`
	Sites       []SiteSpec `yaml:"sites"`
}

// manifestFileNames are the file names looked up by "cert apply -f".
var manifestFileNames = []string{"cert-tool.yml", "cert-tool.yaml", "cert-tool.json"}

// managedMarker marks nginx configs created by this tool. Only files with this
// marker are ever modified or deleted by "cert apply --prune".
const managedMarker = "Managed by cert-tool"

// loadManifest reads and validates the manifest.
func loadManifest(path string) (*Manifest, error) {
	if path == "" {
		for _, name := range manifestFileNames {
			candidate := filepath.Join(".", name)
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("no manifest found (looked for %s); create one or pass -f <file>", strings.Join(manifestFileNames, ", "))
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	manifest := &Manifest{}
	// YAML is a superset of JSON, so one parser handles both formats.
	if err := yaml.Unmarshal(data, manifest); err != nil {
		return nil, fmt.Errorf("cannot parse manifest %s: %w", path, err)
	}
	if len(manifest.Sites) == 0 {
		return nil, fmt.Errorf("manifest %s contains no sites", path)
	}

	seen := map[string]bool{}
	for i := range manifest.Sites {
		site := &manifest.Sites[i]

		// Manifest-level defaults.
		if site.DNSProvider == "" {
			site.DNSProvider = strings.ToLower(strings.TrimSpace(manifest.DNSProvider))
		}
		if !site.Staging {
			site.Staging = manifest.Staging
		}
		if site.Domain == "" {
			return nil, fmt.Errorf("manifest %s: site #%d has no domain", path, i+1)
		}
		if site.Port == 0 && site.Upstream == "" && manifest.DefaultPort > 0 {
			site.Port = manifest.DefaultPort
		}
		if seen[site.Domain] {
			return nil, fmt.Errorf("manifest %s: duplicate site %q", path, site.Domain)
		}
		seen[site.Domain] = true
	}
	return manifest, nil
}

// ==========================================
// Plan (diff between desired and current state)
// ==========================================

// applyPlan is the computed difference between the manifest and the server.
type applyPlan struct {
	Create    []SiteSpec
	Update    []SiteSpec
	Unchanged []SiteSpec
	Delete    []string // domains managed by cert-tool but absent from the manifest
}

// currentManagedSites scans nginx configs created by this tool and returns
// domain → upstream.
func currentManagedSites(cfg *Config) (map[string]string, error) {
	entries, err := os.ReadDir(cfg.NginxConfDir)
	if err != nil {
		return nil, err
	}

	serverNameRegex := regexp.MustCompile(`server_name\s+([^;]+);`)
	proxyPassRegex := regexp.MustCompile(`proxy_pass\s+([^;]+);`)
	sites := map[string]string{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		if entry.Name() == challengeConfName || entry.Name() == mapConfName {
			continue
		}

		data, err := os.ReadFile(filepath.Join(cfg.NginxConfDir, entry.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		if !strings.Contains(content, managedMarker) {
			// Never touch configs we did not create.
			continue
		}

		domain := ""
		if m := serverNameRegex.FindStringSubmatch(content); len(m) > 1 {
			names := strings.Fields(strings.TrimSpace(m[1]))
			if len(names) > 0 {
				domain = strings.ToLower(names[0])
			}
		}
		if domain == "" {
			continue
		}

		upstream := ""
		if m := proxyPassRegex.FindStringSubmatch(content); len(m) > 1 {
			upstream = strings.TrimSpace(m[1])
		}
		sites[domain] = upstream
	}
	return sites, nil
}

// buildPlan diffs the desired state against the current state.
func buildPlan(cfg *Config, manifest *Manifest) (*applyPlan, error) {
	current, err := currentManagedSites(cfg)
	if err != nil {
		return nil, err
	}

	plan := &applyPlan{}
	desired := map[string]bool{}

	for _, spec := range manifest.Sites {
		desired[spec.Domain] = true
		currentUpstream, exists := current[spec.Domain]
		switch {
		case !exists:
			plan.Create = append(plan.Create, spec)
		case currentUpstream != spec.Upstream:
			plan.Update = append(plan.Update, spec)
		default:
			plan.Unchanged = append(plan.Unchanged, spec)
		}
	}

	for domain := range current {
		if !desired[domain] {
			plan.Delete = append(plan.Delete, domain)
		}
	}
	sort.Strings(plan.Delete)
	return plan, nil
}

func (p *applyPlan) empty() bool {
	return len(p.Create) == 0 && len(p.Update) == 0 && len(p.Delete) == 0
}

// print renders the plan as a human-readable summary.
func (p *applyPlan) print() {
	for _, spec := range p.Create {
		fmt.Printf("  %s %-32s -> %s\n", green("+"), cyan(spec.Domain), spec.Upstream)
	}
	for _, spec := range p.Update {
		fmt.Printf("  %s %-32s -> %s\n", yellow("~"), cyan(spec.Domain), spec.Upstream)
	}
	for _, domain := range p.Delete {
		fmt.Printf("  %s %-32s\n", red("-"), cyan(domain))
	}
	for _, spec := range p.Unchanged {
		fmt.Printf("  %s %-32s -> %s\n", green("="), cyan(spec.Domain), spec.Upstream)
	}
}

// ==========================================
// Apply
// ==========================================

const defaultWorkers = 3

// runProvisioning provisions the given sites concurrently and reloads nginx
// once at the end. Returns per-site results.
func runProvisioning(cfg *Config, specs []SiteSpec, accountDir string) []provisionResult {
	results := make([]provisionResult, len(specs))
	if len(specs) == 0 {
		return results
	}

	sem := make(chan struct{}, currentWorkers())
	var wg sync.WaitGroup

	for i, spec := range specs {
		wg.Add(1)
		go func(index int, spec SiteSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[index] = provisionSite(cfg, spec, accountDir)
		}(i, spec)
	}
	wg.Wait()
	return results
}

// handleApply runs the declarative workflow.
func handleApply(args []string, cfg *Config) {
	manifestPath := ""
	prune := false
	dryRun := false
	workers := defaultWorkers

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--file":
			if i+1 < len(args) {
				manifestPath = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-f=") || strings.HasPrefix(arg, "--file="):
			manifestPath = arg[strings.Index(arg, "=")+1:]
		case arg == "--prune":
			prune = true
		case arg == "--dry-run" || arg == "-n":
			dryRun = true
		case arg == "--workers":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &workers)
				i++
			}
		case strings.HasPrefix(arg, "--workers="):
			fmt.Sscanf(arg[strings.Index(arg, "=")+1:], "%d", &workers)
		}
	}
	if workers < 1 {
		workers = 1
	}
	if workers > 10 {
		workers = 10
	}
	defaultWorkersSet(workers)

	manifest, err := loadManifest(manifestPath)
	if err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}

	// Apply manifest-level identity settings over the global config.
	applyCfg := *cfg
	if manifest.Email != "" {
		applyCfg.Email = manifest.Email
	}
	if manifest.BaseDomain != "" {
		applyCfg.BaseDomain = manifest.BaseDomain
	}
	// E-mail is optional but recommended (CA expiry reminders).
	if applyCfg.Email != "" && !validEmail(applyCfg.Email) {
		fmt.Printf("%s invalid ACME e-mail address: %q (set \"email\" in the manifest)\n", red("Error:"), applyCfg.Email)
		os.Exit(1)
	}
	if applyCfg.Email == "" {
		fmt.Printf("%s no ACME e-mail configured — expiry reminders are disabled (optional: \"email\" in the manifest)\n", yellow("Note:"))
	}

	// Normalize every site up front so configuration errors fail fast
	// (this also expands short subdomains against the base domain).
	seen := map[string]bool{}
	for i := range manifest.Sites {
		if err := manifest.Sites[i].normalize(&applyCfg); err != nil {
			fmt.Printf("%s site %q: %v\n", red("Error:"), manifest.Sites[i].Domain, err)
			os.Exit(1)
		}
		if seen[manifest.Sites[i].Domain] {
			fmt.Printf("%s manifest contains %q more than once\n", red("Error:"), manifest.Sites[i].Domain)
			os.Exit(1)
		}
		seen[manifest.Sites[i].Domain] = true

		// Coexistence check: fail fast when a foreign server block already
		// serves this domain (HTTP-01 would not be able to complete).
		if err := preflightHTTP01(&applyCfg, manifest.Sites[i]); err != nil {
			fmt.Printf("%s %v\n", red("Error:"), err)
			os.Exit(1)
		}
	}

	plan, err := buildPlan(&applyCfg, manifest)
	if err != nil {
		fmt.Printf("%s %v\n", red("Error:"), err)
		os.Exit(1)
	}

	fmt.Println(yellow(bold("Applying site manifest")))
	fmt.Println()
	plan.print()
	fmt.Println()

	if plan.empty() {
		fmt.Println(green(bold("✅ Everything is already up to date.")))
		return
	}

	if dryRun {
		fmt.Println(yellow("Dry run — nothing was changed. Re-run without --dry-run to apply."))
		if !prune && len(plan.Delete) > 0 {
			fmt.Println(yellow("Note: deletions are only performed together with --prune."))
		}
		return
	}

	if !prune {
		if len(plan.Delete) > 0 {
			fmt.Printf("%s %d site(s) are missing from the manifest but will NOT be deleted (use --prune to remove them): %s\n",
				yellow("Note:"), len(plan.Delete), strings.Join(plan.Delete, ", "))
			plan.Delete = nil
		}
	}

	// Shared challenge endpoint first, then provision concurrently.
	if err := ensureSharedConfigs(&applyCfg); err != nil {
		fmt.Printf("%s failed to prepare the ACME challenge endpoint: %v\n", red("Error:"), err)
		os.Exit(1)
	}

	work := append(append([]SiteSpec{}, plan.Create...), plan.Update...)
	fmt.Printf("Provisioning %d site(s) with %d worker(s)...\n", len(work), workers)
	results := runProvisioning(&applyCfg, work, defaultAccountDir)

	failures := 0
	for _, res := range results {
		if res.Err != nil {
			failures++
			fmt.Printf("   %s %s: %v\n", red("✗"), res.Domain, res.Err)
		} else {
			fmt.Printf("   %s %s: %s (valid until %s)\n", green("✓"), res.Domain, res.Action, res.Expiry)
		}
	}

	removed := 0
	for _, domain := range plan.Delete {
		if err := removeSite(&applyCfg, domain); err != nil {
			failures++
			fmt.Printf("   %s remove %s: %v\n", red("✗"), domain, err)
		} else {
			removed++
			fmt.Printf("   %s removed %s\n", green("✓"), domain)
		}
	}

	if failures == 0 {
		if err := reloadNginx(); err != nil {
			fmt.Printf("%s nginx reload failed: %v\n", yellow("Warning:"), err)
		}
	}

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%s apply finished with %d failure(s)\n", red(bold("❌")), failures)
		os.Exit(1)
	}
	fmt.Printf("%s apply complete: %d created, %d updated, %d removed, %d unchanged\n",
		green(bold("✅")), len(plan.Create), len(plan.Update), removed, len(plan.Unchanged))
}

// defaultWorkersSet adjusts the shared worker count used by provisioning and
// renewal batches.
func defaultWorkersSet(n int) {
	workersMu.Lock()
	batchWorkers = n
	workersMu.Unlock()
}

var (
	workersMu    sync.Mutex
	batchWorkers = defaultWorkers
)

func currentWorkers() int {
	workersMu.Lock()
	defer workersMu.Unlock()
	return batchWorkers
}
