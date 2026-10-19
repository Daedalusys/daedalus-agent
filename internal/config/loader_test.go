package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

// envPrefix is the prefix used for env-var overrides. Centralized here
// so the tests don't drift from the implementation.
const envPrefix = "DAEDALUS_AGENT_"

// --- LoadWithEnvOverride(path) ----------------------------------------------

func TestLoadWithEnvOverride_EmptyPath_Defaults(t *testing.T) {
	// t.Setenv unsets/clears any inherited override before the call,
	// so we know the result is purely from built-in defaults.
	for _, k := range []string{
		envPrefix + "AGENT_ID",
		envPrefix + "CONTROL_PLANE_ADDR",
		envPrefix + "DATA_DIR",
	} {
		t.Setenv(k, "")
	}

	cfg, err := config.LoadWithEnvOverride("")
	if err != nil {
		t.Fatalf("LoadWithEnvOverride(\"\"): %v", err)
	}
	if cfg.AgentID == "" {
		t.Error("AgentID should fall back to Default()")
	}
	if cfg.ControlPlaneAddr == "" {
		t.Error("ControlPlaneAddr should fall back to Default()")
	}
	if cfg.DataDir == "" {
		t.Error("DataDir should fall back to Default()")
	}
}

func TestLoadWithEnvOverride_YAMLOnly_NoEnv(t *testing.T) {
	for _, k := range []string{
		envPrefix + "AGENT_ID",
		envPrefix + "CONTROL_PLANE_ADDR",
		envPrefix + "CERT_PATH",
		envPrefix + "KEY_PATH",
		envPrefix + "CA_PATH",
		envPrefix + "DATA_DIR",
		envPrefix + "AUDIT_DIR",
	} {
		t.Setenv(k, "")
	}

	yamlDoc := `
agent_id: from-yaml
control_plane_addr: yaml.example:7443
cert_path: /etc/daedalus/tls/agent.pem
key_path: /etc/daedalus/tls/agent.key
ca_path: /etc/daedalus/tls/ca.pem
data_dir: /var/lib/daedalus-agent
audit_dir: /var/log/daedalus-agent
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := config.LoadWithEnvOverride(path)
	if err != nil {
		t.Fatalf("LoadWithEnvOverride: %v", err)
	}

	want := map[string]string{
		"AgentID":          "from-yaml",
		"ControlPlaneAddr": "yaml.example:7443",
		"CertPath":         "/etc/daedalus/tls/agent.pem",
		"KeyPath":          "/etc/daedalus/tls/agent.key",
		"CAPath":           "/etc/daedalus/tls/ca.pem",
		"DataDir":          "/var/lib/daedalus-agent",
		"AuditDir":         "/var/log/daedalus-agent",
	}
	got := map[string]string{
		"AgentID":          cfg.AgentID,
		"ControlPlaneAddr": cfg.ControlPlaneAddr,
		"CertPath":         cfg.CertPath,
		"KeyPath":          cfg.KeyPath,
		"CAPath":           cfg.CAPath,
		"DataDir":          cfg.DataDir,
		"AuditDir":         cfg.AuditDir,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestLoadWithEnvOverride_EnvBeatsYAML(t *testing.T) {
	// Precedence proof: env vars should override YAML values
	// field-by-field, leaving unset fields to come from YAML.
	yamlDoc := `
agent_id: from-yaml
control_plane_addr: yaml.example:7443
data_dir: /var/lib/daedalus-agent
audit_dir: /var/log/daedalus-agent
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	t.Setenv(envPrefix+"AGENT_ID", "from-env")
	t.Setenv(envPrefix+"CONTROL_PLANE_ADDR", "env.example:7443")
	// CERT_PATH is set via env but not in YAML — should be sourced from env.
	t.Setenv(envPrefix+"CERT_PATH", "/etc/daedalus/tls/env-cert.pem")
	// DATA_DIR and AUDIT_DIR are unset on purpose — should fall through
	// to YAML.
	for _, k := range []string{
		envPrefix + "DATA_DIR",
		envPrefix + "AUDIT_DIR",
		envPrefix + "KEY_PATH",
		envPrefix + "CA_PATH",
	} {
		t.Setenv(k, "")
	}

	cfg, err := config.LoadWithEnvOverride(path)
	if err != nil {
		t.Fatalf("LoadWithEnvOverride: %v", err)
	}

	if cfg.AgentID != "from-env" {
		t.Errorf("AgentID = %q, want from-env (env should beat yaml)", cfg.AgentID)
	}
	if cfg.ControlPlaneAddr != "env.example:7443" {
		t.Errorf("ControlPlaneAddr = %q, want env.example:7443 (env should beat yaml)", cfg.ControlPlaneAddr)
	}
	if cfg.CertPath != "/etc/daedalus/tls/env-cert.pem" {
		t.Errorf("CertPath = %q, want env-supplied path", cfg.CertPath)
	}
	if cfg.DataDir != "/var/lib/daedalus-agent" {
		t.Errorf("DataDir = %q, want yaml value (env unset)", cfg.DataDir)
	}
	if cfg.AuditDir != "/var/log/daedalus-agent" {
		t.Errorf("AuditDir = %q, want yaml value (env unset)", cfg.AuditDir)
	}
}

