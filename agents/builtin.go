// Package agents 提供编译时登记的只读清单，不加载宿主用户配置。
package agents

import (
	"embed"
	"io/fs"
)

// files 只包含明确登记的清单、配置模板及程序摘要，不嵌入大型程序或宿主配置。
//
//go:embed probe/agent.yaml probe/probe.sh probe/templates/settings.json codex/agent.yaml codex/templates/* codex/executable.sha256 claude/agent.yaml claude/templates/settings.json claude/executable.sha256
var files embed.FS

// Builtins 返回编译时只读资源，调用方不能添加任意宿主文件。
func Builtins() fs.FS { return files }
