package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"safeagent/internal/protocol"
	"safeagent/internal/workspacebundle"
)

// ErrConflict 表示宿主文件与导入基线不一致，不能批准覆盖。
var ErrConflict = errors.New("宿主文件冲突，未覆盖")

// ExportClient 提供停止后工作区的只读数据访问。
type ExportClient interface {
	// ListChanges 分页读取文件清单。
	ListChanges(context.Context, protocol.ListChangesParams) (protocol.ChangePage, error)
	// ReadChangeChunk 有界读取文件内容。
	ReadChangeChunk(context.Context, protocol.ReadChangeChunkParams) (protocol.ChangeChunk, error)
}

// FileVersion 保存冻结的普通文件内容。
type FileVersion struct {
	// Data 保存批准时实际写入的内容。
	Data []byte `json:"data"`
	// Executable 决定是否设置执行位。
	Executable bool `json:"executable"`
}

// Change 描述一个相对路径的前后版本，nil表示不存在。
type Change struct {
	// Path 仅允许规范相对路径。
	Path string `json:"path"`
	// Before 为导入时版本，新增文件为nil。
	Before *FileVersion `json:"before"`
	// After 为冻结版本，删除文件为nil。
	After *FileVersion `json:"after"`
}

// Review 持有Host冻结的审核内容，调用方只能通过副本查看。
type Review struct {
	// mu 串行化重复批准。
	mu sync.Mutex
	// store 是持久保存审核记录的所属存储。
	store *Store
	// record 是已冻结且不再修改的审核内容。
	record reviewRecord
}

// reviewRecord 保存可在Host重启后恢复的冻结审核。
type reviewRecord struct {
	// ID 绑定全部冻结内容。
	ID string `json:"id"`
	// BaselineID 固定最初导入的基线。
	BaselineID BaselineID `json:"baselineID"`
	// Changes 保存有序变更。
	Changes []Change `json:"changes"`
}

// ID 返回需由用户明确批准的内容摘要。
func (r *Review) ID() string { return r.record.ID }

// Changes 返回不会修改冻结内容的独立副本。
func (r *Review) Changes() []Change {
	data, _ := json.Marshal(r.record.Changes)
	var result []Change
	json.Unmarshal(data, &result)
	return result
}

// Diff 返回逐文件前后文本，二进制内容只显示大小和摘要。
func (r *Review) Diff() string {
	var out strings.Builder
	for _, c := range r.record.Changes {
		fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", c.Path, c.Path)
		for i, v := range []*FileVersion{c.Before, c.After} {
			if v == nil {
				continue
			}
			prefix := "-"
			if i == 1 {
				prefix = "+"
			}
			fmt.Fprintf(&out, "%s mode executable=%t bytes=%d\n", prefix, v.Executable, len(v.Data))
			if !utf8.Valid(v.Data) || bytes.IndexByte(v.Data, 0) >= 0 {
				fmt.Fprintf(&out, "%s binary sha256:%x\n", prefix, sha256.Sum256(v.Data))
				continue
			}
			for _, line := range strings.Split(string(v.Data), "\n") {
				fmt.Fprintf(&out, "%s%s\n", prefix, strconv.Quote(line))
			}
		}
	}
	return out.String()
}

