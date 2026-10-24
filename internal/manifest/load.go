package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"safeagent/agents"
)

// registration 保存可信内置包登记，字段不由 YAML 自报决定。
type registration struct {
	// ref 固定允许加载的完整引用。
	ref string
	// directory 指定只读资源中的包目录。
	directory string
	// packageRef 标识镜像预装包。
	packageRef string
	// executable 固定 Guest 可执行路径。
	executable string
	// executableAsset 指定包内参与摘要的实际程序文件。
	executableAsset string
	// environment 指定该包允许声明的环境名称。
	environment []string
}

// compiledFile 保存加载时固定的目标与已解析结构模板。
type compiledFile struct {
	// path 是相对隔离 HOME 的静态路径。
	path string
	// format 选择 JSON 或 TOML 编码器。
	format string
	// data 是不会向调用方暴露的配置树。
	data map[string]any
}

// Definition 是不可变内置定义，只能通过 Catalog.Load 取得。
type Definition struct {
	// descriptor 保存引用、摘要与平台声明。
	descriptor Descriptor
	// runtime 保存已经校验的固定运行字段。
	runtime runtimeSpec
	// packageRef 绑定可信包引用。
	packageRef string
	// executableDigest 绑定内置测试程序的实际字节。
	executableDigest string
	// files 保存已校验、排序的配置定义。
	files []compiledFile
}

// Catalog 只持有编译时允许的内置定义，不开放动态注册。
type Catalog struct {
	// entries 按完整引用查找不可变定义。
	entries map[string]*Definition
}

// NewCatalog 加载唯一明确登记的 probe，不把空 CLI 文件当作可运行内置清单。
func NewCatalog() (*Catalog, error) {
	reg := registration{ref: "probe@1.0.0", directory: "probe", packageRef: "probe-builtin-v1", executable: "/opt/agents/probe/bin/probe", executableAsset: "probe.sh", environment: []string{"HOME", "PATH", "PROBE_CONFIG", "SAFEAGENT_PROXY_KEY"}}
	definition, err := loadDefinition(agents.Builtins(), reg)
	if err != nil {
		return nil, err
	}
	return &Catalog{entries: map[string]*Definition{reg.ref: definition}}, nil
}

// List 返回独立且按引用排序的内置描述。
func (c *Catalog) List() []Descriptor {
	result := make([]Descriptor, 0, len(c.entries))
	for _, definition := range c.entries {
		result = append(result, definition.Descriptor())
	}
	slices.SortFunc(result, func(a, b Descriptor) int { return strings.Compare(a.Ref, b.Ref) })
	return result
}

// Load 按确切引用、摘要及平台选择，不自动回退版本或架构。
func (c *Catalog) Load(ref, expectedDigest string, platform Platform) (*Definition, error) {
	definition, exists := c.entries[ref]
	if !exists {
		return nil, ErrUnknown
	}
	if expectedDigest != definition.descriptor.ManifestDigest {
		return nil, ErrDigest
	}
	if !slices.Contains(definition.descriptor.Platforms, platform) {
		return nil, ErrPlatform
	}
	return definition, nil
}

// Descriptor 返回独立的平台切片，不暴露共享定义。
func (d *Definition) Descriptor() Descriptor {
	result := d.descriptor
	result.Platforms = slices.Clone(result.Platforms)
	return result
}

// readAsset 在解析前限制源字节数，并保留实际资源读取错误。
func readAsset(files fs.FS, name string) ([]byte, error) {
	f, err := files.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	if len(data) > maxSourceBytes {
		return nil, ErrLimit
	}
	return data, nil
}

// digest 返回算法前缀及小写十六进制 SHA-256。
func digest(data []byte) string {
	value := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(value[:])
}

// fingerprint 保存内部固定结构的资源摘要，不是通用 JSON 规范化协议。
type fingerprint struct {
	// Manifest 绑定原始 YAML，包括注释及空白。
	Manifest string
	// Executable 绑定实际内置测试程序字节。
	Executable string
	// Templates 按模板路径记录内容摘要，JSON 编码器固定排序映射键。
	Templates map[string]string
}

// loadDefinition 只供内置入口和包内坏样例测试使用，不开放任意文件系统注册。
func loadDefinition(files fs.FS, reg registration) (*Definition, error) {
	raw, err := readAsset(files, path.Join(reg.directory, "agent.yaml"))
	if err != nil {
		return nil, err
	}
	spec, err := decodeManifest(raw)
	if err != nil {
		return nil, err
	}
	if err := validateSpec(spec, reg); err != nil {
		return nil, err
	}
	executable, err := readAsset(files, path.Join(reg.directory, reg.executableAsset))
	if err != nil {
		return nil, err
	}
	record := fingerprint{Manifest: digest(raw), Executable: digest(executable), Templates: make(map[string]string)}
	compiled := make([]compiledFile, 0, len(spec.Files))
	templates := make(map[string]map[string]any)
	for _, file := range spec.Files {
		tree, exists := templates[file.Template]
		if !exists {
			data, err := readAsset(files, path.Join(reg.directory, file.Template))
			if err != nil {
				return nil, err
			}
			tree, err = decodeTemplate(data)
			if err != nil {
				return nil, fmt.Errorf("%w: 模板 %s", err, file.Template)
			}
			templates[file.Template] = tree
			record.Templates[file.Template] = digest(data)
		}
		compiled = append(compiled, compiledFile{path: strings.TrimPrefix(file.Destination, "${home}/"), format: file.Format, data: tree})
	}
	slices.SortFunc(compiled, func(a, b compiledFile) int { return strings.Compare(a.path, b.path) })
	encoded, _ := json.Marshal(record)
	platforms := make([]Platform, 0, len(spec.Metadata.Support.Architectures))
	for _, arch := range spec.Metadata.Support.Architectures {
		platforms = append(platforms, Platform{OS: spec.Metadata.Support.OS, Architecture: arch})
	}
	return &Definition{descriptor: Descriptor{Ref: reg.ref, DisplayName: spec.Metadata.DisplayName, ManifestDigest: digest(encoded), Platforms: platforms, ModelProtocol: spec.Capabilities.Model.Protocol}, runtime: spec.Runtime, packageRef: reg.packageRef, executableDigest: record.Executable, files: compiled}, nil
}

