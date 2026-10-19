package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTruncateFloatboatNotice(t *testing.T) {
	message := `{"content":[{"type":"text","text":"Actual answer.\n\n💡 Tip: Your current client version will soon stop receiving maintenance. https://floatboat.ai/download?utm_source=app"},{"type":"tool_use","name":"lookup","input":{}},{"type":"text","text":"💡 提示：您当前的客户端版本即将停止维护。"}],"usage":{"output_tokens":42}}`
	got := TruncateFloatboatNotice([]byte(message))
	if text := gjson.GetBytes(got, "content.0.text").String(); text != "Actual answer." {
		t.Fatalf("first text = %q", text)
	}
	if text := gjson.GetBytes(got, "content.2.text").String(); text != "" {
		t.Fatalf("later text = %q", text)
	}
	if tool := gjson.GetBytes(got, "content.1.name").String(); tool != "lookup" {
		t.Fatalf("tool block = %q", tool)
	}
	if usage := gjson.GetBytes(got, "usage.output_tokens").Int(); usage != 42 {
		t.Fatalf("usage = %d", usage)
	}
	plain := []byte(`{"content":[{"type":"text","text":"Visit https://floatboat.ai/download?utm_source=app for release notes."}]}`)
	if got := string(TruncateFloatboatNotice(plain)); got != string(plain) {
		t.Fatalf("ordinary URL changed: %s", got)
	}
}

func TestFloatboatNoticeStreamFilterSplitMarker(t *testing.T) {
	var filter FloatboatNoticeStreamFilter
	frames := []string{
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Actual answer.\n\n💡 Ti"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"p: Your current client version will soon stop receiving maintenance."}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"💡 提示：您当前的客户端版本即将停止维护。"}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	var output strings.Builder
	for _, frame := range frames {
		for _, filtered := range filter.ProcessFrame([]byte(frame)) {
			output.Write(filtered)
		}
	}
	got := output.String()
	if !strings.Contains(got, "Actual answer.") || strings.Contains(got, "current client version") || strings.Contains(got, "您当前的客户端版本") {
		t.Fatalf("filtered stream = %s", got)
	}
	if !strings.Contains(got, "message_stop") {
		t.Fatalf("message_stop lost: %s", got)
	}
	if strings.Contains(got, "💡") {
		t.Fatalf("notice prefix leaked: %s", got)
	}
}

func TestTruncateFloatboatNoticeSSE(t *testing.T) {
	input := "event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Answer. 💡 提示：您当前的客户端版本即将停止维护。"}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	got := string(TruncateFloatboatNoticeSSE([]byte(input)))
	if !strings.Contains(got, "Answer. ") || strings.Contains(got, "您当前的客户端版本") || !strings.Contains(got, "message_stop") {
		t.Fatalf("filtered SSE = %s", got)
	}
}
