// Package agents 提供编译时登记的只读测试清单，不加载宿主用户配置。
package agents

import (
	"embed"
	"io/fs"
)

// files 只包含明确登记的 probe 资源，不将空 CLI 占位文件登记为可运行配置。
//
//go:embed probe/agent.yaml probe/probe.sh probe/templates/settings.json
var files embed.FS

// Builtins 返回编译时只读资源，调用方不能添加任意宿主文件。
func Builtins() fs.FS { return files }
