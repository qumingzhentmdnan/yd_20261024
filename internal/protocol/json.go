package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MarshalJSON 将正数请求序号编码为十进制字符串。
func (s Sequence) MarshalJSON() ([]byte, error) {
	if s == 0 {
		return nil, Failure(CodeInvalidArgument)
	}
	return json.Marshal(strconv.FormatUint(uint64(s), 10))
}

// UnmarshalJSON 拒绝前导零、溢出和零请求序号。
func (s *Sequence) UnmarshalJSON(data []byte) error {
	n, err := parseDecimal(data, false)
	if err == nil {
		*s = Sequence(n)
	}
	return err
}

// MarshalJSON 将允许零的累计计数编码为十进制字符串。
func (c Counter) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(c), 10))
}

// UnmarshalJSON 拒绝累计计数的前导零和溢出。
func (c *Counter) UnmarshalJSON(data []byte) error {
	n, err := parseDecimal(data, true)
	if err == nil {
		*c = Counter(n)
	}
	return err
}

// parseDecimal 校验 JSON 字符串与 uint64 的规范文本。
func parseDecimal(data []byte, zero bool) (uint64, error) {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value || (!zero && n == 0) {
		return 0, Failure(CodeInvalidArgument)
	}
	return n, nil
}

// Decode 严格解码共享 DTO，检查重复键、未知字段、必填字段及值约束。
func Decode(data []byte, target any) error {
	if !utf8.Valid(data) {
		return Failure(CodeInvalidArgument)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := checkJSON(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return Failure(CodeInvalidArgument)
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := required(data, reflect.ValueOf(target).Elem()); err != nil {
		return err
	}
	return Validate(target)
}

// Encode 校验共享 DTO 的值与 JSON 形状；请求和响应应使用各自的专用编码函数。
func Encode(value any) ([]byte, error) {
	if err := Validate(value); err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	target := reflect.New(reflect.TypeOf(value))
	if err := Decode(data, target.Interface()); err != nil {
		return nil, err
	}
	return data, nil
}

// checkJSON 遍历对象与数组，拒绝任何层级的重复键。
func checkJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return Failure(CodeInvalidArgument)
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name := key.(string)
			if keys[name] {
				return Failure(CodeInvalidArgument)
			}
			keys[name] = true
		}
		if err := checkJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// required 按 DTO 字段检查必填与可空语义，防止 JSON 零值掩盖缺失布尔字段。
func required(data []byte, v reflect.Value) error {
	if v.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		return required(data, v.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return Failure(CodeInvalidArgument)
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		allowed := make(map[string]bool, v.NumField())
		for i := 0; i < v.NumField(); i++ {
			allowed[strings.Split(v.Type().Field(i).Tag.Get("json"), ",")[0]] = true
		}
		for name := range fields {
			if !allowed[name] {
				return fmt.Errorf("%w: unknown %s", Failure(CodeInvalidArgument), name)
			}
		}
		for i := 0; i < v.NumField(); i++ {
			tag := strings.Split(v.Type().Field(i).Tag.Get("json"), ",")
			raw, ok := fields[tag[0]]
			optional := len(tag) > 1 && tag[1] == "omitempty"
			if !ok {
				if optional {
					continue
				}
				return fmt.Errorf("%w: missing %s", Failure(CodeInvalidArgument), tag[0])
			}
			if optional && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return fmt.Errorf("%w: null %s", Failure(CodeInvalidArgument), tag[0])
			}
			if err := required(raw, v.Field(i)); err != nil {
				return fmt.Errorf("%s: %w", tag[0], err)
			}
		}
	case reflect.Slice:
		// 字节切片采用base64字符串，Decode已验证编码；null检查仍在上方生效。
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '"' {
				return Failure(CodeInvalidArgument)
			}
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		for i, item := range items {
			if err := required(item, v.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}
