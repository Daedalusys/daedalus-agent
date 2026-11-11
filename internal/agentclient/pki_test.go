package agentclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// testPKI 是测试用的最小 PKI:一张自签 CA 与签发叶子证书所需的私钥。
type testPKI struct {
	caCertPEM, caKeyPEM []byte
	caCert              *x509.Certificate
	caKey               *ecdsa.PrivateKey
}

// newTestPKI 生成一张自签 CA(ECDSA P-256,有效期 10 年)。
func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("生成 CA serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agentclient-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发 CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 CA: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化 CA 私钥: %v", err)
	}
	return testPKI{
		caCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		caKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		caCert:    cert,
		caKey:     key,
	}
}

// issueTestCert 用 pki 签一张叶子证书,并把证书与私钥写进 dir。
func issueTestCert(t *testing.T, dir, name string, pki testPKI, cn string, dns []string, eku x509.ExtKeyUsage) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成叶子私钥: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("生成叶子 serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, pki.caCert, &key.PublicKey, pki.caKey)
	if err != nil {
		t.Fatalf("签发叶子证书: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("序列化叶子私钥: %v", err)
	}
	certPath = writePEMFile(t, dir, name+".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPath = writePEMFile(t, dir, name+"-key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPath, keyPath
}

// writePEMFile 把 PEM 内容写进 dir/name 并返回路径。
func writePEMFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
	return path
}

// overrideDialForBufconn 把建连指向内存 listener,返回复原函数。
func overrideDialForBufconn(t *testing.T, lis *bufconn.Listener) func() {
	t.Helper()
	orig := dialContext
	dialContext = func(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
		return orig(ctx, target, opts...)
	}
	return func() { dialContext = orig }
}

// echoAgent 是只实现 Register 的最小服务端桩。
type echoAgent struct {
	agentv1.UnimplementedAgentServiceServer
}

func (e *echoAgent) Register(context.Context, *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	return &agentv1.RegisterResponse{NodeId: "node-from-peer"}, nil
}
