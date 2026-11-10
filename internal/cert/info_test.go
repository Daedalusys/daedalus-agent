package cert_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/cert"
)

func TestFingerprintFromFile_MatchesCertSHA256(t *testing.T) {
	caPEM, _ := mintCA(t)
	path := filepath.Join(t.TempDir(), "leaf.pem")
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("测试 PEM 解码失败")
	}
	sum := sha256.Sum256(block.Bytes)
	want := hex.EncodeToString(sum[:])

	got, err := cert.FingerprintFromFile(path)
	if err != nil {
		t.Fatalf("FingerprintFromFile: %v", err)
	}
	if got != want {
		t.Fatalf("fingerprint = %q, want %q(sha256(DER) 的小写 hex,无冒号)", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("长度 = %d, want 64", len(got))
	}
}

func TestFingerprintFromFile_Rejects(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path string }{
		{"空路径", ""},
		{"文件不存在", filepath.Join(dir, "absent.pem")},
		{"空文件", empty},
		{"非 PEM", junk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cert.FingerprintFromFile(tc.path); err == nil {
				t.Fatalf("FingerprintFromFile(%q) 必须报错", tc.path)
			}
		})
	}
}
