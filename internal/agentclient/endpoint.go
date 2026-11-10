// Package agentclient 提供 agent 侧的出站 gRPC 客户端:把配置里的端点写法
// 归一化成 gRPC target,并按 CA / 客户端证书 / 私钥三件套装配严格 TLS。
//
// 设计取向是 fail-closed:证书材料缺失、对端不可信、scheme 不认识,一律报错,
// 不做明文降级,也不猜测"大概是哪个地址"。
package agentclient

import (
	"errors"
	"fmt"
	"strings"
)

// Endpoint 把配置里的控制面写法归一化成 gRPC target。
//
// 归一化规则(全部 fail-closed,不接受"猜一个"):
//   - 裸 host:port 原样返回;
//   - https://host:port 去掉 scheme,返回 host:port;
//   - 带 :// 但 authority 为空的自定义 gRPC target(passthrough:///bufnet)
//     原样返回 —— 内存链路与联调依赖它;
//   - 其余(空串、authority 非空的非 https scheme)一律报错,不降级成明文。
func Endpoint(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("agentclient: Endpoint: 地址为空")
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw, nil
	}
	if scheme == "https" {
		if rest == "" {
			return "", fmt.Errorf("agentclient: Endpoint: %q 的 host:port 为空", raw)
		}
		return rest, nil
	}
	if strings.HasPrefix(rest, "/") {
		// gRPC 自定义 target 的空 authority 形态:交给解析器原样处理。
		return raw, nil
	}
	return "", fmt.Errorf("agentclient: Endpoint: 不支持的 scheme %q(只接受 https 或裸 host:port)", scheme)
}
