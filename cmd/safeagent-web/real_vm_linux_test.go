package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"safeagent/internal/host/api"
)

// TestRealWebVMExport 验证真实HTTP入口、Codex PTY本地命令、停止冻结和显式确认导出；不访问模型供应商。
func TestRealWebVMExport(t *testing.T) {
	configPath := os.Getenv("SAFEAGENT_WEB_CONFIG")
	if configPath == "" {
		t.Skip("需显式提供SAFEAGENT_WEB_CONFIG与真实生产镜像")
	}
	config, content, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config.DataRoot = filepath.Join(t.TempDir(), "runtime")
	project := t.TempDir()
	if output, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", output, err)
	}
	appServer := httptest.NewUnstartedServer(nil)
	origin := "http://" + appServer.Listener.Addr().String()
	app, err := api.New(origin, func(ctx context.Context, req api.Request, interact func(api.Terminal) error, review func(api.Review, func(context.Context) error) error) error {
		err := launch(ctx, config, content, req, interact, review)
		if err != nil {
			t.Logf("测试后端错误: %v", err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	appServer.Config.Handler = app.Handler(nil, nil)
	appServer.Start()
	defer appServer.Close()
	token := strings.SplitN(app.URL(), "#", 2)[1]
	client := appServer.Client()
	client.Timeout = 15 * time.Second
	// apiCall 模拟带同源认证的浏览器请求，不打印凭据或访问令牌。
	apiCall := func(path string, body any, target any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest("POST", origin+"/api/"+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", origin)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("%s: status=%d %s", path, response.StatusCode, data)
		}
		if target != nil {
			if err = json.NewDecoder(response.Body).Decode(target); err != nil {
				t.Fatal(err)
			}
		}
	}
	apiCall("start", api.Request{Agent: "codex", Project: project, Files: []string{}, Origin: "https://example.com", Key: "fake-web-host-only-key", Model: "web-test-model"}, nil)
	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	output := ""
	var outputOffset int64
	steps := map[string]bool{}
	commandSent := false
	enterSent := false
	reviewRequested := false
	escape := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	command := "!printf WEB_VM_EXPORTED > web-result.txt"
	for {
		select {
		case <-timeout.C:
			t.Fatalf("Web VM超时，终端=%s", output)
		case <-ticker.C:
		}
		var poll struct {
			// State 是当前会话阶段。
			State string `json:"state"`
			// Output 是原始PTY输出。
			Output []byte `json:"output"`
			// OutputStart 是当前缓存首字节在会话中的偏移。
			OutputStart int64 `json:"outputStart"`
			// Review 是冻结的变更审核。
			Review *api.Review `json:"review"`
		}
		apiCall("poll", struct{}{}, &poll)
		skip := max(int64(0), outputOffset-poll.OutputStart)
		if skip < int64(len(poll.Output)) {
			poll.Output = poll.Output[skip:]
			output += string(poll.Output)
			outputOffset = poll.OutputStart + skip + int64(len(poll.Output))
		} else {
			poll.Output = nil
		}
		if strings.Contains(output, "fake-web-host-only-key") {
			t.Fatal("Host测试密钥泄露到终端")
		}
		if strings.HasPrefix(poll.State, "failed") {
			t.Fatalf("VM会话失败，终端=%s", output)
		}
		if poll.State == "running" && bytes.Contains(poll.Output, []byte("\x1b[6n")) {
			apiCall("input", map[string][]byte{"data": []byte("\x1b[1;1R")}, nil)
		}
		visible := strings.Join(strings.Fields(escape.ReplaceAllString(output, " ")), " ")
		for _, step := range []string{"Welcome to Codex", "Choose the text style", "Do you trust the files", "Do you trust this", "Press Enter to continue", "Yes, I trust this folder"} {
			if strings.Contains(visible, step) && !steps[step] {
				steps[step] = true
				apiCall("input", map[string][]byte{"data": []byte("\r")}, nil)
			}
		}
		if !commandSent && strings.Contains(visible, "model: web-test-model") {
			commandSent = true
			apiCall("input", map[string][]byte{"data": []byte(command)}, nil)
		}
		if commandSent && !enterSent && strings.Contains(visible, "web-result.txt") {
			enterSent = true
			apiCall("input", map[string][]byte{"data": []byte("\r")}, nil)
		}
		if enterSent && !reviewRequested && (strings.Contains(visible, "Process exited with code 0") || strings.Contains(visible, "You ran printf")) {
			reviewRequested = true
			apiCall("review", struct{}{}, nil)
		}
		if poll.Review != nil {
			if !strings.Contains(poll.Review.Diff, "WEB_VM_EXPORTED") {
				t.Fatalf("未生成目标变更：%s，终端=%s", poll.Review.Diff, visible)
			}

			// 先验证宿主并发新增冲突停止覆盖，审核仍可保留供下一次明确确认。
			target := filepath.Join(project, "web-result.txt")
			if err := os.WriteFile(target, []byte("HOST_CONFLICT"), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"id": poll.Review.ID})
			conflictRequest, _ := http.NewRequest("POST", origin+"/api/apply", bytes.NewReader(body))
			conflictRequest.Header.Set("Authorization", "Bearer "+token)
			conflictRequest.Header.Set("Origin", origin)
			response, err := client.Do(conflictRequest)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 409 {
				t.Fatalf("冲突未拒绝: %d", response.StatusCode)
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "HOST_CONFLICT" {
				t.Fatal("冲突覆盖了宿主内容")
			}
			if err = os.Remove(target); err != nil {
				t.Fatal(err)
			}
			apiCall("apply", map[string]string{"id": poll.Review.ID}, nil)
			data, err = os.ReadFile(filepath.Join(project, "web-result.txt"))
			if err != nil || string(data) != "WEB_VM_EXPORTED" {
				t.Fatalf("导出失败: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(project, ".codex/config.toml")); !os.IsNotExist(err) {
				t.Fatal("生成配置被导出")
			}
			// 等待会话完整回收，不能用测试主动取消掩盖正常完成结果。
			for {
				var final struct {
					// State 是完整清理后的会话阶段。
					State string `json:"state"`
				}
				apiCall("poll", struct{}{}, &final)
				if final.State == "finished" {
					break
				}
				if strings.HasPrefix(final.State, "failed") {
					t.Fatalf("导出后清理失败: %s", final.State)
				}
				select {
				case <-timeout.C:
					t.Fatal("导出后等待清理超时")
				case <-ticker.C:
				}
			}
			t.Log(fmt.Sprintf("真实HTTP→Codex PTY→冻结审核→冲突拒绝→确认导出通过，审核ID=%s；未验收模型供应商或浏览器TUI渲染", poll.Review.ID))
			return
		}
	}
}
