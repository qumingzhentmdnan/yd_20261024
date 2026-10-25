package main

import (
	"safeagent/internal/host/launchconfig"
	"safeagent/internal/protocol"
)

// launchConfig 复用不含密钥的可信启动配置。
type launchConfig = launchconfig.Config

// readJSON 复用严格的宿主 JSON 边界校验。
func readJSON(path string, target any) error { return launchconfig.ReadJSON(path, target) }

// loadConfig 校验可信镜像与显式导入选择。
func loadConfig(path string) (launchConfig, protocol.Preparation, error) {
	return launchconfig.Load(path)
}
