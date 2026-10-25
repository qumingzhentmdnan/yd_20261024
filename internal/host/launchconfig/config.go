// Package launchconfig 共享真实 CLI 与本地 Web 的无凭据可信启动配置。
package launchconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"safeagent/internal/host/vm"
	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
)

// Config 保存用户显式选择的宿主配置；不接受任何密钥字段。
type Config struct {
	// Agent 选择镜像已安装的codex或claude。
	Agent string `json:"agent"`
	// QEMU 是可信QEMU可执行文件的绝对路径。
	QEMU string `json:"qemu"`
	// QEMUImg 是可信qemu-img的绝对路径。
	QEMUImg string `json:"qemuImg"`
	// DataRoot 是本入口独占的宿主私有资源目录。
	DataRoot string `json:"dataRoot"`
	// Image 保存可信内核、initramfs及基础盘路径。
	Image vm.Image `json:"image"`
	// ContentFile 是所选镜像构建输出content.json的绝对路径。
	ContentFile string `json:"contentFile"`
	// Accelerator 必须明确选择kvm或tcg，不自动回退。
	Accelerator vm.Accelerator `json:"accelerator"`
	// VCPUs 是本次VM的CPU数量。
	VCPUs uint16 `json:"vcpus"`
	// MemoryMiB 是本次VM内存，单位MiB。
	MemoryMiB uint32 `json:"memoryMiB"`
	// Project 是用户确认导入的宿主绝对源目录。
	Project string `json:"project"`
	// Files 是精确的相对普通文件列表；空列表表示空项目。
	Files []string `json:"files"`
	// Origin 是Host批准的HTTPS模型源站，不含API路径。
	Origin string `json:"origin"`
	// Model 是显式自定义模型；空值使用镜像默认值。
	Model string `json:"model,omitempty"`
	// MCP 保存无凭据的远程工具服务授权。
	MCP []MCP `json:"mcp,omitempty"`
	// AllowedNetworks 显式约束源站DNS解析后的实际拨号地址。
	AllowedNetworks []netip.Prefix `json:"allowedNetworks"`
}

// ReadJSON 限制宿主配置大小并拒绝未知字段、尾随值及截断输入。
func ReadJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("配置超过1MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("配置必须只有一个JSON值")
	}
	return nil
}

// Load 在读取密钥或创建VM之前校验用户选择与镜像登记。
func Load(path string) (Config, protocol.Preparation, error) {
	var config Config
	var content protocol.Preparation
	if err := ReadJSON(path, &config); err != nil {
		return config, content, err
	}
	if config.Agent != "codex" && config.Agent != "claude" {
		return config, content, fmt.Errorf("agent必须为codex或claude")
	}
	for _, path := range []string{config.QEMU, config.QEMUImg, config.DataRoot, config.ContentFile, config.Project} {
		if !filepath.IsAbs(path) {
			return config, content, fmt.Errorf("宿主资源路径必须为绝对路径")
		}
	}
	if config.Accelerator != vm.KVM && config.Accelerator != vm.TCG {
		return config, content, fmt.Errorf("加速器必须显式选择kvm或tcg")
	}
	if err := ReadJSON(config.ContentFile, &content); err != nil {
		return config, content, err
	}
	if !strings.HasPrefix(string(content.ManifestRef), config.Agent+"@") {
		return config, content, fmt.Errorf("镜像清单与所选agent不匹配")
	}
	if err := protocol.Validate(content); err != nil {
		return config, content, err
	}
	if _, err := workspacebundle.ValidateSelection(config.Files, workspacebundle.GuestLimits()); err != nil {
		return config, content, err
	}
	if err := (vm.VMConfig{ID: "validation", Architecture: vm.AMD64, Machine: vm.MicroVM, Accelerator: config.Accelerator, VCPUs: config.VCPUs, MemoryMiB: config.MemoryMiB, Image: config.Image}).Validate(); err != nil {
		return config, content, err
	}
	return config, content, nil
}

// MCP 保存精确 HTTPS 工具服务及经用户确认的工具名，不保存密钥。
type MCP struct {
	// Ref 是逻辑服务名称。
	Ref string `json:"ref"`
	// URL 是远程 Streamable HTTP MCP 端点。
	URL string `json:"url"`
	// AllowedTools 是用户明确允许调用的工具名称。
	AllowedTools []string `json:"allowedTools"`
	// KeyFD 是调用方显式传入的专用凭据FD；零值使用隐藏终端输入。
	KeyFD int `json:"keyFd,omitempty"`
	// AllowedNetworks 限制服务实际拨号地址。
	AllowedNetworks []netip.Prefix `json:"allowedNetworks"`
}
