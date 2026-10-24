package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"
)

// contextReader 在每次底层读取前检查本次调用是否已取消。
type contextReader struct {
	// ctx 限制当前读取生命周期。
	ctx context.Context
	// reader 提供实际字节流。
	reader io.Reader
}

// Read 在取消后停止继续读取。
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// digestBytes 计算带算法前缀的小写摘要。
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hashDigest 将流式哈希转换为公共摘要格式。
func hashDigest(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// bundleHeader 构造不包含宿主身份及时间信息的确定性头部。
func bundleHeader(name string, size int64, executable bool) *tar.Header {
	mode := int64(0644)
	if executable {
		mode = 0755
	}
	return &tar.Header{Name: name, Size: size, Mode: mode, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
}

// headerBytes 使用同一编码器生成期望头部，包含必要的规范 path 扩展。
func headerBytes(header *tar.Header) ([]byte, error) {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(header); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// expectBytes 精确比较流中的规范元数据，拒绝非规范头、填充和扩展记录。
func expectBytes(r io.Reader, expected []byte) error {
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(r, actual); err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return ErrCorruptBaseline
	}
	return nil
}

// validateManifest 校验包清单的外部输入及资源上限。
func validateManifest(m Manifest, limits Limits) error {
	if m.Version != 1 || m.Entries == nil || len(m.Entries) > limits.MaxFiles {
		return ErrCorruptBaseline
	}
	paths := make([]string, len(m.Entries))
	var total int64
	for i, e := range m.Entries {
		if e.Size < 0 || e.Size > limits.MaxFileBytes || e.Size > 8589934591 || e.Size > limits.MaxTotalBytes-total {
			return ErrLimitExceeded
		}
		if !strings.HasPrefix(e.Digest, "sha256:") || len(e.Digest) != 71 {
			return ErrCorruptBaseline
		}
		if _, err := hex.DecodeString(e.Digest[7:]); err != nil || strings.ToLower(e.Digest) != e.Digest {
			return ErrCorruptBaseline
		}
		paths[i] = e.Path
		total += e.Size
	}
	sorted, err := validateSelection(paths, limits)
	if err != nil {
		return err
	}
	for i := range sorted {
		if sorted[i] != paths[i] {
			return ErrCorruptBaseline
		}
	}
	return nil
}

// manifestBudget 计算清单的最大编码大小，乘法前先按归档限额截断。
func manifestBudget(limits Limits) int64 {
	budget := limits.MaxArchiveBytes
	if budget <= 192 || int64(limits.MaxPathBytes) > (budget-192)/6 {
		return budget
	}
	perFile := int64(limits.MaxPathBytes)*6 + 160
	if int64(limits.MaxFiles) > (budget-32)/perFile {
		return budget
	}
	return int64(limits.MaxFiles)*perFile + 32
}

// ValidateBundle 流式校验完整规范归档，供 Host 与未来 Guest 导入复用。
func ValidateBundle(ctx context.Context, source io.Reader, limits Limits) (Manifest, error) {
	// 使用规范头逐字节比较，而非依赖 tar.Reader 合并后丢失的原始扩展信息。
	r := contextReader{ctx: ctx, reader: source}
	limited := &io.LimitedReader{R: r, N: limits.MaxArchiveBytes}
	var m Manifest
	first := make([]byte, 512)
	if _, err := io.ReadFull(limited, first); err != nil {
		return m, err
	}
	size, err := strconv.ParseInt(strings.Trim(string(first[124:136]), " \x00"), 8, 64)
	if err != nil || size < 0 || size > limits.MaxArchiveBytes-1536 || size > manifestBudget(limits) {
		return m, ErrCorruptBaseline
	}
	header, err := headerBytes(bundleHeader("manifest.json", size, false))
	if err != nil || !bytes.Equal(header, first) {
		return m, ErrCorruptBaseline
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(limited, data); err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, ErrCorruptBaseline
	}
	canonical, _ := json.Marshal(m)
	if !bytes.Equal(data, canonical) {
		return m, ErrCorruptBaseline
	}
	if err := validateManifest(m, limits); err != nil {
		return m, err
	}
	if err := expectBytes(limited, make([]byte, (512-size%512)%512)); err != nil {
		return m, err
	}
	for _, entry := range m.Entries {
		header, err := headerBytes(bundleHeader("files/"+entry.Path, entry.Size, entry.Executable))
		if err != nil {
			return m, err
		}
		if err := expectBytes(limited, header); err != nil {
			return m, err
		}
		h := sha256.New()
		if _, err := io.CopyN(h, limited, entry.Size); err != nil {
			return m, err
		}
		if hashDigest(h) != entry.Digest {
			return m, ErrCorruptBaseline
		}
		if err := expectBytes(limited, make([]byte, (512-entry.Size%512)%512)); err != nil {
			return m, err
		}
	}
	if err := expectBytes(limited, make([]byte, 1024)); err != nil {
		return m, err
	}
	var extra [1]byte
	if n, err := r.Read(extra[:]); n != 0 || err != io.EOF {
		if err != nil && err != io.EOF {
			return m, err
		}
		return m, fmt.Errorf("%w: 归档尾部有额外数据", ErrCorruptBaseline)
	}
	return m, nil
}
