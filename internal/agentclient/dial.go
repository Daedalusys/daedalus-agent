package agentclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

// DefaultCallTimeout 是单次 RPC 的调用上限。它挂在调用侧而不是建连侧:
// 运维命令需要一个确定的失败时间,但长连接本身不该被这个值绑定。
const DefaultCallTimeout = 30 * time.Second

// dialContext 是建连的注入缝。测试用 bufconn 替换它,从而在没有真实监听端口
// 的情况下走完 mTLS 握手与请求往返。
var dialContext = grpc.DialContext

// DialConfig 是 Dial 的输入。ServerName 留空时从 Addr 的 host 段推导;
// 自定义 gRPC target 没有 host:port 形态,必须显式给出。
type DialConfig struct {
	Addr        string
	CertPath    string
	KeyPath     string
	CAPath      string
	ServerName  string
	CallTimeout time.Duration
}

// Dial 按 cfg 装配严格 mTLS 并返回 AgentService 客户端与关闭函数。
//
// 失败路径一律返回 nil client / nil closer:调用方不需要判断"半成功"。
func Dial(ctx context.Context, cfg DialConfig) (agentv1.AgentServiceClient, func() error, error) {
	target, err := Endpoint(cfg.Addr)
	if err != nil {
		return nil, nil, err
	}
	cfg.Addr = target
	tlsCfg, err := tlsConfigFor(cfg)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialContext(ctx, cfg.Addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, nil, fmt.Errorf("agentclient: 连接 %s 失败: %w", cfg.Addr, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn.Close, nil
}

// DialFromConfig 把配置翻成 DialConfig。
//
// 证书三件套任一为空即拒绝:生产与运维用同一条路径,所以这里不能退化成
// "开发模式下走明文",否则一次配置缺失会把调试命令变成明文 RPC。
func DialFromConfig(cfg *config.Config) (agentv1.AgentServiceClient, func() error, error) {
	if cfg == nil {
		return nil, nil, errors.New("agentclient: DialFromConfig: nil 配置")
	}
	var missing []string
	if cfg.CAPath == "" {
		missing = append(missing, "ca_path")
	}
	if cfg.CertPath == "" {
		missing = append(missing, "cert_path")
	}
	if cfg.KeyPath == "" {
		missing = append(missing, "key_path")
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("agentclient: 缺证书路径 %v:拒绝无 mTLS 建连(fail-closed)", missing)
	}
	return Dial(context.Background(), DialConfig{
		Addr:        cfg.ControlPlaneAddr,
		CertPath:    cfg.CertPath,
		KeyPath:     cfg.KeyPath,
		CAPath:      cfg.CAPath,
		CallTimeout: DefaultCallTimeout,
	})
}

// tlsConfigFor 组装客户端 TLS 配置。
//
// 三项非空校验放在所有 I/O 之前:缺哪个字段就报哪个字段,而不是让
// os.ReadFile("") 的 "no such file or directory" 掩盖配置错误。
// InsecureSkipVerify 保持零值 —— 对端必须被 CA 验证,客户端证书必须被对端要求。
func tlsConfigFor(cfg DialConfig) (*tls.Config, error) {
	var missing []string
	if cfg.CAPath == "" {
		missing = append(missing, "CAPath")
	}
	if cfg.CertPath == "" {
		missing = append(missing, "CertPath")
	}
	if cfg.KeyPath == "" {
		missing = append(missing, "KeyPath")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("agentclient: 缺证书材料 %v:拒绝无 mTLS 建连", missing)
	}

	caPEM, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("agentclient: 读 CA %s 失败: %w", cfg.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("agentclient: CA %s 不含可用证书", cfg.CAPath)
	}
	clientCert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("agentclient: 加载客户端证书 %s / %s 失败: %w", cfg.CertPath, cfg.KeyPath, err)
	}

	serverName := cfg.ServerName
	if serverName == "" {
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("agentclient: Addr %q 无 host:port(自定义 target 需显式 ServerName)", cfg.Addr)
		}
		serverName = host
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
