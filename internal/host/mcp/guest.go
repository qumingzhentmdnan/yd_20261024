package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/BurntSushi/toml"
	"safeagent/internal/host/gateway"
)

// GuestFiles 生成只包含自定义模型和 Guest 回环地址的项目配置，不读取或修改用户文件。
func GuestFiles(agent, model string, refs []string) (map[string][]byte, error) {
	if agent != "codex" && agent != "claude" || len(model) > 128 || strings.ContainsAny(model, "\r\n\x00") {
		return nil, gateway.ErrInvalidConfig
	}
	servers := make(map[string]any, len(refs))
	for _, ref := range refs {
		if !serviceName.MatchString(ref) {
			return nil, gateway.ErrInvalidConfig
		}
		if _, exists := servers[ref]; exists {
			return nil, gateway.ErrInvalidConfig
		}
		server := map[string]any{"url": "http://127.0.0.1:18080/mcp/" + ref}
		if agent == "claude" {
			server["type"] = "http"
		}
		servers[ref] = server
	}
	files := make(map[string][]byte)
	if agent == "codex" {
		config := make(map[string]any)
		if model != "" {
			config["model"] = model
		}
		if len(refs) > 0 {
			config["mcp_servers"] = servers
		}
		if len(config) > 0 {
			var b bytes.Buffer
			if err := toml.NewEncoder(&b).Encode(config); err != nil {
				return nil, err
			}
			files[".codex/config.toml"] = b.Bytes()
		}
	} else {
		if model != "" {
			data, err := json.Marshal(map[string]any{"model": model, "env": map[string]string{"ANTHROPIC_MODEL": model, "ANTHROPIC_DEFAULT_HAIKU_MODEL": model}})
			if err != nil {
				return nil, err
			}
			files[".claude/settings.json"] = data
		}
		if len(refs) > 0 {
			data, err := json.Marshal(map[string]any{"mcpServers": servers})
			if err != nil {
				return nil, err
			}
			files[".mcp.json"] = data
		}
	}
	return files, nil
}
