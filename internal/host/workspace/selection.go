package workspace

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// validateSelection 校验外部文件路径并返回独立的已排序清单。
func validateSelection(paths []string, limits Limits) ([]string, error) {
	if len(paths) > limits.MaxFiles {
		return nil, ErrLimitExceeded
	}
	result := slices.Clone(paths)
	slices.Sort(result)
	seen := make(map[string]bool, len(result))
	for _, p := range result {
		if len(p) > limits.MaxPathBytes {
			return nil, ErrLimitExceeded
		}
		if p == "" || !utf8.ValidString(p) || strings.ContainsAny(p, "\\:") {
			return nil, fmt.Errorf("%w: 路径编码或分隔符", ErrInvalidSelection)
		}
		for _, r := range p {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("%w: 路径含控制字符", ErrInvalidSelection)
			}
		}
		for _, part := range strings.Split(p, "/") {
			base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
			reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL"
			reserved = reserved || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9')
			if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || reserved {
				return nil, fmt.Errorf("%w: 路径组件不允许", ErrInvalidSelection)
			}
		}
		key := strings.ToLower(p)
		if seen[key] {
			return nil, fmt.Errorf("%w: 文件路径重复", ErrInvalidSelection)
		}
		seen[key] = true
	}
	for p := range seen {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return nil, fmt.Errorf("%w: 文件占用父目录", ErrInvalidSelection)
			}
		}
	}
	return result, nil
}
