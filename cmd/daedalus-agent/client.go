package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
	"github.com/Daedalusys/daedalus-sdk/sysinfo"

	"github.com/Daedalusys/daedalus-agent/internal/agentclient"
	"github.com/Daedalusys/daedalus-agent/internal/audit"
	"github.com/Daedalusys/daedalus-agent/internal/cert"
	"github.com/Daedalusys/daedalus-agent/internal/config"
)

// dialClient 是建连的注入缝:测试替换它即可在不碰证书文件的前提下驱动整个
// 子命令路径,并能断言"用法错误压根没有建连"。
var dialClient = agentclient.DialFromConfig

// clientConfig 加载配置并应用 --addr 覆盖,然后把地址归一化成 gRPC target。
// --addr 是操作者把客户端指向"实际在服务的端点"的唯一手段。
func clientConfig(cmd *cobra.Command, cfgPath string) (*config.Config, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	if addr, err := cmd.Flags().GetString("addr"); err == nil && addr != "" {
		cfg.ControlPlaneAddr = addr
	}
	target, err := agentclient.Endpoint(cfg.ControlPlaneAddr)
	if err != nil {
		return nil, fmt.Errorf("--addr/control_plane_addr: %w", err)
	}
	cfg.ControlPlaneAddr = target
	return cfg, nil
}

// printProtoJSON 用 proto 字段名输出缩进 JSON:运维读的是 wire 字段名,
// 不是 protojson 默认的 lowerCamelCase。
func printProtoJSON(w io.Writer, msg proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return fmt.Errorf("序列化响应失败: %w", err)
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// clientCallContext 给单次 RPC 套上确定的上限,避免命令在不可达对端上挂死。
func clientCallContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), agentclient.DefaultCallTimeout)
}

// wrapPeerError 把"对端没有这个服务"翻译成可行动的运维错误:点名地址 + 指向 --addr。
// 其余 RPC 错误只补地址前缀,保持 gRPC 原始状态码可判。
func wrapPeerError(cmd, addr string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("%s: 对端 %s 未提供 AgentService(可能不是控制面/agent 端点);"+
			"用 --addr 指向实际在服务的端点: %w", cmd, addr, err)
	}
	return fmt.Errorf("%s: 调用 %s 失败: %w", cmd, addr, err)
}

// collectRegisterRequest 组装 Register 请求。身份字段全部来自本机:
// 版本用 ldflags 注入的二进制版本,指纹读当前客户端证书,续传点读本地审计链。
func collectRegisterRequest(cfg *config.Config) (*agentv1.RegisterRequest, error) {
	fingerprint, err := cert.FingerprintFromFile(cfg.CertPath)
	if err != nil {
		return nil, fmt.Errorf("读取 cert 指纹: %w", err)
	}
	store, err := audit.New(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("打开审计链: %w", err)
	}
	_, lastSeq, err := store.Since(0)
	if err != nil {
		return nil, fmt.Errorf("读取审计链 seq: %w", err)
	}
	return &agentv1.RegisterRequest{
		AgentId:         cfg.AgentID,
		AgentVersion:    version,
		OsInfo:          collectOsInfo(),
		CertFingerprint: fingerprint,
		LastAuditSeq:    lastSeq,
	}, nil
}

// collectOsInfo 汇总 OS 自报字段。任何一项探测失败都留空:Register 的
// 可用性不该被 /etc/os-release 的缺失拖住,控制面也能接受空字段。
func collectOsInfo() *agentv1.OsInfo {
	release := sysinfo.NewService(nil, "", nil).OSRelease()
	distro, _ := release["NAME"].(string)
	versionID, _ := release["VERSION_ID"].(string)
	var kernel string
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(raw))
	}
	hostname, _ := os.Hostname()
	return &agentv1.OsInfo{
		Distro:   distro,
		Version:  versionID,
		Kernel:   kernel,
		Arch:     runtime.GOARCH,
		Hostname: hostname,
	}
}
