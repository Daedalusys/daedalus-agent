package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

func TestDefault_HasRequiredFields(t *testing.T) {
	cfg := config.Default()

	if cfg.AgentID == "" {
		t.Error("Default().AgentID is empty")
	}
	if cfg.ControlPlaneAddr == "" {
		t.Error("Default().ControlPlaneAddr is empty")
	}
	if cfg.DataDir == "" {
		t.Error("Default().DataDir is empty")
	}
	if cfg.AuditDir == "" {
		t.Error("Default().AuditDir is empty")
	}
}

func TestLoad_EmptyPath_ReturnsDefault(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.AgentID == "" {
		t.Fatal("Load(\"\") returned empty AgentID")
	}
	if cfg.ControlPlaneAddr == "" {
		t.Fatal("Load(\"\") returned empty ControlPlaneAddr")
	}
}

func TestLoad_MissingFile_ReturnsError(t *testing.T) {
	if _, err := config.Load("/nonexistent/path/to/agent.yaml"); err == nil {
		t.Fatal("Load on missing file: want error, got nil")
	}
}

func TestLoad_YAMLRoundTrip(t *testing.T) {
	yamlDoc := `
agent_id: test-agent
control_plane_addr: cp.example.com:7443
cert_path: /etc/daedalus/tls/agent.pem
key_path: /etc/daedalus/tls/agent.key
ca_path: /etc/daedalus/tls/ca.pem
data_dir: /var/lib/daedalus-agent
audit_dir: /var/log/daedalus-agent
`
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
		t.Fatalf("write test yaml: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]string{
		"AgentID":          "test-agent",
		"ControlPlaneAddr": "cp.example.com:7443",
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

func TestLoad_PartialYAML_LayersOverDefaults(t *testing.T) {
	yamlDoc := `
agent_id: partial-agent
`
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte(yamlDoc), 0o600); err != nil {
		t.Fatalf("write test yaml: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentID != "partial-agent" {
		t.Errorf("AgentID = %q, want partial-agent", cfg.AgentID)
	}
	if cfg.ControlPlaneAddr == "" {
		t.Error("ControlPlaneAddr should fall back to Default()")
	}
	if cfg.DataDir == "" {
		t.Error("DataDir should fall back to Default()")
	}
}

func TestLoad_MalformedYAML_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("agent_id: : :"), 0o600); err != nil {
		t.Fatalf("write test yaml: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load on malformed yaml: want error, got nil")
	}
}

func TestValidate_DefaultConfig_Passes(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default config should validate: %v", err)
	}
}

func TestValidate_EmptyConfig_Fails(t *testing.T) {
	if err := (&config.Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail Validate")
	}
}

func TestValidate_NilConfig_Fails(t *testing.T) {
	if err := (*config.Config)(nil).Validate(); err == nil {
		t.Fatal("nil config should fail Validate")
	}
}

func TestResolveDirs_RelativePaths_BecomeAbsolute(t *testing.T) {
	cfg := &config.Config{
		DataDir:  "data",
		AuditDir: "audit",
		CertPath: "tls/cert.pem",
		KeyPath:  "tls/agent.key",
		CAPath:   "tls/ca.pem",
	}
	cfg.ResolveDirs("/etc/daedalus-agent")

	want := map[string]string{
		"DataDir":  "/etc/daedalus-agent/data",
		"AuditDir": "/etc/daedalus-agent/audit",
		"CertPath": "/etc/daedalus-agent/tls/cert.pem",
		"KeyPath":  "/etc/daedalus-agent/tls/agent.key",
		"CAPath":   "/etc/daedalus-agent/tls/ca.pem",
	}
	got := map[string]string{
		"DataDir":  cfg.DataDir,
		"AuditDir": cfg.AuditDir,
		"CertPath": cfg.CertPath,
		"KeyPath":  cfg.KeyPath,
		"CAPath":   cfg.CAPath,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestResolveDirs_AbsolutePaths_AreUnchanged(t *testing.T) {
	cfg := &config.Config{
		DataDir:  "/var/lib/x",
		AuditDir: "/var/log/y",
	}
	cfg.ResolveDirs("/etc/daedalus-agent")

	if cfg.DataDir != "/var/lib/x" {
		t.Errorf("absolute DataDir was rewritten: %q", cfg.DataDir)
	}
	if cfg.AuditDir != "/var/log/y" {
		t.Errorf("absolute AuditDir was rewritten: %q", cfg.AuditDir)
	}
}

func TestResolveDirs_NilConfig_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ResolveDirs on nil panicked: %v", r)
		}
	}()
	var c *config.Config
	c.ResolveDirs("/etc/daedalus-agent")
}

func TestResolveDirs_EmptyPath_LeftAlone(t *testing.T) {
	cfg := &config.Config{}
	cfg.ResolveDirs("/etc/daedalus-agent")

	if cfg.DataDir != "" {
		t.Errorf("empty DataDir became %q, want empty", cfg.DataDir)
	}
}
