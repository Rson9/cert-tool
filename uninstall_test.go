package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERT_CONFIG", filepath.Join(dir, "config.json"))

	state := loadState()
	state.recordDirCreated("/tmp/does-not-exist-yet")
	state.recordMoved("/etc/nginx/conf.d/default.conf", "/etc/nginx/conf.d/default.conf.bak")
	state.recordMoved("/etc/nginx/conf.d/default.conf", "/etc/nginx/conf.d/default.conf.bak") // idempotent
	if err := state.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadState()
	if len(reloaded.DirsCreated) != 1 {
		t.Errorf("DirsCreated = %v, want 1 entry", reloaded.DirsCreated)
	}
	if len(reloaded.MovedPaths) != 1 {
		t.Errorf("MovedPaths = %v, want exactly 1 entry (duplicate must be ignored)", reloaded.MovedPaths)
	}
	if reloaded.MovedPaths[0].From != "/etc/nginx/conf.d/default.conf" {
		t.Errorf("MovedPaths[0] = %+v", reloaded.MovedPaths[0])
	}
}

func TestBuildUninstallPlan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERT_CONFIG", filepath.Join(dir, "config.json"))

	cfg := &Config{
		CertDir:      filepath.Join(dir, "ssl"),
		NginxConfDir: filepath.Join(dir, "conf.d"),
		WebrootDir:   filepath.Join(dir, "webroot"),
		BaseDomain:   "example.com",
		DefaultPort:  8080,
		Email:        "admin@example.com",
	}
	mustMkdir(t, cfg.CertDir)
	mustMkdir(t, cfg.NginxConfDir)
	mustMkdir(t, cfg.WebrootDir) // recorded as created during installation
	writeConf(t, cfg.NginxConfDir, "api.example.com.conf", "# Managed by cert-tool\nserver { server_name api.example.com; }\n")
	writeConf(t, cfg.NginxConfDir, "foreign.conf", "# someone else's config\nserver { server_name x.example.org; }\n")
	writeConf(t, cfg.NginxConfDir, challengeConfName, "server {}\n")
	writeConf(t, cfg.NginxConfDir, mapConfName, "map $http_upgrade $x {}\n")

	// One issued certificate (identified by its metadata).
	meta := &certMeta{Domain: "api.example.com"}
	if err := saveCertMeta(cfg.CertDir, meta); err != nil {
		t.Fatal(err)
	}

	// A pre-existing default site that was moved aside during installation.
	from := filepath.Join(dir, "default.conf")
	to := filepath.Join(dir, "default.conf.bak")
	writeConf(t, dir, "default.conf.bak", "# distro default\n")

	state := &installState{
		PackagesInstalled: []string{"nginx", "cron"},
		NginxStartedByUs:  true,
		NginxEnabledByUs:  true,
		MovedPaths:        []movedPath{{From: from, To: to}},
		DirsCreated:       []string{cfg.WebrootDir},
	}

	plan := buildUninstallPlan(cfg, state, false, false)

	// Managed confs are removed, the foreign one is never touched.
	if !containsAll(plan.removeConfs,
		filepath.Join(cfg.NginxConfDir, "api.example.com.conf"),
		filepath.Join(cfg.NginxConfDir, challengeConfName),
		filepath.Join(cfg.NginxConfDir, mapConfName)) {
		t.Errorf("removeConfs = %v", plan.removeConfs)
	}
	for _, conf := range plan.removeConfs {
		if filepath.Base(conf) == "foreign.conf" {
			t.Error("foreign.conf must never be removed")
		}
	}

	if len(plan.removeDomains) != 1 || plan.removeDomains[0] != "api.example.com" {
		t.Errorf("removeDomains = %v, want [api.example.com]", plan.removeDomains)
	}
	if len(plan.restore) != 1 || plan.restore[0].From != from {
		t.Errorf("restore = %v, want the recorded default site move", plan.restore)
	}
	if !plan.stopNginx || !plan.disableNginx {
		t.Error("service state started/enabled by us must be restored")
	}
	if len(plan.removePackages) != 2 {
		t.Errorf("removePackages = %v, want [nginx cron]", plan.removePackages)
	}
	if !containsAll(plan.removePaths, cfg.WebrootDir) {
		t.Errorf("removePaths = %v, want the recorded created dir", plan.removePaths)
	}
	for _, path := range plan.removePaths {
		if path == renewalCronPath || path == globalConfigPath {
			t.Errorf("non-existing path must not be planned for removal: %s", path)
		}
	}

	// --keep-sites / --keep-packages: the tool itself goes, everything else stays.
	keepPlan := buildUninstallPlan(cfg, state, true, true)
	if len(keepPlan.removeDomains) != 0 {
		t.Errorf("keep-sites: removeDomains = %v, want none", keepPlan.removeDomains)
	}
	if len(keepPlan.removePackages) != 0 {
		t.Errorf("keep-packages: removePackages = %v, want none", keepPlan.removePackages)
	}
	for _, conf := range keepPlan.removeConfs {
		if filepath.Base(conf) == mapConfName {
			t.Error("keep-sites: the upgrade map must be kept (kept sites depend on it)")
		}
	}
}

func TestUninstallPlanNothingToDo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CERT_CONFIG", filepath.Join(dir, "config.json"))
	cfg := &Config{CertDir: filepath.Join(dir, "ssl"), NginxConfDir: filepath.Join(dir, "conf.d")}
	mustMkdir(t, cfg.CertDir)
	mustMkdir(t, cfg.NginxConfDir)

	plan := buildUninstallPlan(cfg, &installState{}, false, false)
	// Only tool-owned paths are listed; the "nothing to do" short circuit is
	// triggered by the caller for an empty state.
	if plan.stopNginx || plan.disableNginx || len(plan.removeDomains) > 0 || len(plan.removeConfs) > 0 {
		t.Errorf("empty state must not plan any site/service/package action: %+v", plan)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}

func containsAll(list []string, want ...string) bool {
	set := map[string]bool{}
	for _, item := range list {
		set[item] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
