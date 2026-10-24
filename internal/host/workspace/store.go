package workspace

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"safeagent/internal/workspacebundle"
	"strconv"
	"strings"
)

// maxHostMetadataBytes 为仅 Host 保存的元数据设置独立于归档的上限。
const maxHostMetadataBytes int64 = 1 << 20

// Store 在私有目录中保存基线，无长期打开句柄。
type Store struct {
	// dataDir 指定 Host 独占管理的持久目录。
	dataDir string
	// limits 固定本 Store 的资源边界。
	limits Limits
}

// hostRecord 保存不进入 Guest 导入包的 Host 元数据。
type hostRecord struct {
	// Version 固定持久化结构版本。
	Version int `json:"version"`
	// Baseline 保存对外基线描述。
	Baseline Baseline `json:"baseline"`
	// Root 保存 SourceRef 对应的 Host 授权目录。
	Root string `json:"root"`
}

// NewStore 创建或打开私有基线存储，限额由调用方显式提供。
func NewStore(dataDir string, limits Limits) (*Store, error) {
	if !filepath.IsAbs(dataDir) || limits.MaxFiles <= 0 || limits.MaxFileBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxPathBytes <= 0 || limits.MaxArchiveBytes <= 0 {
		return nil, ErrInvalidSelection
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, safeError("创建存储目录", "", err)
	}
	info, err := os.Lstat(dataDir)
	if err != nil {
		return nil, safeError("检查存储目录", "", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%w: 存储目录必须为私有目录", ErrInvalidSelection)
	}
	for _, name := range []string{"staging", "baselines"} {
		if err := os.MkdirAll(filepath.Join(dataDir, name), 0700); err != nil {
			return nil, safeError("创建存储子目录", name, err)
		}
	}
	return &Store{dataDir: dataDir, limits: limits}, nil
}

// newID 生成不携带路径信息的随机资源标识。
func newID(prefix string) string {
	var value [16]byte
	rand.Read(value[:])
	return prefix + hex.EncodeToString(value[:])
}

// Stage 将已确认的文件选择复制为不可变基线后原子发布。
func (s *Store) Stage(ctx context.Context, selection ImportSelection) (baseline Baseline, err error) {
	paths, err := workspacebundle.ValidateSelection(selection.Paths, s.limits)
	if err != nil {
		return baseline, err
	}
	if err := ctx.Err(); err != nil {
		return baseline, err
	}
	root, err := openSourceRoot(selection.Root)
	if err != nil {
		return baseline, err
	}
	defer root.Close()
	id := newID("b_")
	sourceRef := newID("s_")
	temp := filepath.Join(s.dataDir, "staging", id)
	if err := os.Mkdir(temp, 0700); err != nil {
		return baseline, safeError("创建快照", "", err)
	}
	// 只清理本调用拥有的未发布目录；已发布资源由后续显式生命周期管理。
	defer func() {
		if cleanupErr := os.RemoveAll(temp); cleanupErr != nil {
			err = errors.Join(err, safeError("清理快照临时目录", "", cleanupErr))
		}
	}()
	m := Manifest{Version: 1, Entries: make([]Entry, 0, len(paths))}
	var total int64
	for i, p := range paths {
		entry, copyErr := s.copySource(ctx, root, p, filepath.Join(temp, strconv.Itoa(i)), s.limits.MaxTotalBytes-total)
		if copyErr != nil {
			return Baseline{}, copyErr
		}
		m.Entries = append(m.Entries, entry)
		total += entry.Size
	}
	manifest, _ := json.Marshal(m)
	archiveBytes, err := s.writeBundle(ctx, temp, m, manifest)
	if err != nil {
		return baseline, err
	}
	baseline = Baseline{ID: BaselineID(id), SourceRef: sourceRef, Digest: digestBytes(manifest), FileCount: len(m.Entries), TotalBytes: total, ArchiveBytes: archiveBytes}
	record, _ := json.Marshal(hostRecord{Version: 1, Baseline: baseline, Root: selection.Root})
	if int64(len(record)) > maxHostMetadataBytes {
		return Baseline{}, ErrLimitExceeded
	}
	for name, data := range map[string][]byte{"manifest.json": manifest, "host.json": record} {
		if err := writeSynced(filepath.Join(temp, name), data); err != nil {
			return Baseline{}, err
		}
	}
	// 原始复制文件只用于生成包，发布目录仅保留包和元数据。
	for i := range paths {
		if err := os.Remove(filepath.Join(temp, strconv.Itoa(i))); err != nil {
			return Baseline{}, safeError("移除复制临时文件", "", err)
		}
	}
	if err := syncDir(temp); err != nil {
		return Baseline{}, err
	}
	if err := ctx.Err(); err != nil {
		return Baseline{}, err
	}
	if err := os.Rename(temp, filepath.Join(s.dataDir, "baselines", id)); err != nil {
		return Baseline{}, safeError("发布基线", "", err)
	}
	// rename 为提交点，之后不因取消回滚已经发布的基线。
	if err := syncDir(filepath.Join(s.dataDir, "baselines")); err != nil {
		return baseline, err
	}
	return baseline, nil
}

// copySource 从已固定的源句柄复制文件，同时验证容量和常见并发修改。
func (s *Store) copySource(ctx context.Context, root *os.File, relative, target string, remaining int64) (Entry, error) {
	var entry Entry
	source, before, err := openSourceFile(root, relative)
	if err != nil {
		return entry, err
	}
	defer source.Close()
	limit := min(s.limits.MaxFileBytes, remaining, int64(8589934591))
	if before.Size() > limit {
		return entry, ErrLimitExceeded
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return entry, safeError("创建复制文件", "", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReader{ctx: ctx, reader: source}, limit+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		return entry, safeError("复制源文件", relative, errors.Join(copyErr, closeErr))
	}
	if n > limit {
		return entry, ErrLimitExceeded
	}
	after, err := source.Stat()
	if err != nil {
		return entry, safeError("复查源文件", relative, err)
	}
	if n != before.Size() || sourceChanged(before, after) {
		return entry, ErrSourceChanged
	}
	return Entry{Path: relative, Size: n, Executable: before.Mode().Perm()&0111 != 0, Digest: hashDigest(h)}, nil
}

// cappedWriter 在归档写入前检查剩余容量。
type cappedWriter struct {
	// writer 指向实际输出文件。
	writer io.Writer
	// remaining 记录还允许写入的字节数。
	remaining int64
}

// Write 拒绝超限数据，避免先写入再检测。
func (w *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrLimitExceeded
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// writeBundle 将临时复制文件编码为规范归档并同步到磁盘。
func (s *Store) writeBundle(ctx context.Context, temp string, m Manifest, manifest []byte) (int64, error) {
	f, err := os.OpenFile(filepath.Join(temp, "bundle.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, safeError("创建导入包", "", err)
	}
	defer f.Close()
	capped := &cappedWriter{writer: f, remaining: s.limits.MaxArchiveBytes}
	w := tar.NewWriter(capped)
	if err := w.WriteHeader(workspacebundle.Header("manifest.json", int64(len(manifest)), false)); err != nil {
		return 0, safeError("写入包清单头", "", err)
	}
	if _, err := w.Write(manifest); err != nil {
		return 0, safeError("写入包清单", "", err)
	}
	for i, entry := range m.Entries {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := w.WriteHeader(workspacebundle.Header("files/"+entry.Path, entry.Size, entry.Executable)); err != nil {
			return 0, safeError("写入文件头", entry.Path, err)
		}
		input, err := os.Open(filepath.Join(temp, strconv.Itoa(i)))
		if err != nil {
			return 0, safeError("打开复制文件", "", err)
		}
		_, copyErr := io.Copy(w, contextReader{ctx: ctx, reader: input})
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return 0, safeError("编码文件", entry.Path, errors.Join(copyErr, closeErr))
		}
	}
	if err := w.Close(); err != nil {
		return 0, safeError("完成导入包", "", err)
	}
	if err := f.Sync(); err != nil {
		return 0, safeError("同步导入包", "", err)
	}
	if err := f.Close(); err != nil {
		return 0, safeError("关闭导入包", "", err)
	}
	return s.limits.MaxArchiveBytes - capped.remaining, nil
}

// writeSynced 将小型元数据以私有权限写入并同步。
func writeSynced(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return safeError("创建基线元数据", "", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return safeError("写入基线元数据", "", err)
	}
	if err := f.Sync(); err != nil {
		return safeError("同步基线元数据", "", err)
	}
	return safeClose(f)
}

// safeClose 关闭写入文件并去除文件错误中的绝对路径。
func safeClose(f *os.File) error {
	if err := f.Close(); err != nil {
		return safeError("关闭基线文件", "", err)
	}
	return nil
}

// syncDir 同步发布目录项，不把目录同步失败报告为成功。
func syncDir(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return safeError("打开同步目录", "", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return safeError("同步目录", "", err)
	}
	return nil
}

// validBaselineID 校验外部资源标识，禁止被解释为目录路径。
func validBaselineID(id BaselineID) bool {
	text := string(id)
	if len(text) != 34 || !strings.HasPrefix(text, "b_") || strings.ToLower(text) != text {
		return false
	}
	_, err := hex.DecodeString(text[2:])
	return err == nil
}

// readBoundedFile 有界读取 Host 私有元数据。
func readBoundedFile(ctx context.Context, name string, limit int64) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, safeError("读取基线元数据", "", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, safeError("检查基线元数据", "", err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrCorruptBaseline
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, limit))
	if err != nil {
		return nil, safeError("读取基线元数据", "", err)
	}
	return data, nil
}

// Lookup 恢复已发布基线并校验清单与描述的一致性。
func (s *Store) Lookup(ctx context.Context, id BaselineID) (Baseline, error) {
	var zero Baseline
	if !validBaselineID(id) {
		return zero, ErrInvalidSelection
	}
	dir := filepath.Join(s.dataDir, "baselines", string(id))
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return zero, ErrNotFound
		}
		return zero, safeError("查找基线", "", err)
	}
	record, err := readBoundedFile(ctx, filepath.Join(dir, "host.json"), maxHostMetadataBytes)
	if err != nil {
		return zero, err
	}
	var host hostRecord
	if err := json.Unmarshal(record, &host); err != nil || host.Version != 1 || host.Baseline.ID != id || !filepath.IsAbs(host.Root) {
		return zero, ErrCorruptBaseline
	}
	data, err := readBoundedFile(ctx, filepath.Join(dir, "manifest.json"), s.limits.MaxArchiveBytes)
	if err != nil {
		return zero, err
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil || digestBytes(data) != host.Baseline.Digest {
		return zero, ErrCorruptBaseline
	}
	canonical, _ := json.Marshal(m)
	if string(canonical) != string(data) {
		return zero, ErrCorruptBaseline
	}
	if err := workspacebundle.ValidateManifest(m, s.limits); err != nil {
		return zero, errors.Join(ErrCorruptBaseline, err)
	}
	var total int64
	for _, entry := range m.Entries {
		total += entry.Size
	}
	if host.Baseline.FileCount != len(m.Entries) || host.Baseline.TotalBytes != total || host.Baseline.ArchiveBytes < 0 || host.Baseline.ArchiveBytes > s.limits.MaxArchiveBytes {
		return zero, ErrCorruptBaseline
	}
	return host.Baseline, nil
}

// importReader 持有一个独立归档句柄和本次读取上下文。
type importReader struct {
	// ctx 限制本次导入读取的生命周期。
	ctx context.Context
	// file 在 Close 时释放独立句柄。
	file *os.File
}

// Read 读取归档并去除底层文件错误中的存储绝对路径。
func (r *importReader) Read(p []byte) (int, error) {
	n, err := (contextReader{ctx: r.ctx, reader: r.file}).Read(p)
	if err != nil && err != io.EOF {
		return n, safeError("读取导入包", "", err)
	}
	return n, err
}

// Close 释放本次导入句柄，不影响其他读者。
func (r *importReader) Close() error {
	return safeClose(r.file)
}

// OpenImport 校验已发布包后返回从头读取的独立句柄。
func (s *Store) OpenImport(ctx context.Context, id BaselineID) (io.ReadCloser, error) {
	_, _, reader, err := s.OpenImportForGuest(ctx, id, s.limits)
	return reader, err
}

// OpenImportForGuest 在发送前以存储与Guest限额的交集校验一次完整归档。
func (s *Store) OpenImportForGuest(ctx context.Context, id BaselineID, guest Limits) (Baseline, Manifest, io.ReadCloser, error) {
	limits := workspacebundle.Intersect(s.limits, guest)
	if limits.MaxFiles <= 0 || limits.MaxFileBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxPathBytes <= 0 || limits.MaxArchiveBytes <= 0 {
		return Baseline{}, Manifest{}, nil, ErrInvalidSelection
	}
	baseline, err := s.Lookup(ctx, id)
	if err != nil {
		return Baseline{}, Manifest{}, nil, err
	}
	f, err := os.Open(filepath.Join(s.dataDir, "baselines", string(id), "bundle.tar"))
	if err != nil {
		return Baseline{}, Manifest{}, nil, safeError("打开导入包", "", err)
	}
	if baseline.ArchiveBytes > limits.MaxArchiveBytes || baseline.FileCount > limits.MaxFiles || baseline.TotalBytes > limits.MaxTotalBytes {
		f.Close()
		return Baseline{}, Manifest{}, nil, ErrLimitExceeded
	}
	m, err := ValidateBundle(ctx, f, limits)
	if err != nil {
		f.Close()
		return Baseline{}, Manifest{}, nil, classifyBundleError(err)
	}
	data, _ := json.Marshal(m)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return Baseline{}, Manifest{}, nil, safeError("检查导入包", "", err)
	}
	if digestBytes(data) != baseline.Digest || info.Size() != baseline.ArchiveBytes {
		f.Close()
		return Baseline{}, Manifest{}, nil, ErrCorruptBaseline
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return Baseline{}, Manifest{}, nil, safeError("定位导入包", "", err)
	}
	return baseline, m, &importReader{ctx: ctx, file: f}, nil
}

// classifyBundleError 区分格式及截断错误与实际存储读取故障。
func classifyBundleError(err error) error {
	clean := safeError("校验导入包", "", err)
	if errors.Is(err, ErrCorruptBaseline) || errors.Is(err, ErrInvalidSelection) || errors.Is(err, ErrLimitExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.Join(ErrCorruptBaseline, clean)
	}
	return clean
}
