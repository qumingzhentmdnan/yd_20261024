package protocol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Request 携带强类型身份与待按方法解码的参数。
type Request struct {
	// RequestID 在连接内严格递增，不得复用。
	RequestID Sequence `json:"requestID"`
	// Epoch 原样使用现有 VMInstance 的启动代次。
	Epoch Epoch `json:"epoch"`
	// ConnectionID 由 Host 为本连接生成。
	ConnectionID UUID `json:"connectionID"`
	// Method 决定参数和成功结果类型，未知方法由 Handler 返回 UNIMPLEMENTED。
	Method Method `json:"method"`
	// TimeoutMs 从完整接收请求后开始计时，单位毫秒。
	TimeoutMs Millis `json:"timeoutMs"`
	// Params 必须是单个 JSON 对象。
	Params json.RawMessage `json:"params"`
}

// Response 只允许成功 result 与业务 error 二选一。
type Response struct {
	// RequestID 必须关联原请求。
	RequestID Sequence `json:"requestID"`
	// Epoch 必须与原请求完全一致。
	Epoch Epoch `json:"epoch"`
	// ConnectionID 必须与原请求完全一致。
	ConnectionID UUID `json:"connectionID"`
	// Result 在成功时必填，由原方法解码，失败时省略。
	Result json.RawMessage `json:"result,omitempty"`
	// Error 在失败时必填，成功时省略。
	Error *Error `json:"error,omitempty"`
}

// Cancel 只取消同连接的请求等待，不回滚已提交副作用。
type Cancel struct {
	// RequestID 指向本连接原请求。
	RequestID Sequence `json:"requestID"`
	// Epoch 限定原 VM 启动。
	Epoch Epoch `json:"epoch"`
	// ConnectionID 限定原控制连接。
	ConnectionID UUID `json:"connectionID"`
}

// SessionExitedEvent 通知固定退出结果，不替代 GetState。
type SessionExitedEvent struct {
	// Epoch 必须匹配本连接。
	Epoch Epoch `json:"epoch"`
	// ConnectionID 必须匹配本连接。
	ConnectionID UUID `json:"connectionID"`
	// EventID 是连接内单调递增序号。
	EventID Sequence `json:"eventID"`
	// Event 固定为 SessionExited。
	Event string `json:"event"`
	// Params 来自真正的主进程等待结果。
	Params SessionExit `json:"params"`
}

// EncodeRequest 同时检查请求 envelope 和方法参数，避免发送无效原始参数。
func EncodeRequest(request Request) ([]byte, error) {
	data, err := Encode(request)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeRequest(data); err != nil {
		return nil, err
	}
	return data, nil
}

// EncodeResponse 按原请求检查关联身份与结果类型，业务错误无需成功结果。
func EncodeResponse(response Response, request Request) ([]byte, error) {
	data, err := Encode(response)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeResponse(data, request); err != nil {
		return nil, err
	}
	return data, nil
}

// DecodeRequest 严格解码 envelope；已知方法还检查独立参数，未知方法保留给 Handler。
func DecodeRequest(data []byte) (Request, error) {
	r, err := DecodeRequestEnvelope(data)
	if err != nil {
		return r, err
	}
	if _, err := r.DecodeParams(); err != nil {
		if e, ok := err.(*Error); !ok || e.Code != CodeUnimplemented {
			return r, err
		}
	}
	return r, nil
}

// DecodeRequestEnvelope 只验证公共 envelope，供 Handler 先检查握手和身份，再验证方法参数。
func DecodeRequestEnvelope(data []byte) (Request, error) {
	var r Request
	if err := Decode(data, &r); err != nil {
		return r, err
	}
	if len(bytes.TrimSpace(r.Params)) == 0 || bytes.TrimSpace(r.Params)[0] != '{' {
		return r, Failure(CodeInvalidArgument)
	}
	return r, nil
}

// DecodeParams 根据方法返回精确参数类型，不能用共用大结构体接收其他方法字段。
func (r Request) DecodeParams() (any, error) {
	var target any
	switch r.Method {
	case MethodHello:
		target = &HelloParams{}
	case MethodGetState:
		target = &Empty{}
	case MethodPrepareAgent:
		target = &PrepareAgentParams{}
	case MethodStartSession:
		target = &StartSessionParams{}
	case MethodAttachTerminal:
		target = &AttachTerminalParams{}
	case MethodResizeTerminal:
		target = &ResizeTerminalParams{}
	case MethodDetachTerminal:
		target = &DetachTerminalParams{}
	case MethodStopSession:
		target = &StopSessionParams{}
	default:
		return nil, Failure(CodeUnimplemented)
	}
	if err := Decode(r.Params, target); err != nil {
		return nil, err
	}
	return target, nil
}

// DecodeResponse 核对关联身份和互斥分支，并按原请求方法检查成功结果。
func DecodeResponse(data []byte, request Request) (Response, error) {
	var r Response
	if err := Decode(data, &r); err != nil {
		return r, err
	}
	if r.RequestID != request.RequestID || r.Epoch != request.Epoch || r.ConnectionID != request.ConnectionID {
		return r, ErrFrame
	}
	if (len(r.Result) > 0) == (r.Error != nil) {
		return r, ErrFrame
	}
	if r.Error != nil {
		return r, nil
	}
	target, err := resultTarget(request.Method)
	if err != nil {
		return r, err
	}
	if err := Decode(r.Result, target); err != nil {
		return r, err
	}
	return r, nil
}

// resultTarget 为方法创建唯一成功结果类型，业务错误不进入该路径。
func resultTarget(method Method) (any, error) {
	switch method {
	case MethodHello:
		return &HelloResult{}, nil
	case MethodGetState:
		return &GuestState{}, nil
	case MethodPrepareAgent:
		return &Preparation{}, nil
	case MethodStartSession:
		return &SessionInfo{}, nil
	case MethodAttachTerminal:
		return &AttachTerminalResult{}, nil
	case MethodResizeTerminal, MethodDetachTerminal, MethodStopSession:
		return &Empty{}, nil
	}
	return nil, Failure(CodeUnimplemented)
}

// Fingerprint 返回去重参数规范 JSON 与 SHA-256，不包含连接和超时字段。
func Fingerprint(method Method, params any) (string, string, error) {
	if err := Validate(params); err != nil {
		return "", "", err
	}
	var tuple []any
	switch p := params.(type) {
	case PrepareAgentParams:
		if method != MethodPrepareAgent {
			return "", "", Failure(CodeInvalidArgument)
		}
		tuple = []any{method, p.ManifestRef, p.ManifestDigest, p.ConfigDigest, p.WorkspaceRef, p.WorkspaceDigest}
	case StartSessionParams:
		if method != MethodStartSession {
			return "", "", Failure(CodeInvalidArgument)
		}
		tuple = []any{method, p.SessionID, p.PreparedID, p.WorkspaceRef, p.WorkspaceDigest, p.Rows, p.Cols}
	default:
		return "", "", Failure(CodeInvalidArgument)
	}
	data, err := json.Marshal(tuple)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return string(data), hex.EncodeToString(sum[:]), nil
}

// NewUUID 生成规范 UUID v4，随机源失败时明确返回错误。
func NewUUID() (UUID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return UUID(fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])), nil
}

// NewNonce 生成一次握手或绑定使用的 32 字节随机令牌。
func NewNonce() (Nonce, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return Nonce(hex.EncodeToString(b[:])), nil
}
