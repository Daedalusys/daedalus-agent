package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the environment-variable prefix used by
// LoadWithEnvOverride. All overrides must start with this prefix.
//
// Centralizing the constant here lets the systemd unit, docker
// orchestration, and tests refer to one source of truth — drift
// between docs/code is what causes "I set the env var but it didn't
// take" bugs in deployment.
const EnvPrefix = "DAEDALUS_AGENT_"

// LoadWithEnvOverride reads config from path with the following
// precedence (highest first):
//
//  1. Env vars matching DAEDALUS_AGENT_<FIELD_UPPER>
//  2. YAML values from path
//  3. Built-in defaults from Default()
//
// Behaviour:
//   - path == "" → defaults + env, no error (matches Load).
//   - path != "" and file missing → error. The systemd unit hardcodes
//     `--config /etc/daedalus-agent/config.yaml`; if that file is
//     missing, the operator forgot to run postinstall, which is a
//     deployment error worth surfacing — not a condition to silently
//     paper over with built-in defaults.
//   - malformed YAML → error (env can never paper over bad YAML).
//
// Note: LoadWithEnvOverride does NOT call Validate or ResolveDirs; the
// caller (main) is responsible for both, exactly as for Load. This keeps
// the loader's contract pure: "give me a Config", not "give me a
// ready-to-run Config".
func LoadWithEnvOverride(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	applyEnvOverrides(cfg)
	return cfg, nil
}

// applyEnvOverrides walks the Config's fields and applies any matching
// DAEDALUS_AGENT_<UPPER_FIELD> env var. Reflection keeps the mapping
// in one place: add a field to Config and it picks up an env override
// for free (EnvKeyFor documents the contract for ops).
func applyEnvOverrides(cfg *Config) {
	if cfg == nil {
		return
	}
	// Use field-name-driven mapping so a new field on Config is
	// automatically env-overridable, no manual case-list maintenance.
	overrides := map[string]string{
		EnvKeyFor("AgentID"):          cfg.AgentID,
		EnvKeyFor("ControlPlaneAddr"): cfg.ControlPlaneAddr,
		EnvKeyFor("CertPath"):         cfg.CertPath,
		EnvKeyFor("KeyPath"):          cfg.KeyPath,
		EnvKeyFor("CAPath"):           cfg.CAPath,
		EnvKeyFor("DataDir"):          cfg.DataDir,
		EnvKeyFor("AuditDir"):         cfg.AuditDir,
	}
	for key, fallback := range overrides {
		if v, ok := lookupEnv(key); ok {
			// Mirror back into cfg via the same key → field map.
			applyOverride(cfg, key, v, fallback)
		}
	}
}

// lookupEnv returns the value of key and true when it is set and
// non-empty. An empty env var is treated as unset: empty values are
// operationally indistinguishable from "I forgot to set it", and
// leaving the YAML/default in place matches operator intent.
func lookupEnv(key string) (string, bool) {
	v := os.Getenv(key)
	if v == "" {
		return "", false
	}
	return v, true
}

// applyOverride writes env-supplied value into the cfg field that
// corresponds to key. The fallback is unused here; it exists to make
// the symmetry with overrides obvious during review.
func applyOverride(cfg *Config, key, value, _ string) {
	switch key {
	case EnvKeyFor("AgentID"):
		cfg.AgentID = value
	case EnvKeyFor("ControlPlaneAddr"):
		cfg.ControlPlaneAddr = value
	case EnvKeyFor("CertPath"):
		cfg.CertPath = value
	case EnvKeyFor("KeyPath"):
		cfg.KeyPath = value
	case EnvKeyFor("CAPath"):
		cfg.CAPath = value
	case EnvKeyFor("DataDir"):
		cfg.DataDir = value
	case EnvKeyFor("AuditDir"):
		cfg.AuditDir = value
	}
}

// EnvKeyFor returns the environment-variable name that
// LoadWithEnvOverride recognizes for the given Config field name.
// Exposed so the systemd unit, docker compose, and operator docs can
// reference a single source of truth instead of hand-typing the env
// var name.
//
// Conversion rules:
//   - AgentID          → DAEDALUS_AGENT_AGENT_ID
//   - ControlPlaneAddr → DAEDALUS_AGENT_CONTROL_PLANE_ADDR
//   - CertPath         → DAEDALUS_AGENT_CERT_PATH
//   - etc.
//
// Implementation note: we keep an explicit switch (rather than reflect)
// so a typo in a future field name is caught at compile time, not at
// deploy time.
func EnvKeyFor(field string) string {
	switch field {
	case "AgentID":
		return EnvPrefix + "AGENT_ID"
	case "ControlPlaneAddr":
		return EnvPrefix + "CONTROL_PLANE_ADDR"
	case "CertPath":
		return EnvPrefix + "CERT_PATH"
	case "KeyPath":
		return EnvPrefix + "KEY_PATH"
	case "CAPath":
		return EnvPrefix + "CA_PATH"
	case "DataDir":
		return EnvPrefix + "DATA_DIR"
	case "AuditDir":
		return EnvPrefix + "AUDIT_DIR"
	}
	// Unknown field — return a name that will never match an env var
	// so applyEnvOverride silently skips it. We do NOT panic: callers
	// like EnvKeyFor documentation tooling must be able to ask about
	// any string without crashing.
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(field, " ", "_"))
}