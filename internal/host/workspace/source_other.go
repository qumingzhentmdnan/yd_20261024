//go:build !linux

package workspace

import "os"

// openSourceRoot 在未验证安全访问的平台明确拒绝复制。
func openSourceRoot(string) (*os.File, error) {
	return nil, ErrUnsupportedPlatform
}

// openSourceFile 在未验证安全访问的平台明确拒绝复制。
func openSourceFile(*os.File, string) (*os.File, os.FileInfo, error) {
	return nil, nil, ErrUnsupportedPlatform
}

// sourceChanged 不在不支持的平台使用。
func sourceChanged(os.FileInfo, os.FileInfo) bool {
	panic("不支持的平台不会进入复制流程")
}