func TestLoadWithEnvOverride_EnvBeatsDefaults(t *testing.T) {
	// Empty path → defaults only, but env should still apply on top.
	for _, k := range []string{
		envPrefix + "AGENT_ID",
		envPrefix + "CONTROL_PLANE_ADDR",
		envPrefix + "DATA_DIR",
		envPrefix + "AUDIT_DIR",
		envPrefix + "CERT_PATH",
		envPrefix + "KEY_PATH",
		envPrefix + "CA_PATH",
	} {
		t.Setenv(k, "")
	}

	t.Setenv(envPrefix+"AGENT_ID", "env-only-agent")
	t.Setenv(envPrefix+"CONTROL_PLANE_ADDR", "env-only.example:7443")

	cfg, err := config.LoadWithEnvOverride("")
	if err != nil {
		t.Fatalf("LoadWithEnvOverride: %v", err)
	}
	if cfg.AgentID != "env-only-agent" {
		t.Errorf("AgentID = %q, want env-only-agent", cfg.AgentID)
	}
	if cfg.ControlPlaneAddr != "env-only.example:7443" {
		t.Errorf("ControlPlaneAddr = %q, want env-only.example:7443", cfg.ControlPlaneAddr)
	}
	if cfg.DataDir == "" {
		t.Error("DataDir should still fall back to Default() when env unset")
	}
}

func TestLoadWithEnvOverride_MissingYAML_ReturnsError(t *testing.T) {
	for _, k := range []string{
		envPrefix + "AGENT_ID",
		envPrefix + "CONTROL_PLANE_ADDR",
		envPrefix + "DATA_DIR",
		envPrefix + "AUDIT_DIR",
		envPrefix + "CERT_PATH",
		envPrefix + "KEY_PATH",
		envPrefix + "CA_PATH",
	} {
		t.Setenv(k, "")
	}

	// Missing file must error: the systemd unit pins
	// `--config /etc/daedalus-agent/config.yaml` and a missing
	// file there means the operator forgot to run postinstall.
	// Silently falling back to defaults would hide that bug.
	if _, err := config.LoadWithEnvOverride("/nonexistent/path/config.yaml"); err == nil {
		t.Fatal("LoadWithEnvOverride on missing file: want error, got nil")
	}
}

func TestLoadWithEnvOverride_MalformedYAML_ReturnsError(t *testing.T) {
	for _, k := range []string{envPrefix + "AGENT_ID"} {
		t.Setenv(k, "")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("agent_id: : :"), 0o600); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}
	if _, err := config.LoadWithEnvOverride(path); err == nil {
		t.Fatal("LoadWithEnvOverride on malformed yaml: want error, got nil")
	}
}

// TestLoadWithEnvOverride_PrefixIsDAEDALUS_AGENT_ asserts the prefix is
// exactly what the brief specifies, so future maintainers don't silently
// rename it. If this test fails, the daemon's systemd unit / docker
// orchestration that injects env vars is also wrong.
func TestLoadWithEnvOverride_PrefixIsDAEDALUS_AGENT_(t *testing.T) {
	for _, k := range []string{
		envPrefix + "AGENT_ID",
		envPrefix + "CONTROL_PLANE_ADDR",
		envPrefix + "DATA_DIR",
		envPrefix + "AUDIT_DIR",
		envPrefix + "CERT_PATH",
		envPrefix + "KEY_PATH",
		envPrefix + "CA_PATH",
	} {
		t.Setenv(k, "")
	}

	t.Setenv("DAEDALUS_AGENT_AGENT_ID", "prefix-test")

	cfg, err := config.LoadWithEnvOverride("")
	if err != nil {
		t.Fatalf("LoadWithEnvOverride: %v", err)
	}
	if cfg.AgentID != "prefix-test" {
		t.Errorf("AgentID = %q, want prefix-test (DAEDALUS_AGENT_ prefix)", cfg.AgentID)
	}

	// Wrong prefix must be ignored.
	t.Setenv("DAEDALUS_AGENTID", "wrong-prefix")
	cfg2, err := config.LoadWithEnvOverride("")
	if err != nil {
		t.Fatalf("LoadWithEnvOverride (2): %v", err)
	}
	if cfg2.AgentID == "wrong-prefix" {
		t.Errorf("AgentID picked up DAEDALUS_AGENTID (no underscore) — prefix should be DAEDALUS_AGENT_")
	}
	if cfg2.AgentID != "prefix-test" {
		t.Errorf("AgentID = %q, want prefix-test (the correctly-prefixed env should still apply)", cfg2.AgentID)
	}
}

// TestEnvKeyFor_FieldName — internal/indirect: the loader must expose a
// stable mapping so the systemd unit / docker compose / docs can refer
// to the env-var name for a given field without copy-pasting the string.
// This is a guard against accidental drift between field naming and
// env-var naming.
func TestEnvKeyFor_FieldName(t *testing.T) {
	cases := []struct {
		field string
		want  string
	}{
		{"AgentID", envPrefix + "AGENT_ID"},
		{"ControlPlaneAddr", envPrefix + "CONTROL_PLANE_ADDR"},
		{"CertPath", envPrefix + "CERT_PATH"},
		{"KeyPath", envPrefix + "KEY_PATH"},
		{"CAPath", envPrefix + "CA_PATH"},
		{"DataDir", envPrefix + "DATA_DIR"},
		{"AuditDir", envPrefix + "AUDIT_DIR"},
	}
	for _, c := range cases {
		got := config.EnvKeyFor(c.field)
		if !strings.EqualFold(got, c.want) {
			t.Errorf("EnvKeyFor(%q) = %q, want %q", c.field, got, c.want)
		}
	}
}