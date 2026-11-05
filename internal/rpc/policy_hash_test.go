package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// sha256HexOf independently recomputes the sha256(hex) of path's bytes.
// It deliberately does not call policyHash so the assertion cannot pass
// by construction (both sides computing the same wrong thing).
func sha256HexOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestPolicyHash_SHA256OfBytes(t *testing.T) {
	got, err := policyHash("testdata/policy.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256HexOf(t, "testdata/policy.toml")
	if got != want {
		t.Errorf("policyHash() = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Errorf("sha256 hex 长度 = %d, want 64", len(got))
	}
}

func TestPolicyHash_EmptyPathIsNoPolicy(t *testing.T) {
	got, err := policyHash("")
	if err != nil {
		t.Fatalf("未配置策略不应报错, got %v", err)
	}
	if got != "" {
		t.Errorf("未配置策略 policyHash() = %q, want 空串", got)
	}
}

func TestPolicyHash_UnreadablePathErrors(t *testing.T) {
	if _, err := policyHash(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("配置了路径但文件缺失必须报错(部署事故不得静默)")
	}
}
