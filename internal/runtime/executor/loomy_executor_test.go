package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	loomyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/loomy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestLoomyExecutorIdentifier(t *testing.T) {
	exec := NewLoomyExecutor(nil)
	if got := exec.Identifier(); got != constant.Loomy {
		t.Fatalf("Identifier() = %q, want %q", got, constant.Loomy)
	}
}

func TestLoomyRequestToFormatIsAlwaysOpenAI(t *testing.T) {
	exec := NewLoomyExecutor(nil)
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		if got := exec.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{SourceFormat: source}); got != sdktranslator.FormatOpenAI {
			t.Fatalf("RequestToFormat(%q) = %q, want openai", source, got)
		}
	}
}

func TestLoomyChatURLUsesCredentialBaseURL(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://gateway.example/api/v1/"}}
	want := "https://gateway.example/api/v1/chat/completions"
	if got := loomyChatURL(auth); got != want {
		t.Fatalf("chat URL = %q, want %q", got, want)
	}
	if got := loomyChatURL(nil); got != loomyauth.APIBase+"/chat/completions" {
		t.Fatalf("default chat URL = %q", got)
	}
}

func TestLoomySessionPrefersAccessToken(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"access_token": "session-a", "api_key": "session-b"}}
	if got := loomySession(auth); got != "session-a" {
		t.Fatalf("session = %q", got)
	}
	meta := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "session-c"}}
	if got := loomySession(meta); got != "session-c" {
		t.Fatalf("metadata session = %q", got)
	}
}

// TestLoomyInBandErrorDetectsEnvelopeAndFrame verifies both documented shapes,
// because publishing either as success would return an empty completion.
func TestLoomyInBandErrorDetectsEnvelopeAndFrame(t *testing.T) {
	envelope := []byte(`{"code":"100002","desc":"缺少 token"}`)
	errEnvelope, okEnvelope := loomyInBandError(envelope)
	if !okEnvelope {
		t.Fatal("bare error envelope not detected")
	}
	// A stale session must map to 401 so the manager stops selecting it instead
	// of cooling the model as a transient failure.
	if status, _ := errEnvelope.(interface{ StatusCode() int }); status == nil || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("auth envelope classification = %#v", errEnvelope)
	}
	if scoped, _ := errEnvelope.(interface{ IsCredentialScoped() bool }); scoped == nil || !scoped.IsCredentialScoped() {
		t.Fatalf("auth envelope is not credential scoped: %#v", errEnvelope)
	}

	// A parameter error must not cool a model.
	badRequest, okBadRequest := loomyInBandError([]byte(`{"code":"100001","desc":"未知 task key"}`))
	if !okBadRequest {
		t.Fatal("bad-request envelope not detected")
	}
	if status, _ := badRequest.(interface{ StatusCode() int }); status == nil || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad-request classification = %#v", badRequest)
	}

	frame := []byte(`data:{"error":{"code":"invalid_model_error","message":"Unsupported model \"x\""}}`)
	err, ok := loomyInBandError(frame)
	if !ok {
		t.Fatal("SSE error frame not detected")
	}
	if !strings.Contains(err.Error(), "Unsupported model") {
		t.Fatalf("error message = %q", err.Error())
	}

	if _, okNormal := loomyInBandError([]byte(`data:{"choices":[{"delta":{"content":"hi"}}]}`)); okNormal {
		t.Fatal("a normal chunk must not be read as an error")
	}
	if _, okSuccess := loomyInBandError([]byte(`{"code":"000000","data":{"ok":true}}`)); okSuccess {
		t.Fatal("a success envelope must not be read as an error")
	}
}

// TestLoomyQuotaRejectionIsCredentialScoped pins that a points rejection cools
// the credential rather than a single model.
func TestLoomyQuotaRejectionIsCredentialScoped(t *testing.T) {
	err, ok := loomyInBandError([]byte(`data:{"error":{"message":"积分不足"}}`))
	if !ok {
		t.Fatal("quota rejection not detected")
	}
	scoped, isScoped := err.(interface{ IsCredentialScoped() bool })
	if !isScoped || !scoped.IsCredentialScoped() {
		t.Fatalf("quota error is not credential scoped: %#v", err)
	}
	status, isStatus := err.(interface{ StatusCode() int })
	if !isStatus || status.StatusCode() != http.StatusPaymentRequired {
		t.Fatalf("quota status = %#v", err)
	}
	// The HTTP-error path must agree with the in-band path.
	upstream := loomyUpstreamError(http.StatusOK, nil, []byte(`{"error":{"message":"余额不足"}}`))
	upstreamScoped, okScoped := upstream.(interface{ IsCredentialScoped() bool })
	if !okScoped || !upstreamScoped.IsCredentialScoped() {
		t.Fatalf("HTTP quota error is not credential scoped: %#v", upstream)
	}
}

