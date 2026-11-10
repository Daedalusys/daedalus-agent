package agentclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

func TestDial_RejectsMissingCertMaterial(t *testing.T) {
	cases := []struct {
		name string
		cfg  DialConfig
		want string // 错误文案必须包含的字段名
	}{
		{"缺 CA", DialConfig{Addr: "127.0.0.1:9443", CertPath: "/nope.crt", KeyPath: "/nope.key"}, "CAPath"},
		{"缺客户端证书", DialConfig{Addr: "127.0.0.1:9443", KeyPath: "/nope.key", CAPath: "/nope.ca"}, "CertPath"},
		{"缺私钥", DialConfig{Addr: "127.0.0.1:9443", CertPath: "/nope.crt", CAPath: "/nope.ca"}, "KeyPath"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, closer, err := Dial(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("缺证书材料必须拒绝建连")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案 = %q, want 含 %q", err.Error(), tc.want)
			}
			if client != nil || closer != nil {
				t.Fatalf("失败时不得返回 client/closer, got client=%v, closer 非 nil=%v", client, closer != nil)
			}
		})
	}
}

func TestDial_RejectsMissingCertPaths(t *testing.T) {
	// 三件套齐全但文件不存在:必须在 I/O 失败处停下,不得产出连接。
	cfg := DialConfig{
		Addr:     "127.0.0.1:9443",
		CertPath: filepath.Join(t.TempDir(), "absent.crt"),
		KeyPath:  filepath.Join(t.TempDir(), "absent.key"),
		CAPath:   filepath.Join(t.TempDir(), "absent.ca"),
	}
	client, closer, err := Dial(context.Background(), cfg)
	if err == nil {
		t.Fatal("证书文件不存在必须拒绝建连")
	}
	if client != nil || closer != nil {
		t.Fatal("失败时不得返回 client/closer")
	}
}

func TestDialFromConfig_RejectsMissingCertPaths(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil 配置", nil, "nil"},
		{"全空路径", &config.Config{ControlPlaneAddr: "127.0.0.1:9443"}, "ca_path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := DialFromConfig(tc.cfg)
			if err == nil {
				t.Fatal("缺证书路径必须报错,不得静默进入开发模式")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案 = %q, want 含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDialFromConfig_RejectsNonHTTPSScheme(t *testing.T) {
	// 证书路径齐全(存在性由 Endpoint 之后的校验负责),scheme 错误必须先暴露。
	cfg := &config.Config{
		ControlPlaneAddr: "http://127.0.0.1:9443",
		CertPath:         "/nope.crt", KeyPath: "/nope.key", CAPath: "/nope.ca",
	}
	if _, _, err := DialFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("非 https scheme 必须报错, got %v", err)
	}
}

func TestDial_BufconnMTLSRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pki := newTestPKI(t)
	caPath := writePEMFile(t, dir, "ca.pem", pki.caCertPEM)
	serverCertPath, serverKeyPath := issueTestCert(t, dir, "server", pki, "localhost",
		[]string{"localhost"}, x509.ExtKeyUsageServerAuth)
	clientCertPath, clientKeyPath := issueTestCert(t, dir, "client", pki, "client",
		nil, x509.ExtKeyUsageClientAuth)

	lis := bufconn.Listen(1 << 20)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pki.caCertPEM) {
		t.Fatal("测试 CA 加入 pool 失败")
	}
	srvCert, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("加载服务端证书: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	})))
	agentv1.RegisterAgentServiceServer(srv, &echoAgent{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(overrideDialForBufconn(t, lis))

	client, closer, err := Dial(context.Background(), DialConfig{
		Addr:       "passthrough:///bufnet",
		CertPath:   clientCertPath,
		KeyPath:    clientKeyPath,
		CAPath:     caPath,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("Dial 失败: %v", err)
	}
	defer func() { _ = closer() }()

	resp, err := client.Register(context.Background(), &agentv1.RegisterRequest{AgentId: "agent-1"})
	if err != nil {
		t.Fatalf("Register 失败(说明 mTLS 或 target 归一化有问题): %v", err)
	}
	if resp.GetNodeId() != "node-from-peer" {
		t.Fatalf("node_id = %q, want %q", resp.GetNodeId(), "node-from-peer")
	}
}
