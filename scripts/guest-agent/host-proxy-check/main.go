//go:build linux

// Command host-proxy-check 仅用于独立测试镜像，在UID1000下验证真实CLI经Host代理的请求与响应。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"safeagent/internal/guest/prepare"
	"safeagent/internal/manifest"
)

// run 使用正式清单配置启动真实CLI的非交互测试，不建立Guest假上游或直接网络。
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("必须明确指定codex或claude")
	}
	agent := os.Args[1]
	ref, ok := map[string]string{"codex": "codex@0.114.0", "claude": "claude@2.1.81"}[agent]
	if !ok {
		return fmt.Errorf("未知CLI")
	}
	if os.Getuid() != 1000 || os.Getgid() != 1000 {
		return fmt.Errorf("检查器必须由生产Runner以UID/GID1000启动")
	}
	catalog, err := manifest.NewCatalogFor(ref)
	if err != nil {
		return err
	}
	descriptor := catalog.List()[0]
	definition, err := catalog.Load(ref, descriptor.ManifestDigest, manifest.Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		return err
	}
	plan, err := definition.Render(manifest.Values{Home: "/home/agent/host-proxy-check", Workspace: "/workspace/project", ModelGatewayURL: "http://127.0.0.1:18080"})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = prepare.Install(ctx, plan, 1000, 1000); err != nil {
		return err
	}
	args := []string{"-a", "never", "exec", "--skip-git-repo-check", "--json", "Reply with the upstream text without tools."}
	if agent == "claude" {
		args = []string{"-p", "Reply with the upstream text without tools.", "--output-format", "json", "--settings", plan.Home + "/.claude/settings.json"}
	}
	command := exec.CommandContext(ctx, plan.Executable, args...)
	command.Dir = plan.Directory
	for _, item := range plan.Environment {
		command.Env = append(command.Env, item.Name+"="+item.Value)
	}
	var output, diagnostic bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostic
	err = command.Run()
	fmt.Printf("CLI_HOST_PROXY_OUTPUT=%s\nCLI_HOST_PROXY_DIAGNOSTIC=%s\n", output.String(), diagnostic.String())
	if err != nil {
		return err
	}
	text, err := finalText(agent, output.Bytes())
	if err != nil {
		return err
	}
	fmt.Printf("CLI_HOST_PROXY_RESULT=%q\n", text)
	fmt.Printf("CLI_HOST_PROXY_OK agent=%s uid=%d gid=%d\n", agent, os.Getuid(), os.Getgid())
	return nil
}

// main 仅向测试终端输出无凭据结果，失败返回非零。
func main() {
	if err := run(); err != nil {
		fmt.Println("CLI_HOST_PROXY_ERROR", err)
		os.Exit(1)
	}
}

// codexEvent 只解释固定CLI结构化输出中的最终消息与回合状态。
type codexEvent struct {
	// Type 是CLI事件类型。
	Type string `json:"type"`
	// Item 是事件中的工作项，其他事件为nil。
	Item *codexItem `json:"item"`
}

// codexItem 保存待核对的助手消息文本。
type codexItem struct {
	// Type 区分助手消息与命令等其他项目。
	Type string `json:"type"`
	// Text 保存助手消息正文，其他项目为空。
	Text string `json:"text"`
}

// claudeResult 保存固定Claude JSON输出的成功与最终正文。
type claudeResult struct {
	// Type 必须为result。
	Type string `json:"type"`
	// Subtype 必须为success。
	Subtype string `json:"subtype"`
	// IsError 明确标记推理是否失败。
	IsError bool `json:"is_error"`
	// Result 保存最终助手正文，不包含诊断输出。
	Result string `json:"result"`
}

// finalText 只读取CLI成功回合的结构化助手结果，提示词或诊断子串不作为响应证据。
func finalText(agent string, output []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	if agent == "claude" {
		var result claudeResult
		if err := decoder.Decode(&result); err != nil {
			return "", err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return "", fmt.Errorf("Claude输出不止一个JSON结果")
		}
		if result.Type != "result" || result.Subtype != "success" || result.IsError || result.Result == "" {
			return "", fmt.Errorf("Claude未返回成功助手结果")
		}
		return result.Result, nil
	}
	text, completed := "", false
	for {
		var event codexEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if event.Type == "turn.failed" || event.Type == "error" {
			return "", fmt.Errorf("Codex回合失败")
		}
		if event.Type == "turn.completed" {
			completed = true
		}
		if event.Type == "item.completed" && event.Item != nil && event.Item.Type == "agent_message" {
			text = event.Item.Text
		}
	}
	if !completed || text == "" {
		return "", fmt.Errorf("Codex未返回完整成功助手回合")
	}
	return text, nil
}
