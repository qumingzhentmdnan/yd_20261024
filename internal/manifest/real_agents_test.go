package manifest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestRealAgentCatalog 验证每镜像只选择一个真实包、固定模型和 loopback 配置，不继承宿主认证。
func TestRealAgentCatalog(t *testing.T) {
	for _, ref := range []string{"codex@0.114.0", "claude@2.1.81"} {
		t.Run(ref, func(t *testing.T) {
			catalog, err := NewCatalogFor(ref)
			if err != nil {
				t.Fatal(err)
			}
			entries := catalog.List()
			if len(entries) != 1 || entries[0].Ref != ref {
				t.Fatal(entries)
			}
			definition, err := catalog.Load(ref, entries[0].ManifestDigest, Platform{OS: "linux", Architecture: "amd64"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := definition.Render(Values{Home: "/home/agent/prepared", Workspace: "/workspace/project", ModelGatewayURL: "http://127.0.0.1:18080"})
			if err != nil {
				t.Fatal(err)
			}
			agent := strings.Split(ref, "@")[0]
			locked, err := os.ReadFile("../../agents/" + agent + "/executable.sha256")
			if err != nil {
				t.Fatal(err)
			}
			if plan.ExecutableDigest != strings.TrimSpace(string(locked)) || plan.Executable != "/opt/agents/"+agent+"/bin/"+agent {
				t.Fatal(plan)
			}
			if len(plan.Files) != 1 || plan.Files[0].Mode != 0600 {
				t.Fatal(plan.Files)
			}
			var config map[string]any
			if agent == "codex" {
				if _, err = toml.Decode(string(plan.Files[0].Content), &config); err != nil {
					t.Fatal(err)
				}
				provider := config["model_providers"].(map[string]any)["safeagent"].(map[string]any)
				if config["model"] != "gpt-5.4" || config["model_provider"] != "safeagent" || provider["base_url"] != "http://127.0.0.1:18080/v1" || provider["wire_api"] != "responses" || provider["requires_openai_auth"] != false {
					t.Fatal(config)
				}
			} else {
				if err = json.Unmarshal(plan.Files[0].Content, &config); err != nil {
					t.Fatal(err)
				}
				env := config["env"].(map[string]any)
				if config["model"] != "claude-sonnet-4-6" || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:18080" || env["ANTHROPIC_AUTH_TOKEN"] != "proxy-managed" {
					t.Fatal(config)
				}
			}
			if _, err = catalog.Load("probe@1.0.0", entries[0].ManifestDigest, Platform{OS: "linux", Architecture: "amd64"}); err != ErrUnknown {
				t.Fatal("非选中包未拒绝", err)
			}
		})
	}
	if _, err := NewCatalogFor("codex@latest"); err != ErrUnknown {
		t.Fatal(err)
	}
}
