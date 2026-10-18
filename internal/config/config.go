// Package config owns the daemon's YAML-loaded configuration.
//
// The config is intentionally flat: every field is a string path or scalar.
// T2 will expand it with env-var overrides and a richer loader, but for T1
// the surface is just "struct + YAML round-trip + Validate".
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds the daemon's runtime configuration.
//
// Field tags use snake_case to match the YAML schema. Field order in the
// struct is the order presented in --help output of any future generated
// docs; keep it stable.
type Config struct {
	AgentID          string `yaml:"agent_id"`
	ControlPlaneAddr string `yaml:"control_plane_addr"`
	CertPath         string `yaml:"cert_path"`
	KeyPath          string `yaml:"key_path"`
	CAPath           string `yaml:"ca_path"`
	DataDir          string `yaml:"data_dir"`
	AuditDir         string `yaml:"audit_dir"`
}

// Default returns a Config populated with sane defaults for local
// development. Production deployments must override ControlPlaneAddr,
// CertPath, KeyPath, and CAPath via a YAML file.
func Default() *Config {
	return &Config{
		AgentID:          "agent-local",
		ControlPlaneAddr: "https://control.daedalusys.local:7443",
		DataDir:          "/var/lib/daedalus-agent",
		AuditDir:         "/var/log/daedalus-agent",
	}
}

// Load reads config from path. If path is empty, Default() is returned.
// Missing files produce an error; partial files are layered on top of
// Default() so unset fields keep their built-in defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		return Default(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

// Validate enforces the minimum required fields. T1 only requires
// AgentID, ControlPlaneAddr, DataDir, and AuditDir; T3 will add cert
// validation when mTLS lands.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil")
	}
	var missing []string
	if c.AgentID == "" {
		missing = append(missing, "agent_id")
	}
	if c.ControlPlaneAddr == "" {
		missing = append(missing, "control_plane_addr")
	}
	if c.DataDir == "" {
		missing = append(missing, "data_dir")
	}
	if c.AuditDir == "" {
		missing = append(missing, "audit_dir")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required fields: %v", missing)
	}
	return nil
}

// ResolveDirs makes relative paths absolute against baseDir. Already
// absolute paths are left untouched. This is called by main when the
// config file lives outside /etc so relative paths in it resolve
// relative to the file's directory.
func (c *Config) ResolveDirs(baseDir string) {
	if c == nil {
		return
	}
	c.DataDir = resolvePath(c.DataDir, baseDir)
	c.AuditDir = resolvePath(c.AuditDir, baseDir)
	c.CertPath = resolvePath(c.CertPath, baseDir)
	c.KeyPath = resolvePath(c.KeyPath, baseDir)
	c.CAPath = resolvePath(c.CAPath, baseDir)
}

func resolvePath(p, base string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
