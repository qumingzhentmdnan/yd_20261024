package prepare

import (
	"crypto/sha256"
	"fmt"
	"safeagent/internal/manifest"
)

// Policy 是唯一内置渲染输入的规范字节；变更布局必须同步提升版本及镜像登记。
const Policy = "manifest-prepare-v1\nhomeRoot=/home/agent\nworkspace=/workspace/project\nmodelGatewayURL=http://127.0.0.1:18080\n"

// ConfigDigest 绑定发布前可确定的渲染输入，不代替实际计划摘要。
func ConfigDigest() string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(Policy))) }

// Values 将不可复用的准备标识映射为本次实际渲染输入。
func Values(id string) manifest.Values {
	return manifest.Values{Home: "/home/agent/" + id, Workspace: "/workspace/project", ModelGatewayURL: "http://127.0.0.1:18080"}
}