// CollectReview 校验Guest清单、大小与哈希，冻结全部变更并持久发布。
func (s *Store) CollectReview(ctx context.Context, id BaselineID, client ExportClient) (*Review, error) {
	_, _, archive, err := s.OpenImportForGuest(ctx, id, s.limits)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	before := map[string]*FileVersion{}
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(h.Name, "files/") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		before[strings.TrimPrefix(h.Name, "files/")] = &FileVersion{Data: data, Executable: h.Mode&0111 != 0}
	}
	entries := []Entry{}
	cursor := 0
	for {
		page, err := client.ListChanges(ctx, protocol.ListChangesParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if err = protocol.Validate(page); err != nil {
			return nil, err
		}
		entries = append(entries, page.Entries...)
		if len(entries) > s.limits.MaxFiles {
			return nil, ErrLimitExceeded
		}
		if page.Next == 0 {
			break
		}
		if page.Next != len(entries) || page.Next <= cursor {
			return nil, ErrInvalidSelection
		}
		cursor = page.Next
	}
	manifest := Manifest{Version: 1, Entries: entries}
	if err := workspacebundle.ValidateManifest(manifest, s.limits); err != nil {
		return nil, err
	}
	after := map[string]*FileVersion{}
	for _, e := range entries {
		data := make([]byte, 0, e.Size)
		for int64(len(data)) < e.Size {
			chunk, err := client.ReadChangeChunk(ctx, protocol.ReadChangeChunkParams{Path: e.Path, Offset: int64(len(data))})
			if err != nil {
				return nil, err
			}
			if len(chunk.Data) == 0 || len(chunk.Data) > protocol.ExportChunkBytes || int64(len(data)+len(chunk.Data)) > e.Size {
				return nil, ErrCorruptBaseline
			}
			data = append(data, chunk.Data...)
		}
		if fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != e.Digest {
			return nil, ErrCorruptBaseline
		}
		after[e.Path] = &FileVersion{Data: data, Executable: e.Executable}
	}
	generated, err := s.GeneratedPaths(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, p := range generated {
		delete(before, p)
		delete(after, p)
	}
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	record := reviewRecord{BaselineID: id, Changes: []Change{}}
	for p := range paths {
		if equalVersion(before[p], after[p]) {
			continue
		}
		record.Changes = append(record.Changes, Change{Path: p, Before: before[p], After: after[p]})
	}
	sort.Slice(record.Changes, func(i, j int) bool { return record.Changes[i].Path < record.Changes[j].Path })
	if err = validateReviewChanges(record.Changes, s.limits); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(record)
	record.ID = fmt.Sprintf("r_%x", sha256.Sum256(data))
	data, _ = json.Marshal(record)
	dir := filepath.Join(s.dataDir, "reviews")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	temp := filepath.Join(dir, newID(".review-"))
	if err = writeSynced(temp, data); err != nil {
		return nil, err
	}
	defer os.Remove(temp)
	if err = os.Rename(temp, filepath.Join(dir, record.ID+".json")); err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		return nil, err
	}
	return &Review{store: s, record: record}, nil
}

// validateReviewChanges 使用相同规则校验审核路径；文件与目录结构替换被明确拒绝。
func validateReviewChanges(changes []Change, limits Limits) error {
	before := make([]string, 0, len(changes))
	after := make([]string, 0, len(changes))
	union := make([]string, 0, len(changes)*2)
	for _, change := range changes {
		if change.Before != nil {
			before = append(before, change.Path)
		}
		if change.After != nil {
			after = append(after, change.Path)
		}
		union = append(union, change.Path)
	}
	if _, err := workspacebundle.ValidateSelection(before, limits); err != nil {
		return errors.Join(ErrCorruptBaseline, err)
	}
	if _, err := workspacebundle.ValidateSelection(after, limits); err != nil {
		return errors.Join(ErrCorruptBaseline, err)
	}
	expanded := limits
	expanded.MaxFiles *= 2
	if _, err := workspacebundle.ValidateSelection(union, expanded); err != nil {
		return errors.Join(ErrCorruptBaseline, err)
	}
	return nil
}

// equalVersion 比较内容与执行位，nil只与nil相等。
func equalVersion(a, b *FileVersion) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Executable == b.Executable && bytes.Equal(a.Data, b.Data)
}

// LoadReview 恢复持久化记录并重新校验内容绑定及所有路径。
func (s *Store) LoadReview(ctx context.Context, id string) (*Review, error) {
	if len(id) != 66 || !strings.HasPrefix(id, "r_") {
		return nil, ErrInvalidSelection
	}
	for _, c := range id[2:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, ErrInvalidSelection
		}
	}
	data, err := readBoundedFile(ctx, filepath.Join(s.dataDir, "reviews", id+".json"), s.limits.MaxTotalBytes*4+(8<<20))
	if err != nil {
		return nil, err
	}
	var record reviewRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	record.ID = ""
	canonical, _ := json.Marshal(record)
	if fmt.Sprintf("r_%x", sha256.Sum256(canonical)) != id {
		return nil, ErrCorruptBaseline
	}
	record.ID = id
	if _, err = s.Lookup(ctx, record.BaselineID); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(record.Changes))
	var totals [2]int64
	previous := ""
	for _, c := range record.Changes {
		if c.Path <= previous || equalVersion(c.Before, c.After) {
			return nil, ErrCorruptBaseline
		}
		previous = c.Path
		paths = append(paths, c.Path)
		for i, v := range []*FileVersion{c.Before, c.After} {
			if v == nil {
				continue
			}
			size := int64(len(v.Data))
			totals[i] += size
			if size > s.limits.MaxFileBytes || totals[i] > s.limits.MaxTotalBytes {
				return nil, ErrLimitExceeded
			}
		}
	}
	if err = validateReviewChanges(record.Changes, s.limits); err != nil {
		return nil, err
	}
	return &Review{store: s, record: record}, nil
}
