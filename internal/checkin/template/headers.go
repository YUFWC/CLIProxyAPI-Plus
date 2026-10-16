// Package template compiles account metadata references for check-in headers.
package template

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type segment struct {
	literal string
	path    []string
}

type header struct {
	name     string
	segments []segment
}

// Headers is immutable and may render independent account snapshots concurrently.
type Headers struct {
	headers []header
	paths   []string
}

// ProtectedHeaders lists system-owned authentication and transport header names.
func ProtectedHeaders(provider string) []string {
	names := []string{"Authorization", "Host", "Content-Length", "Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "TE", "Trailer", "Transfer-Encoding", "Upgrade"}
	switch provider {
	case "codearts":
		names = append(names, "X-Sdk-Date", "X-Security-Token", "X-Sdk-Content-Sha256")
	case "trae":
		names = append(names, "Cloud-IDE-JWT")
	}
	return names
}

// Compile validates names and template syntax without reading any account data.
func Compile(values map[string]string, provider string) (*Headers, error) {
	result := &Headers{}
	protected := make(map[string]bool)
	for _, name := range ProtectedHeaders(provider) {
		protected[strings.ToLower(name)] = true
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(values))
	paths := make(map[string]bool)
	for _, name := range names {
		if !validName(name) {
			return nil, fmt.Errorf("invalid HTTP header name %q", name)
		}
		canonical := strings.ToLower(name)
		if seen[canonical] {
			return nil, fmt.Errorf("duplicate HTTP header name %q", name)
		}
		seen[canonical] = true
		if protected[canonical] {
			return nil, fmt.Errorf("header %q is managed by the system", name)
		}
		value := values[name]
		if !validValue(value) {
			return nil, fmt.Errorf("invalid HTTP header value for %q", name)
		}
		segments, err := compileValue(value)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", name, err)
		}
		for _, part := range segments {
			if part.path != nil {
				paths[strings.Join(part.path, ".")] = true
			}
		}
		result.headers = append(result.headers, header{name: name, segments: segments})
	}
	for path := range paths {
		result.paths = append(result.paths, path)
	}
	sort.Strings(result.paths)
	return result, nil
}

// Paths returns the unique referenced metadata paths, without exposing mutable state.
func (h *Headers) Paths() []string {
	if h == nil {
		return nil
	}
	return append([]string(nil), h.paths...)
}

func compileValue(value string) ([]segment, error) {
	var parts []segment
	var literal strings.Builder
	flush := func() {
		if literal.Len() != 0 {
			parts = append(parts, segment{literal: literal.String()})
			literal.Reset()
		}
	}
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "$${") {
			literal.WriteString("${")
			i += 3
		} else if strings.HasPrefix(value[i:], "${") {
			flush()
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				return nil, fmt.Errorf("template_syntax: unclosed placeholder")
			}
			path := strings.Split(value[i+2:i+2+end], ".")
			for _, key := range path {
				if key == "" {
					return nil, fmt.Errorf("template_syntax: empty path segment")
				}
				for j := range len(key) {
					c := key[j]
					if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
						return nil, fmt.Errorf("template_syntax: invalid path segment")
					}
				}
			}
			parts = append(parts, segment{path: path})
			i += end + 3
		} else {
			literal.WriteByte(value[i])
			i++
		}
	}
	flush()
	return parts, nil
}

// Render reads only the provided metadata; substitutions are never parsed again.
func (h *Headers) Render(metadata map[string]any) (http.Header, error) {
	result := make(http.Header)
	if h == nil {
		return result, nil
	}
	for _, entry := range h.headers {
		var value strings.Builder
		for _, part := range entry.segments {
			if part.path == nil {
				value.WriteString(part.literal)
				continue
			}
			var current any = metadata
			for _, key := range part.path {
				object, ok := current.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("template_value_not_scalar: %s", strings.Join(part.path, "."))
				}
				current, ok = object[key]
				if !ok || current == nil {
					return nil, fmt.Errorf("template_value_missing: %s", strings.Join(part.path, "."))
				}
			}
			scalar, err := scalarString(current)
			if err != nil {
				return nil, fmt.Errorf("%s: %s", err, strings.Join(part.path, "."))
			}
			value.WriteString(scalar)
		}
		rendered := value.String()
		if !validValue(rendered) {
			return nil, fmt.Errorf("template_header_value_invalid: %s", entry.name)
		}
		result.Set(entry.name, rendered)
	}
	return result, nil
}

func scalarString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.FormatInt(int64(v), 10), nil
	case int8:
		return strconv.FormatInt(int64(v), 10), nil
	case int16:
		return strconv.FormatInt(int64(v), 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || math.Abs(float64(v)) > 16777215 {
			return "", fmt.Errorf("template_value_unsafe_number")
		}
		return strconv.FormatFloat(float64(v), 'f', -1, 32), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 9007199254740991 {
			return "", fmt.Errorf("template_value_unsafe_number")
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		// json.Number retains its original precision; validate that it really is a JSON number.
		var number json.Number
		if len(v) == 0 || !(v[0] == '-' || v[0] >= '0' && v[0] <= '9') {
			return "", fmt.Errorf("template_value_unsafe_number")
		}
		if err := json.Unmarshal([]byte(v), &number); err != nil || number == "" || string(number) != string(v) {
			return "", fmt.Errorf("template_value_unsafe_number")
		}
		return string(number), nil
	default:
		return "", fmt.Errorf("template_value_not_scalar")
	}
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func validValue(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 && value[i] != '\t' || value[i] == 0x7f {
			return false
		}
	}
	return true
}
