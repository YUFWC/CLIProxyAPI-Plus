package helps

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// FloatBoat appends this client upgrade notice to some assistant responses.
// Match its opening words instead of the download URL so a normal answer that
// happens to mention floatboat.ai is not changed.
var floatboatNoticeStarts = []string{
	"\n\n💡 Tip: Your current client version",
	"\n💡 Tip: Your current client version",
	"💡 Tip: Your current client version",
	"Tip: Your current client version",
	"\n\n💡 提示：您当前的客户端版本",
	"\n💡 提示：您当前的客户端版本",
	"💡 提示：您当前的客户端版本",
	"提示：您当前的客户端版本",
}

func floatboatNoticeIndex(text string) int {
	index := -1
	for _, marker := range floatboatNoticeStarts {
		if at := strings.Index(text, marker); at >= 0 && (index < 0 || at < index) {
			index = at
		}
	}
	return index
}

// TruncateFloatboatNotice removes the gateway's upgrade footer from Claude
// message text while leaving tool blocks and response metadata intact.
func TruncateFloatboatNotice(payload []byte) []byte {
	blocks := gjson.GetBytes(payload, "content")
	if !blocks.IsArray() {
		return payload
	}
	truncated := false
	for index, block := range blocks.Array() {
		if block.Get("type").String() != "text" {
			continue
		}
		path := fmt.Sprintf("content.%d.text", index)
		value := block.Get("text").String()
		if truncated {
			value = ""
		} else if at := floatboatNoticeIndex(value); at >= 0 {
			value = value[:at]
			truncated = true
		} else {
			continue
		}
		updated, err := sjson.SetBytes(payload, path, value)
		if err != nil {
			return payload
		}
		payload = updated
	}
	return payload
}

// FloatboatNoticeStreamFilter keeps only a possible notice prefix between
// Claude text deltas, so a marker split across network chunks is caught.
type FloatboatNoticeStreamFilter struct {
	pending string
	index   int64
	ended   bool
}

func (f *FloatboatNoticeStreamFilter) textDelta(text string) string {
	if f.ended {
		return ""
	}
	combined := f.pending + text
	if at := floatboatNoticeIndex(combined); at >= 0 {
		f.pending = ""
		f.ended = true
		return combined[:at]
	}
	keep := 0
	for _, marker := range floatboatNoticeStarts {
		for n := 1; n < len(marker) && n <= len(combined); n++ {
			if strings.HasSuffix(combined, marker[:n]) && n > keep {
				keep = n
			}
		}
	}
	f.pending = combined[len(combined)-keep:]
	return combined[:len(combined)-keep]
}

func (f *FloatboatNoticeStreamFilter) flush() []byte {
	if f.pending == "" || f.ended {
		return nil
	}
	data, err := sjson.SetBytes([]byte(`{"type":"content_block_delta","delta":{"type":"text_delta"}}`), "index", f.index)
	if err != nil {
		return nil
	}
	data, err = sjson.SetBytes(data, "delta.text", f.pending)
	if err != nil {
		return nil
	}
	f.pending = ""
	return append(append([]byte("event: content_block_delta\ndata: "), data...), '\n', '\n')
}

// ProcessFrame returns zero or more complete SSE frames to pass to the Claude
// translator. The final message events still pass through after truncation.
func (f *FloatboatNoticeStreamFilter) ProcessFrame(frame []byte) [][]byte {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(line[len("data:"):])
			break
		}
	}
	typ := gjson.GetBytes(data, "type").String()
	if typ == "content_block_delta" && gjson.GetBytes(data, "delta.type").String() == "text_delta" {
		f.index = gjson.GetBytes(data, "index").Int()
		value := f.textDelta(gjson.GetBytes(data, "delta.text").String())
		if value == "" {
			return nil
		}
		updated, err := sjson.SetBytes(data, "delta.text", value)
		if err != nil {
			return [][]byte{frame}
		}
		return [][]byte{bytes.Replace(frame, data, updated, 1)}
	}
	if typ == "content_block_stop" || typ == "message_stop" {
		if flushed := f.flush(); len(flushed) > 0 {
			return [][]byte{flushed, frame}
		}
	}
	return [][]byte{frame}
}

// Flush emits a withheld partial marker when the upstream stream ends early.
func (f *FloatboatNoticeStreamFilter) Flush() []byte { return f.flush() }

// TruncateFloatboatNoticeSSE filters a complete upstream stream used to serve
// a non-streaming request in another API format.
func TruncateFloatboatNoticeSSE(payload []byte) []byte {
	if !bytes.Contains(payload, []byte("data:")) {
		return payload
	}
	var filter FloatboatNoticeStreamFilter
	var out bytes.Buffer
	normalized := bytes.ReplaceAll(payload, []byte("\r\n"), []byte("\n"))
	for _, event := range bytes.Split(normalized, []byte("\n\n")) {
		if len(event) == 0 {
			continue
		}
		frame := append(bytes.Clone(event), '\n', '\n')
		for _, filtered := range filter.ProcessFrame(frame) {
			out.Write(filtered)
		}
	}
	if pending := filter.Flush(); len(pending) > 0 {
		out.Write(pending)
	}
	return out.Bytes()
}