func TestLoomyAggregateChunksFolds(t *testing.T) {
	raw := []byte(`{"id":"a","object":"chat.completion.chunk","model":"spark-x","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}
{"id":"a","object":"chat.completion.chunk","model":"spark-x","choices":[{"index":0,"delta":{"content":"hello "}}]}
{"id":"a","object":"chat.completion.chunk","model":"spark-x","choices":[{"index":0,"delta":{"content":"loomy"},"finish_reason":"stop"}]}`)
	out := loomyAggregateChunks(raw)
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q (payload=%s)", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello loomy" {
		t.Fatalf("content = %q", got)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q", got)
	}
}

// TestLoomyExecuteSendsBothAuthHeaders pins the header split: the chat endpoint
// requires Bearer and the desktop client also sends `token`.
func TestLoomyExecuteSendsBothAuthHeaders(t *testing.T) {
	var upstreamURL, bearer, tokenHeader, body string
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamURL = req.URL.String()
		bearer = req.Header.Get("Authorization")
		tokenHeader = req.Header.Get("token")
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data:{"id":"a","object":"chat.completion.chunk","model":"spark-x","choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n" +
					`data:{"id":"a","object":"chat.completion.chunk","model":"spark-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
					"data:[DONE]\n\n",
			)),
		}, nil
	}))

	exec := NewLoomyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   constant.Loomy,
		Attributes: map[string]string{"access_token": "session-xyz", "base_url": "https://gateway.example/api/v1"},
	}
	payload := []byte(`{"model":"spark-x","messages":[{"role":"user","content":"hi"}]}`)
	resp, errExec := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "spark-x", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if upstreamURL != "https://gateway.example/api/v1/chat/completions" {
		t.Fatalf("upstreamURL = %q", upstreamURL)
	}
	if bearer != "Bearer session-xyz" {
		t.Fatalf("Authorization = %q", bearer)
	}
	if tokenHeader != "session-xyz" {
		t.Fatalf("token header = %q", tokenHeader)
	}
	if !gjson.GetBytes([]byte(body), "stream").Bool() {
		t.Fatalf("upstream stream flag not set: %s", body)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("response content = %q (payload=%s)", got, resp.Payload)
	}
}

func TestLoomyExecuteTranslatesAnthropicClient(t *testing.T) {
	var body []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data:{"id":"a","created":1,"model":"spark-x","object":"chat.completion.chunk","usage":null,"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"hello"},"message":null}]}` + "\n\n" +
					`data:{"id":"a","created":1,"model":"spark-x","object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop","delta":{}}]}` + "\n\n" +
					"data:[DONE]\n\n",
			)),
		}, nil
	}))

	exec := NewLoomyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   constant.Loomy,
		Attributes: map[string]string{"access_token": "session-xyz", "base_url": "https://gateway.example/api/v1"},
	}
	payload := []byte(`{"model":"spark-x","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	resp, errExec := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "spark-x", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errExec != nil {
		t.Fatalf("Execute() error = %v", errExec)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "spark-x" {
		t.Fatalf("upstream model = %q (body=%s)", got, body)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "hello" {
		t.Fatalf("response content = %q (payload=%s)", got, resp.Payload)
	}
}

func TestLoomyExecutePreservesUpstreamStatus(t *testing.T) {
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"30"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
		}, nil
	}))

	exec := NewLoomyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   constant.Loomy,
		Attributes: map[string]string{"access_token": "session-xyz", "base_url": "https://gateway.example/api/v1"},
	}
	payload := []byte(`{"model":"spark-x","messages":[{"role":"user","content":"hi"}]}`)
	_, errExec := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "spark-x", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if errExec == nil {
		t.Fatal("expected an error for a 429 upstream response")
	}
	status, ok := errExec.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %#v", errExec)
	}
	retryAfter, okRetry := errExec.(interface{ RetryAfter() *time.Duration })
	if !okRetry || retryAfter.RetryAfter() == nil || *retryAfter.RetryAfter() != 30*time.Second {
		t.Fatalf("Retry-After not preserved: %#v", errExec)
	}
}

// TestLoomyRefreshReportsExpiredSessionAsUnauthorized pins that a dead session is
// terminal rather than retried: Loomy cannot rotate it.
func TestLoomyRefreshReportsExpiredSessionAsUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"100002","desc":"缺少 token"}`))
	}))
	defer server.Close()

	exec := NewLoomyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   constant.Loomy,
		Attributes: map[string]string{"access_token": "stale", "base_url": server.URL},
	}
	_, errRefresh := exec.Refresh(context.Background(), auth)
	if errRefresh == nil {
		t.Fatal("expired session reported as refreshed")
	}
	status, ok := errRefresh.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("refresh error = %#v", errRefresh)
	}
}

func TestLoomyRefreshKeepsLiveSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != "live" {
			t.Errorf("probe token header = %q", r.Header.Get("token"))
		}
		_, _ = w.Write([]byte(`{"code":"000000","data":{"balance":1}}`))
	}))
	defer server.Close()

	exec := NewLoomyExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   constant.Loomy,
		Attributes: map[string]string{"access_token": "live", "base_url": server.URL},
	}
	refreshed, errRefresh := exec.Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if refreshed == nil || refreshed.Metadata["last_refresh"] == nil {
		t.Fatalf("refresh did not record last_refresh: %#v", refreshed)
	}
}
