package protocol

import (
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"unicode/utf8"
)

var (
	opaquePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	vmPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	uuidPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	noncePattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	manifestRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_@.\-]{0,127}$`)
	signalPattern      = regexp.MustCompile(`^(|SIG[A-Z0-9]+(\+[0-9]+)?)$`)
)

const (
	MaxOperations = 256
	MaxSessions   = 64
	MaxPending    = 16
	MaxRequests   = 8192
	OutputLimit   = 65536
)

// Error 返回不含内部 cause 的稳定业务错误。
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Failure 构造仅含稳定错误码的业务失败。
func Failure(code Code) *Error { return &Error{Code: code, Message: string(code)} }

// Validate 检查具名值类型、完整 DTO 范围及跨字段约束，不查询运行资源。
func Validate(value any) error { return validateValue(reflect.ValueOf(value)) }

// validateValue 递归校验共享 DTO，复用具名标量约束以避免各方法另起字段解释。
func validateValue(v reflect.Value) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return validateValue(v.Elem())
	}
	bad := false
	switch x := v.Interface().(type) {
	case OpaqueID:
		bad = !opaquePattern.MatchString(string(x))
	case VMRef:
		bad = !vmPattern.MatchString(string(x))
	case Epoch:
		bad = !utf8.ValidString(string(x)) || utf8.RuneCountInString(string(x)) < 1 || utf8.RuneCountInString(string(x)) > 128
	case UUID:
		bad = !uuidPattern.MatchString(string(x))
	case Nonce:
		bad = !noncePattern.MatchString(string(x))
	case Digest:
		bad = !digestPattern.MatchString(string(x))
	case Sequence:
		bad = x == 0
	case StreamID:
		bad = x == 0
	case Dimension:
		bad = x == 0
	case Millis:
		bad = x < 1 || x > 30000
	case Code:
		bad = !oneOf(string(x), "INVALID_ARGUMENT", "UNIMPLEMENTED", "FAILED_PRECONDITION", "STALE_INSTANCE", "CONFLICT", "NOT_FOUND", "RESOURCE_EXHAUSTED", "CANCELLED", "DEADLINE_EXCEEDED", "INTERNAL")
	case SessionState:
		bad = !oneOf(string(x), "running", "exited")
	case GroupState:
		bad = !oneOf(string(x), "running", "empty", "unknown")
	case AttachmentState:
		bad = !oneOf(string(x), "awaiting-bind", "bound", "detaching")
	case ExitReason:
		bad = !oneOf(string(x), "process-exited", "stopped", "forced")
	case *Error:
		return Validate(*x)
	case Error:
		bad = utf8.RuneCountInString(x.Message) > 256
	case ListChangesParams:
		bad = x.Cursor < 0 || x.Cursor > 4096
	case ReadChangeChunkParams:
		bad = x.Offset < 0 || x.Offset > 16<<20 || len(x.Path) > 1024 || !fs.ValidPath(x.Path)
	case ChangePage:
		bad = len(x.Entries) > 8 || x.Next < 0 || x.Next > 4096
	case ChangeChunk:
		bad = len(x.Data) > ExportChunkBytes
	case HelloParams:
		bad = len(x.SupportedVersions) != 1 || x.SupportedVersions[0] != "0.1"
	case HelloResult:
		bad = x.Version != "0.1"
	case Request:
		bad = utf8.RuneCountInString(string(x.Method)) < 1 || utf8.RuneCountInString(string(x.Method)) > 64
	case Response:
		bad = (len(x.Result) > 0) == (x.Error != nil)
	case SessionExitedEvent:
		bad = x.Event != "SessionExited"
	case SessionExit:
		bad = x.Code < -1 || x.Code > 255 || len(x.Signal) > 32 || !signalPattern.MatchString(x.Signal) || (x.Signal == "" && x.Code < 0) || (x.Signal != "" && x.Code != -1)
	case SessionInfo:
		bad = (x.State == SessionStateRunning && x.Exit != nil) || (x.State == SessionStateExited && x.Exit == nil) || (x.Exit != nil && x.Exit.SessionID != x.SessionID)
	case GuestState:
		if len(x.Sessions) > MaxSessions {
			bad = true
			break
		}
		found := x.CurrentSessionID == nil
		previous := OpaqueID("")
		ids := map[OpaqueID]bool{}
		for _, session := range x.Sessions {
			if session.SessionID <= previous || ids[session.SessionID] {
				bad = true
			}
			previous = session.SessionID
			ids[session.SessionID] = true
			if x.CurrentSessionID != nil && *x.CurrentSessionID == session.SessionID {
				found = true
			}
		}
		bad = bad || !found || (x.Attachment != nil && !ids[x.Attachment.SessionID])
	}
	if bad {
		return fmt.Errorf("%w: %s", Failure(CodeInvalidArgument), v.Type())
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			// ManifestRef 允许完整版本引用，其他不透明标识仍使用原有格式。
			if v.Type().Field(i).Name == "ManifestRef" {
				if !manifestRefPattern.MatchString(v.Field(i).String()) {
					return fmt.Errorf("%w: ManifestRef", Failure(CodeInvalidArgument))
				}
				continue
			}
			if err := validateValue(v.Field(i)); err != nil {
				return fmt.Errorf("%s: %w", v.Type().Field(i).Name, err)
			}
		}
	case reflect.Slice:
		// 原始JSON和文件块的字节没有逐元素业务约束，避免在Guest上反射扫描整帧。
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := validateValue(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return Failure(CodeInvalidArgument)
		}
	}
	return nil
}

// oneOf 判断值是否属于显式枚举集合。
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
