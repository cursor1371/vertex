package main

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/infra/transport"
	"github.com/bsfdsagfadg/vertex/internal/node/exitpool"
	"github.com/bsfdsagfadg/vertex/internal/node/importer"
)

const (
	maxBootstrapNodesBytes = 1 << 20
	maxBootstrapNodesCount = 1000
)

// parseBootstrapNodes 复用本地导入器，不发起订阅下载或网络探测。
// 显式配置但没有可导入的启用节点时拒绝启动，防止配置错误被空池直连掩盖。
func parseBootstrapNodes(text string, present bool) ([]exitpool.Node, error) {
	if !present {
		return nil, nil
	}
	if len(text) > maxBootstrapNodesBytes {
		return nil, errors.New("VPROXY_BOOTSTRAP_NODES 超过 1 MiB 上限")
	}
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("VPROXY_BOOTSTRAP_NODES 已设置但为空；不需要节点时请删除该变量")
	}
	parsed := importer.NewService().Parse(text)
	if len(parsed) > maxBootstrapNodesCount {
		return nil, errors.New("VPROXY_BOOTSTRAP_NODES 最多允许 1000 个解析节点")
	}
	nodes := make([]exitpool.Node, 0, len(parsed))
	seen := make(map[string]bool, len(parsed))
	for _, node := range parsed {
		if node.Disabled || seen[node.RawURI] {
			continue
		}
		pn, err := transport.ParseURI(node.RawURI)
		if err != nil || pn == nil || !pn.Supported || strings.TrimSpace(pn.Server) == "" || pn.Port < 1 || pn.Port > 65535 {
			continue
		}
		// 导入器的兜底名称可能是 URI 前缀，其中含用户名/密码；启动节点改用编号。
		if node.Name == "" || node.Name == node.RawURI[:min(len(node.RawURI), 40)] {
			node.Name = fmt.Sprintf("bootstrap-%03d", len(nodes)+1)
		}
		seen[node.RawURI] = true
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, errors.New("VPROXY_BOOTSTRAP_NODES 没有受支持且启用的节点；请填写节点 URI 或订阅内容，不是订阅下载地址")
	}
	return nodes, nil
}

// mergeBootstrapNodes 只追加缺失 URI，不清空节点、不复活已有的人工禁用节点。
// MergeNodes 的持久化失败沿用节点池日志处理；此处确认的是内存可调度状态，非网络可达性。
func mergeBootstrapNodes(manager *exitpool.Manager, nodes []exitpool.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	manager.MergeNodes(nodes)
	wanted := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		wanted[node.RawURI] = true
	}
	available := 0
	for _, node := range manager.LoadNodes() {
		if wanted[node.RawURI] && !node.Disabled {
			available++
		}
	}
	if available == 0 {
		return errors.New("启动节点在现有出口池中全部被禁用，请在管理环境中修复状态或更换启动节点")
	}
	log.Printf("[Bootstrap] 出口节点初始化完成（输入去重后 %d 个，池中启用 %d 个；尚未验证网络）", len(nodes), available)
	return nil
}
