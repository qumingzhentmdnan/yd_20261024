package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"safeagent/internal/workspacebundle"
)

// selectionPaths 合并明确文件选择与公开生成配置，同时拒绝重复路径及统一资源限额违规。
func (s *Store) selectionPaths(selection ImportSelection) ([]string, []string, error) {
	paths := slices.Clone(selection.Paths)
	generated := make([]string, 0, len(selection.Generated))
	for name := range selection.Generated {
		paths = append(paths, name)
		generated = append(generated, name)
	}
	paths, err := workspacebundle.ValidateSelection(paths, s.limits)
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(generated)
	return paths, generated, nil
}

// checkGeneratedSource 拒绝覆盖任何已有源配置，包括未被用户选中的同名路径及链接边界。
func checkGeneratedSource(root *os.File, paths []string) error {
	for _, name := range paths {
		file, _, err := openSourceFile(root, name)
		if err == nil {
			file.Close()
			return fmt.Errorf("%w: 生成配置与源文件 %q 冲突", ErrInvalidSelection, name)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// copyGenerated 将公开运行配置写入快照，不在宿主项目目录创建文件。
func (s *Store) copyGenerated(ctx context.Context, name string, data []byte, target string, remaining int64) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if int64(len(data)) > min(s.limits.MaxFileBytes, remaining) {
		return Entry{}, ErrLimitExceeded
	}
	if err := writeSynced(target, data); err != nil {
		return Entry{}, err
	}
	return Entry{Path: name, Size: int64(len(data)), Digest: digestBytes(data)}, nil
}

// validateGenerated 确认持久化的生成路径有序、唯一且确实属于导入基线。
func validateGenerated(paths []string, manifest Manifest, limits Limits) error {
	if len(paths) == 0 {
		return nil
	}
	validated, err := workspacebundle.ValidateSelection(paths, limits)
	if err != nil || !slices.Equal(validated, paths) {
		return ErrCorruptBaseline
	}
	entries := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		entries[entry.Path] = true
	}
	for _, name := range paths {
		if !entries[name] {
			return ErrCorruptBaseline
		}
	}
	return nil
}

// GeneratedPaths 返回已校验且只属于 Host 的导出排除列表，支持进程重启恢复。
func (s *Store) GeneratedPaths(ctx context.Context, id BaselineID) ([]string, error) {
	if _, err := s.Lookup(ctx, id); err != nil {
		return nil, err
	}
	data, err := readBoundedFile(ctx, filepath.Join(s.dataDir, "baselines", string(id), "host.json"), maxHostMetadataBytes)
	if err != nil {
		return nil, err
	}
	var record hostRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, ErrCorruptBaseline
	}
	return record.Generated, nil
}
