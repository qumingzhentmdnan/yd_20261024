package manifest

import (
	"bytes"
	"encoding/json"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// checkValues 限制真实输入，再校验 Guest 布局和 loopback Bridge 地址。
func checkValues(values Values) error {
	for _, value := range []string{values.Home, values.Workspace, values.ModelGatewayURL} {
		if len(value) > maxValueBytes {
			return ErrLimit
		}
		if !plainText(value) {
			return ErrValues
		}
	}
	if !guestPath(values.Home, "/home/agent") || !guestPath(values.Workspace, "/workspace/project") {
		return ErrValues
	}
	bridge, err := url.Parse(values.ModelGatewayURL)
	if err != nil || bridge.Scheme != "http" || bridge.Hostname() != "127.0.0.1" || bridge.User != nil || strings.ContainsAny(values.ModelGatewayURL, "%?#") {
		return ErrValues
	}
	port, err := strconv.Atoi(bridge.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != bridge.Port() {
		return ErrValues
	}
	if bridge.Path != "" && (!strings.HasPrefix(bridge.Path, "/") || path.Clean(bridge.Path) != bridge.Path) {
		return ErrValues
	}
	return nil
}

// guestPath 检查必须处于固定 Guest 根目录内的规范绝对路径。
func guestPath(value, root string) bool {
	return path.Clean(value) == value && (value == root || strings.HasPrefix(value, root+"/"))
}

// expansion 在分配替换结果前维护单串及整个计划的展开预算。
type expansion struct {
	// values 保存三个已经校验的真实变量值。
	values Values
	// remaining 限制本次 Render 累计生成的字符串字节。
	remaining int
	// replacer 仅对原始文本替换一次，不重解析替换值中的变量。
	replacer *strings.Replacer
}

// expand 在分配结果前检查长度，输入语法已经在加载时校验。
func (e *expansion) expand(source string) (string, error) {
	size := len(source)
	for token, value := range map[string]string{"${home}": e.values.Home, "${workspace}": e.values.Workspace, "${modelGatewayURL}": e.values.ModelGatewayURL} {
		size += strings.Count(source, token) * (len(value) - len(token))
	}
	if size > maxExpandedString || size > e.remaining {
		return "", ErrLimit
	}
	e.remaining -= size
	return e.replacer.Replace(source), nil
}

// tree 生成独立配置树，同时计算键和替换结果的累计预算。
func (e *expansion) tree(value any) (any, error) {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for name, child := range current {
			if len(name) > e.remaining {
				return nil, ErrLimit
			}
			e.remaining -= len(name)
			rendered, err := e.tree(child)
			if err != nil {
				return nil, err
			}
			result[name] = rendered
		}
		return result, nil
	case []any:
		result := make([]any, len(current))
		for i, child := range current {
			rendered, err := e.tree(child)
			if err != nil {
				return nil, err
			}
			result[i] = rendered
		}
		return result, nil
	case string:
		return e.expand(current)
	default:
		return current, nil
	}
}

// boundedBuffer 限制配置序列化的实际输出字节。
type boundedBuffer struct {
	// buffer 持有该配置独立的字节。
	buffer bytes.Buffer
	// remaining 保存实际输出剩余预算。
	remaining int
}

// Write 在写入前拒绝超限，编码器内部空间仍由输入预算约束。
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, ErrLimit
	}
	n, err := b.buffer.Write(p)
	b.remaining -= n
	return n, err
}

// Render 只生成独立计划，既不创建文件也不执行清单中的程序。
func (d *Definition) Render(values Values) (Plan, error) {
	if err := checkValues(values); err != nil {
		return Plan{}, err
	}
	e := expansion{values: values, remaining: maxExpandedTotal, replacer: strings.NewReplacer("${home}", values.Home, "${workspace}", values.Workspace, "${modelGatewayURL}", values.ModelGatewayURL)}
	plan := Plan{Version: 1, ManifestRef: d.descriptor.Ref, ManifestDigest: d.descriptor.ManifestDigest, PackageRef: d.packageRef, Executable: d.runtime.Executable, ExecutableDigest: d.executableDigest, OS: d.descriptor.Platforms[0].OS, Architecture: d.descriptor.Platforms[0].Architecture, User: d.runtime.User, Terminal: d.runtime.Terminal, Home: values.Home, Directory: values.Workspace, Args: make([]string, len(d.runtime.Args)), Environment: make([]Environment, 0, len(d.runtime.Environment)), Files: make([]File, 0, len(d.files))}
	for i, arg := range d.runtime.Args {
		value, err := e.expand(arg)
		if err != nil {
			return Plan{}, err
		}
		plan.Args[i] = value
	}
	for name, source := range d.runtime.Environment {
		value, err := e.expand(source)
		if err != nil {
			return Plan{}, err
		}
		plan.Environment = append(plan.Environment, Environment{Name: name, Value: value})
	}
	slices.SortFunc(plan.Environment, func(a, b Environment) int { return strings.Compare(a.Name, b.Name) })
	remaining := maxFilesBytes
	for _, file := range d.files {
		tree, err := e.tree(file.data)
		if err != nil {
			return Plan{}, err
		}
		output := boundedBuffer{remaining: min(maxFileBytes, remaining)}
		if file.format == "json" {
			err = json.NewEncoder(&output).Encode(tree)
		} else {
			err = toml.NewEncoder(&output).Encode(tree)
		}
		if err != nil {
			return Plan{}, err
		}
		remaining -= output.buffer.Len()
		plan.Files = append(plan.Files, File{Path: file.path, Format: file.format, Mode: 0600, Content: output.buffer.Bytes()})
	}
	// 配置及展开预算已限制组件规模，最终指纹仍检查完整序列化计划大小。
	encoded, _ := json.Marshal(plan)
	if len(encoded) > maxPlanBytes {
		return Plan{}, ErrLimit
	}
	plan.PlanDigest = digest(encoded)
	return plan, nil
}
