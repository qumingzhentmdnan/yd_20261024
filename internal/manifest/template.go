package manifest

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// checkUnicode 拒绝 JSON 解码器会有损替换的字节和孤立 UTF-16 转义。
func checkUnicode(data []byte) error {
	if !utf8.Valid(data) {
		return ErrManifest
	}
	inside := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || data[i] != '\\' {
			continue
		}
		if i+1 >= len(data) {
			return ErrManifest
		}
		if data[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(data) {
			return ErrManifest
		}
		value, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		if err != nil || value >= 0xdc00 && value <= 0xdfff {
			return ErrManifest
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+12 > len(data) || string(data[i+6:i+8]) != `\u` {
				return ErrManifest
			}
			low, err := strconv.ParseUint(string(data[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return ErrManifest
			}
			i += 6
		}
		i += 5
	}
	return nil
}

// decodeTemplate 将严格 JSON 数据解析成不含可执行模板的公共配置子集。
func decodeTemplate(data []byte) (map[string]any, error) {
	if len(data) > maxSourceBytes {
		return nil, ErrLimit
	}
	if err := checkUnicode(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	nodes := 0
	value, err := templateValue(decoder, 0, &nodes)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrManifest
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, ErrManifest
	}
	return root, nil
}

// templateValue 限制嵌套和节点，按解码后的键判重，数组仅接受同类标量。
func templateValue(decoder *json.Decoder, depth int, count *int) (any, error) {
	*count += 1
	if depth > maxDepth || *count > maxNodes {
		return nil, ErrLimit
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, ErrManifest
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return nil, ErrManifest
				}
				name := key.(string)
				if len(name) > maxStringBytes {
					return nil, ErrLimit
				}
				if _, exists := object[name]; exists || strings.Contains(name, "$") {
					return nil, ErrManifest
				}
				child, err := templateValue(decoder, depth+1, count)
				if err != nil {
					return nil, err
				}
				object[name] = child
			}
			if _, err := decoder.Token(); err != nil {
				return nil, ErrManifest
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				child, err := templateValue(decoder, depth+1, count)
				if err != nil {
					return nil, err
				}
				switch child.(type) {
				case string, bool, int64:
				default:
					return nil, ErrManifest
				}
				if len(array) > 0 && reflect.TypeOf(array[0]) != reflect.TypeOf(child) {
					return nil, ErrManifest
				}
				array = append(array, child)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, ErrManifest
			}
			return array, nil
		default:
			return nil, ErrManifest
		}
	case string:
		if len(value) > maxStringBytes {
			return nil, ErrLimit
		}
		if err := checkVariables(value); err != nil {
			return nil, err
		}
		return value, nil
	case bool:
		return value, nil
	case json.Number:
		integer, err := value.Int64()
		if err != nil {
			return nil, ErrManifest
		}
		return integer, nil
	default:
		return nil, ErrManifest
	}
}
