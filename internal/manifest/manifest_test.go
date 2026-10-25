package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"
	"safeagent/agents"
)

// testRegistration 固定测试期望的包白名单，不改变生产登记。
func testRegistration() registration {
	return registration{ref: "probe@1.0.0", directory: "probe", packageRef: "probe-builtin-v1", executable: "/opt/agents/probe/bin/probe", executableAsset: "probe.sh", modelProtocol: "test-http", environment: []string{"HOME", "PATH", "PROBE_CONFIG", "SAFEAGENT_PROXY_KEY"}}
}

// testAssets 将真实内置字节复制为包内坏样例的独立输入。
func testAssets(t *testing.T) fstest.MapFS {
	t.Helper()
	result := make(fstest.MapFS)
	err := fs.WalkDir(agents.Builtins(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(agents.Builtins(), name)
		if err != nil {
			return err
		}
		result[name] = &fstest.MapFile{Data: data, Mode: 0444}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// testDefinition 通过正式引用与摘要选择内置定义。
func testDefinition(t *testing.T) *Definition {
	t.Helper()
	catalog, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.List()[0]
	definition, err := catalog.Load(entry.Ref, entry.ManifestDigest, Platform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// testValues 为纯渲染提供明确的 Guest 路径和未监听的 loopback 地址。
func testValues() Values {
	return Values{Home: "/home/agent", Workspace: "/workspace/project", ModelGatewayURL: "http://127.0.0.1:18080/model"}
}

// TestCatalogSelection 验证引用、摘要、平台以及描述返回值隔离。
func TestCatalogSelection(t *testing.T) {
	catalog, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entries := catalog.List()
	if len(entries) != 1 || entries[0].Ref != "probe@1.0.0" {
		t.Fatalf("错误内置登记: %+v", entries)
	}
	entry := entries[0]
	if _, err := catalog.Load("probe@latest", entry.ManifestDigest, entry.Platforms[0]); !errors.Is(err, ErrUnknown) {
		t.Fatal("未拒绝版本回退", err)
	}
	if _, err := catalog.Load(entry.Ref, "sha256:wrong", entry.Platforms[0]); !errors.Is(err, ErrDigest) {
		t.Fatal("未拒绝错误摘要", err)
	}
	if _, err := catalog.Load(entry.Ref, entry.ManifestDigest, Platform{OS: "linux", Architecture: "arm64"}); !errors.Is(err, ErrPlatform) {
		t.Fatal("未拒绝未经支持的平台", err)
	}
	entry.Platforms[0].Architecture = "changed"
	if catalog.List()[0].Platforms[0].Architecture != "amd64" {
		t.Fatal("描述快照污染内置定义")
	}
}

// TestRenderRoundTrip 验证配置转义、字面变量、环境隔离和完整摘要。
func TestRenderRoundTrip(t *testing.T) {
	t.Setenv("MANIFEST_UNRELATED_ENV", "must-not-inherit")
	d := testDefinition(t)
	values := testValues()
	values.Home = `/home/agent/quote"\${workspace}`
	values.Workspace = `/workspace/project/quote"\`
	plan, err := d.Render(values)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Directory != values.Workspace || plan.Home != values.Home || plan.Executable != "/opt/agents/probe/bin/probe" || plan.User != "agent" || len(plan.Environment) != 4 {
		t.Fatalf("运行参数不正确: %+v", plan)
	}
	for _, e := range plan.Environment {
		if e.Name == "HOME" && e.Value != values.Home || e.Name == "MANIFEST_UNRELATED_ENV" {
			t.Fatal("继承或递归展开了环境变量")
		}
	}
	if !strings.HasPrefix(plan.Args[1], values.Home+"/") {
		t.Fatal("argv 中的字面值被重解释")
	}
	for _, file := range plan.Files {
		var decoded map[string]any
		if file.Format == "json" {
			err = json.Unmarshal(file.Content, &decoded)
		} else {
			_, err = toml.Decode(string(file.Content), &decoded)
		}
		if err != nil {
			t.Fatal(err)
		}
		if decoded["home"] != values.Home || decoded["workspace"] != values.Workspace || decoded["model"].(map[string]any)["base_url"] != values.ModelGatewayURL || file.Mode != 0600 || strings.HasPrefix(file.Path, "/") {
			t.Fatalf("配置改变数据语义: %s", file.Content)
		}
	}
	saved := plan.PlanDigest
	plan.PlanDigest = ""
	encoded, _ := json.Marshal(plan)
	if digest(encoded) != saved {
		t.Fatal("完整产物摘要未绑定计划")
	}
}

// TestStableAndIndependentPlans 验证重复渲染确定性与所有返回数据的所有权。
func TestStableAndIndependentPlans(t *testing.T) {
	d := testDefinition(t)
	first, err := d.Render(testValues())
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(first)
	first.Args[0] = "changed"
	first.Environment[0].Value = "changed"
	first.Files[0].Content[0] ^= 1
	descriptor := d.Descriptor()
	descriptor.Platforms[0].OS = "changed"
	second, err := d.Render(testValues())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(second)
	if !bytes.Equal(original, encoded) {
		t.Fatal("调用方修改污染后续渲染")
	}
	const expected = "sha256:7bf3e2dadb36bed6c897e7b605c6e1c125621bd1ce4736122c1369854745ad25"
	if second.PlanDigest != expected {
		t.Fatalf("固定计划黄金摘要: %s", second.PlanDigest)
	}
}

// TestConfigEncodingAndLimit 验证注入形状的文本保持为值，输出超限被实际编码器拒绝。
func TestConfigEncodingAndLimit(t *testing.T) {
	files := testAssets(t)
	message := "quote\" \\ newline\n[attack]\nadmin=true"
	template, _ := json.Marshal(map[string]any{"message": message, "literal.key": "literal", "number": int64(9223372036854775807), "emoji": "😀"})
	files["probe/templates/settings.json"].Data = template
	d, err := loadDefinition(files, testRegistration())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Render(testValues())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range plan.Files {
		var decoded map[string]any
		if file.Format == "json" {
			decoder := json.NewDecoder(bytes.NewReader(file.Content))
			decoder.UseNumber()
			err = decoder.Decode(&decoded)
		} else {
			_, err = toml.Decode(string(file.Content), &decoded)
		}
		if err != nil || decoded["message"] != message || decoded["literal.key"] != "literal" || decoded["emoji"] != "😀" || len(decoded) != 4 {
			t.Fatalf("文本被当成配置语法: %s %v", file.Content, err)
		}
		if fmt.Sprint(decoded["number"]) != "9223372036854775807" {
			t.Fatal("整数精度丢失")
		}
	}
	object := make(map[string]string)
	for i := range 10 {
		object[fmt.Sprintf("key%d", i)] = strings.Repeat("${home}", 8)
	}
	files["probe/templates/settings.json"].Data, _ = json.Marshal(object)
	d, err = loadDefinition(files, testRegistration())
	if err != nil {
		t.Fatal(err)
	}
	values := testValues()
	values.Home = "/home/agent/" + strings.Repeat(`"`, maxValueBytes-len("/home/agent/"))
	if _, err := d.Render(values); !errors.Is(err, ErrLimit) {
		t.Fatalf("编码转义后的输出上限未拒绝: %v", err)
	}
}

// TestSourceChangesBindDigest 验证 YAML 原始字节、模板与程序都参与摘要。
func TestSourceChangesBindDigest(t *testing.T) {
	original := testDefinition(t).Descriptor().ManifestDigest
	for _, name := range []string{"probe/agent.yaml", "probe/probe.sh", "probe/templates/settings.json"} {
		files := testAssets(t)
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".sh") {
			files[name].Data = append(files[name].Data, []byte("\n# digest change\n")...)
		} else {
			files[name].Data = append(files[name].Data, '\n')
		}
		d, err := loadDefinition(files, testRegistration())
		if err != nil {
			t.Fatal(err)
		}
		if d.Descriptor().ManifestDigest == original {
			t.Fatalf("资源变化未绑定: %s", name)
		}
	}
}

// TestBadYAMLAndPolicy 验证执行包的格式和实际权限约束。
func TestBadYAMLAndPolicy(t *testing.T) {
	cases := map[string]func(string) string{
		"未知字段":   func(s string) string { return s + "unknown: value\n" },
		"重复键":    func(s string) string { return s + "kind: AgentManifest\n" },
		"别名锚点":   func(s string) string { return strings.Replace(s, "id: probe", "id: &id probe", 1) },
		"多文档":    func(s string) string { return s + "---\nkind: AgentManifest\n" },
		"隐式类型":   func(s string) string { return strings.Replace(s, "HOME: '${home}'", "HOME: false", 1) },
		"任意程序":   func(s string) string { return strings.Replace(s, "/opt/agents/probe/bin/probe", "/bin/sh", 1) },
		"root用户": func(s string) string { return strings.Replace(s, "user: agent", "user: root", 1) },
		"任意包": func(s string) string {
			return strings.Replace(s, "packageRef: probe-builtin-v1", "packageRef: unknown", 1)
		},
		"环境钩子": func(s string) string { return strings.Replace(s, "PROBE_CONFIG:", "LD_PRELOAD:", 1) },
		"未知变量": func(s string) string { return strings.Replace(s, "${workspace}", "${unknown}", 1) },
		"裸变量":  func(s string) string { return strings.Replace(s, "${home}", "$HOME", 1) },
		"目标穿越": func(s string) string {
			return strings.Replace(s, "${home}/.config/probe/settings.json'\n    template", "${home}/../outside'\n    template", 1)
		},
		"目标重复": func(s string) string {
			return strings.Replace(s, "${home}/.config/probe/settings.toml", "${home}/.config/probe/settings.json", 1)
		},
		"目标父目录冲突": func(s string) string {
			return strings.Replace(s, "${home}/.config/probe/settings.toml", "${home}/.config/probe/settings.json/child", 1)
		},
		"动态目标": func(s string) string {
			return strings.Replace(s, "${home}/.config/probe/settings.toml", "${home}/${workspace}/settings.toml", 1)
		},
		"模板穿越": func(s string) string {
			return strings.ReplaceAll(s, "templates/settings.json", "templates/../outside.json")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			files := testAssets(t)
			files["probe/agent.yaml"].Data = []byte(mutate(string(files["probe/agent.yaml"].Data)))
			if _, err := loadDefinition(files, testRegistration()); !errors.Is(err, ErrManifest) {
				t.Fatalf("坏清单未拒绝: %v", err)
			}
		})
	}
}

// TestBadTemplateData 验证有损 Unicode、重复解码键和不支持的数据结构均拒绝。
func TestBadTemplateData(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"\u0061":2}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800text"}`,
		`{"x":null}`, `{"x":1.2}`, `{"x":9223372036854775808}`, `{"x":[1,"a"]}`, `{"x":[{}]}`, `{"x":[[]]}`,
		`{"${home}":"x"}`, `{"x":"${bad}"}`, `{"x":1} {}`, `[]`, "{\"x\":\"\xff\"}",
	} {
		if _, err := decodeTemplate([]byte(input)); !errors.Is(err, ErrManifest) {
			t.Fatalf("坏模板未拒绝: %q %v", input, err)
		}
	}
	for _, input := range []string{`{"emoji":"\ud83d\ude00"}`, `{"literal":"\\ud800"}`, `{"replacement":"\ufffd"}`} {
		if _, err := decodeTemplate([]byte(input)); err != nil {
			t.Fatalf("合法 Unicode 被拒绝: %s %v", input, err)
		}
	}
}

// TestInputAndExpansionLimits 验证输入、节点、单串和累计展开在分配前受限。
func TestInputAndExpansionLimits(t *testing.T) {
	d := testDefinition(t)
	for _, badURL := range []string{"https://127.0.0.1:8080", "http://localhost:8080", "http://127.0.0.1", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080/#", "http://127.0.0.1:8080/?", "http://127.0.0.1:8080/%61", "http://127.0.0.1:8080/a/../b"} {
		values := testValues()
		values.ModelGatewayURL = badURL
		if _, err := d.Render(values); !errors.Is(err, ErrValues) {
			t.Fatalf("无效 Bridge 地址未拒绝: %s %v", badURL, err)
		}
	}
	values := testValues()
	values.Home = "/etc"
	if _, err := d.Render(values); !errors.Is(err, ErrValues) {
		t.Fatal("无效Guest布局未拒绝", err)
	}
	values = testValues()
	values.Home = "/home/agent/" + strings.Repeat("a", maxValueBytes-len("/home/agent/"))
	if _, err := d.Render(values); err != nil {
		t.Fatal("精确变量上限未接受", err)
	}
	values.Home += "a"
	if _, err := d.Render(values); !errors.Is(err, ErrLimit) {
		t.Fatal("变量超限未拒绝", err)
	}
	files := testAssets(t)
	files["probe/templates/settings.json"].Data = []byte(`{"expanded":"` + strings.Repeat("${home}", 9) + `"}`)
	large, err := loadDefinition(files, testRegistration())
	if err != nil {
		t.Fatal(err)
	}
	values.Home = values.Home[:maxValueBytes]
	if _, err := large.Render(values); !errors.Is(err, ErrLimit) {
		t.Fatal("单串展开超限未拒绝", err)
	}
	object := make(map[string]string)
	for i := range 40 {
		object[fmt.Sprintf("key%d", i)] = strings.Repeat("${home}", 4)
	}
	data, _ := json.Marshal(object)
	files["probe/templates/settings.json"].Data = data
	large, err = loadDefinition(files, testRegistration())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := large.Render(values); !errors.Is(err, ErrLimit) {
		t.Fatal("累计展开超限未拒绝", err)
	}
	for _, input := range []string{strings.Repeat(" ", maxSourceBytes+1), strings.Repeat(`{"x":`, maxDepth+1) + `"s"` + strings.Repeat("}", maxDepth+1), `{"x":[` + strings.Repeat(`"s",`, maxNodes) + `"s"]}`} {
		if _, err := decodeTemplate([]byte(input)); !errors.Is(err, ErrLimit) {
			t.Fatalf("模板资源边界未拒绝: %v", err)
		}
	}
	if _, err := decodeManifest(bytes.Repeat([]byte(" "), maxSourceBytes+1)); !errors.Is(err, ErrLimit) {
		t.Fatal("YAML资源超限未拒绝", err)
	}
}

// TestConcurrentRender 验证私有定义只读并发使用，返回产物互不污染。
func TestConcurrentRender(t *testing.T) {
	d := testDefinition(t)
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for i := range 8 {
		wg.Go(func() {
			values := testValues()
			values.Home += fmt.Sprintf("/session-%d", i)
			plan, err := d.Render(values)
			if err != nil {
				t.Error(err)
				return
			}
			results <- plan.PlanDigest
		})
	}
	wg.Wait()
	close(results)
	var digests []string
	for d := range results {
		digests = append(digests, d)
	}
	slices.Sort(digests)
	if len(slices.Compact(digests)) != 8 {
		t.Fatal("不同输入未得到独立产物")
	}
}
