// policy.go 解析节点侧的远程工具白名单。
//
// 这份白名单与插件运行时的 shared/policy.toml 是两件事:后者管插件自己的
// 行为边界(shellpolicy 命令白名单、pathguard 目录白名单),这里只回答
// 「控制面可以通过 RPC 调用这台机器上的哪些工具」。
package toolcall

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// Policy 是节点侧「允许被控制面远程调用的工具」白名单。
type Policy struct {
	// RemoteTools 是 plugin_id → 工具名集合。
	RemoteTools map[string]map[string]struct{}
}

// policyFile 只声明本包关心的键。解码器忽略未匹配的顶层键:策略文件由
// 控制面下发,键会随版本演进,多一个键不能让节点丢掉整张白名单。
type policyFile struct {
	RemoteTools map[string]any `toml:"remote_tools"`
}

// LoadPolicy 读取并校验远程工具白名单。
//
// path 为空表示未配置策略(没有远程面);路径非空但读不到是部署事故,必须
// 报错而不是退化成空策略,否则「白名单文件丢了」会被误当成「本来就没开」。
// [remote_tools] 内任何形状问题都 fail-closed:宁可让巡检起不来,也不能
// 把写错的条目静默忽略后继续放行其它条目。
func LoadPolicy(path string) (Policy, error) {
	if path == "" {
		return Policy{}, nil
	}
	var raw policyFile
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Policy{}, fmt.Errorf("toolcall: 解析策略 %s: %w", path, err)
	}
	if len(raw.RemoteTools) == 0 {
		return Policy{}, nil
	}
	pol := Policy{RemoteTools: make(map[string]map[string]struct{}, len(raw.RemoteTools))}
	for pluginID, value := range raw.RemoteTools {
		if pluginID == "" {
			return Policy{}, fmt.Errorf("toolcall: 策略 %s 的 [remote_tools] 含空 plugin id", path)
		}
		list, ok := value.([]any)
		if !ok {
			return Policy{}, fmt.Errorf("toolcall: 策略 %s 的 [remote_tools].%q 必须是字符串数组,得到 %T", path, pluginID, value)
		}
		set := make(map[string]struct{}, len(list))
		for i, item := range list {
			name, ok := item.(string)
			if !ok {
				return Policy{}, fmt.Errorf("toolcall: 策略 %s 的 [remote_tools].%q[%d] 必须是字符串,得到 %T", path, pluginID, i, item)
			}
			if name == "" {
				return Policy{}, fmt.Errorf("toolcall: 策略 %s 的 [remote_tools].%q[%d] 是空工具名", path, pluginID, i)
			}
			set[name] = struct{}{}
		}
		pol.RemoteTools[pluginID] = set
	}
	return pol, nil
}

// Allows 报告 (pluginID, toolName) 是否在远程白名单内。
// 空策略不允许任何调用:未配置即没有远程面。
func (p Policy) Allows(pluginID, toolName string) bool {
	tools, ok := p.RemoteTools[pluginID]
	if !ok {
		return false
	}
	_, ok = tools[toolName]
	return ok
}