// decodeManifest 先拒绝展开及类型混淆，再通过 KnownFields 拒绝字段漂移。
func decodeManifest(data []byte) (manifestSpec, error) {
	var result manifestSpec
	if len(data) > maxSourceBytes {
		return result, ErrLimit
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return result, fmt.Errorf("%w: YAML 解码: %w", ErrManifest, err)
	}
	count := 0
	if err := checkYAML(&document, 0, &count); err != nil {
		return result, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, fmt.Errorf("%w: 只允许单份 YAML", ErrManifest)
	}
	decoder = yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("%w: YAML 字段或类型: %w", ErrManifest, err)
	}
	return result, nil
}

// checkYAML 在类型解码前限制真实外部节点，拒绝别名、重复键及非字符串叶子。
func checkYAML(node *yaml.Node, depth int, count *int) error {
	*count += 1
	if depth > maxDepth || *count > maxNodes || len(node.Value) > maxStringBytes {
		return ErrLimit
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return ErrManifest
	}
	switch node.Kind {
	case yaml.DocumentNode:
	case yaml.MappingNode:
		if node.Tag != "!!map" {
			return ErrManifest
		}
		keys := make(map[string]bool, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || keys[key.Value] {
				return ErrManifest
			}
			keys[key.Value] = true
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return ErrManifest
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return ErrManifest
		}
	default:
		return ErrManifest
	}
	for _, child := range node.Content {
		if err := checkYAML(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// validateSpec 校验可执行清单的实际包授权及输出约束。
func validateSpec(s manifestSpec, reg registration) error {
	m := s.Metadata
	r := s.Runtime
	if s.APIVersion != "safeagent.io/v1" || s.Kind != "AgentManifest" || !idPattern.MatchString(m.ID) || !versionPattern.MatchString(m.Version) || len(m.Version) > 64 || m.ID+"@"+m.Version != reg.ref || m.DisplayName == "" || m.Provenance.Source != "builtin" || m.Provenance.PackageRef != reg.packageRef {
		return ErrManifest
	}
	if m.Support.OS != "linux" || !slices.Equal(m.Support.Architectures, []string{"amd64"}) || !slices.Equal(m.Support.Modes, []string{"pty"}) || s.Capabilities.Model.Protocol != "test-http" {
		return ErrManifest
	}
	if r.Executable != reg.executable || r.Cwd != "${workspace}" || r.Terminal != "pty" || r.User != "agent" || r.Environment["HOME"] != "${home}" || r.Environment["PATH"] != "/usr/bin:/bin" {
		return ErrManifest
	}
	if len(r.Args) > 64 || len(r.Environment) > 32 || len(s.Files) > 16 {
		return ErrLimit
	}
	for _, arg := range r.Args {
		if err := checkVariables(arg); err != nil {
			return err
		}
	}
	for name, value := range r.Environment {
		if !slices.Contains(reg.environment, name) || name == "SAFEAGENT_PROXY_KEY" && value != "proxy-managed" {
			return ErrManifest
		}
		if err := checkVariables(value); err != nil {
			return err
		}
	}
	paths := make(map[string]bool, len(s.Files))
	for _, file := range s.Files {
		if !strings.HasPrefix(file.Destination, "${home}/") || !relativePath(strings.TrimPrefix(file.Destination, "${home}/")) || !strings.HasPrefix(file.Template, "templates/") || !relativePath(file.Template) || file.Format != "json" && file.Format != "toml" {
			return ErrManifest
		}
		if paths[file.Destination] {
			return ErrManifest
		}
		paths[file.Destination] = true
	}
	for target := range paths {
		for parent := path.Dir(target); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return ErrManifest
			}
		}
	}
	return nil
}

// plainText 拒绝路径或 URL 中的非法 UTF-8 及控制字符。
func plainText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// relativePath 校验静态包路径，不使用宿主 filepath 语义。
func relativePath(s string) bool {
	if s == "" || s == "." || strings.HasPrefix(s, "/") || path.Clean(s) != s || strings.ContainsAny(s, "\\$") || !plainText(s) {
		return false
	}
	return !slices.Contains(strings.Split(s, "/"), "..")
}

// checkVariables 在加载时只允许三种明确变量，不读取任何环境值。
func checkVariables(s string) error {
	for {
		i := strings.IndexByte(s, '$')
		if i < 0 {
			return nil
		}
		s = s[i:]
		matched := false
		for _, token := range []string{"${home}", "${workspace}", "${modelGatewayURL}"} {
			if strings.HasPrefix(s, token) {
				s = s[len(token):]
				matched = true
				break
			}
		}
		if !matched {
			return ErrManifest
		}
	}
}
