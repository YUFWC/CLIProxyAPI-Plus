package template

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestHeadersRenderAccountIsolationAndEscaping(t *testing.T) {
	compiled, err := Compile(map[string]string{
		"UID":       "prefix-${meta.uid}-${active}",
		"X-Literal": "$${meta.uid}",
		"X-Copy":    "${meta.uid}",
	}, "codebuddy-cn")
	if err != nil {
		t.Fatal(err)
	}
	first, err := compiled.Render(map[string]any{"meta": map[string]any{"uid": "${secret}"}, "active": true, "secret": "must-not-expand"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiled.Render(map[string]any{"meta": map[string]any{"uid": float64(15451045151)}, "active": false})
	if err != nil {
		t.Fatal(err)
	}
	if first.Get("UID") != "prefix-${secret}-true" || first.Get("X-Literal") != "${meta.uid}" || first.Get("X-Copy") != "${secret}" {
		t.Fatalf("unexpected first-account headers: %v", first)
	}
	if second.Get("UID") != "prefix-15451045151-false" || first.Get("UID") != "prefix-${secret}-true" {
		t.Fatalf("account render was not isolated: first=%v second=%v", first, second)
	}
	if got := compiled.Paths(); !reflect.DeepEqual(got, []string{"active", "meta.uid"}) {
		t.Fatalf("paths: %v", got)
	}
	paths := compiled.Paths()
	paths[0] = "mutated"
	if compiled.Paths()[0] != "active" {
		t.Fatal("caller changed compiled metadata references")
	}
}

func TestHeadersRejectUnsafeAccountValues(t *testing.T) {
	compiled, err := Compile(map[string]string{"UID": "${meta.uid}"}, "codearts")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		metadata map[string]any
		code     string
	}{
		{"missing", map[string]any{}, "template_value_missing"},
		{"null", map[string]any{"meta": nil}, "template_value_missing"},
		{"object", map[string]any{"meta": map[string]any{"uid": map[string]any{}}}, "template_value_not_scalar"},
		{"array", map[string]any{"meta": []any{"uid"}}, "template_value_not_scalar"},
		{"unsafe integer", map[string]any{"meta": map[string]any{"uid": float64(9007199254740992)}}, "template_value_unsafe_number"},
		{"nan", map[string]any{"meta": map[string]any{"uid": math.NaN()}}, "template_value_unsafe_number"},
		{"quoted number", map[string]any{"meta": map[string]any{"uid": json.Number(`"123"`)}}, "template_value_unsafe_number"},
		{"injected header", map[string]any{"meta": map[string]any{"uid": "alice\r\nAuthorization: secret"}}, "template_header_value_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compiled.Render(tc.metadata); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
		})
	}
	precise, err := compiled.Render(map[string]any{"meta": map[string]any{"uid": json.Number("9007199254740993")}})
	if err != nil || precise.Get("UID") != "9007199254740993" {
		t.Fatalf("lossless JSON number: %v, %v", precise, err)
	}
}

func TestHeadersRejectPolicyViolations(t *testing.T) {
	cases := []struct {
		name, provider string
		headers        map[string]string
	}{
		{"duplicate canonical names", "trae", map[string]string{"UID": "one", "uid": "two"}},
		{"transport", "codebuddy-cn", map[string]string{"cOnNeCtIoN": "close"}},
		{"authorization", "codearts", map[string]string{"authorization": "token"}},
		{"signature date", "codearts", map[string]string{"X-SDK-DATE": "date"}},
		{"security token", "codearts", map[string]string{"x-security-token": "token"}},
		{"trae auth", "trae", map[string]string{"cloud-ide-jwt": "token"}},
		{"invalid name", "trae", map[string]string{"X Bad": "value"}},
		{"literal control", "trae", map[string]string{"UID": "a\x00b"}},
		{"unclosed path", "trae", map[string]string{"UID": "${meta.uid"}},
		{"empty path", "trae", map[string]string{"UID": "${meta..uid}"}},
		{"array syntax", "trae", map[string]string{"UID": "${meta[0]}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(tc.headers, tc.provider); err == nil {
				t.Fatal("accepted invalid headers")
			}
		})
	}
}
