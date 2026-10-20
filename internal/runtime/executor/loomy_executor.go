// Package executor provides per-provider runtime executors.
//
// This file implements the Loomy (讯飞 Loomy) managed-account executor.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	loomyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/loomy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// compile-time assertion that the executor satisfies the full ProviderExecutor
// contract.
var _ cliproxyauth.ProviderExecutor = (*LoomyExecutor)(nil)

// LoomyExecutor proxies requests to the Loomy business gateway.
//
// Four measured upstream behaviours shape this executor (all recorded by the
// reference implementation in docs/providers/loomy-integration.md):
//
//   - Inference is a plain OpenAI Chat Completions surface with standard SSE, so
//     the shared OpenAI translators produce both the body and the client-visible
//     response. The provider identity stays separate from the wire format.
//   - The chat endpoint requires `Authorization: Bearer <session>` while every
//     business endpoint requires the lowercase `token` header; the desktop client
//     sends both on the chat call, and so does this executor.
//   - Application failures can arrive as an HTTP 200 with an envelope, or as an
//     SSE data frame carrying `error`. Treating either as success would surface an
//     empty completion.
//   - There is no refresh endpoint. Refresh only probes liveness; an expired
//     session requires a new login.
type LoomyExecutor struct {
	cfg *config.Config
}

// NewLoomyExecutor creates the Loomy executor.
func NewLoomyExecutor(cfg *config.Config) *LoomyExecutor {
	return &LoomyExecutor{cfg: cfg}
}

// Identifier returns the executor/provider identifier.
func (e *LoomyExecutor) Identifier() string { return constant.Loomy }

// RequestToFormat reports the upstream wire format. The gateway implements only
// OpenAI Chat Completions, so every request is translated to OpenAI regardless of
// the client's source format.
func (e *LoomyExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

// PrepareRequest injects Loomy credentials into an outgoing HTTP request.
func (e *LoomyExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	session := loomySession(auth)
	if session != "" {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+session)
		// Business endpoints only accept this header; the chat endpoint accepts
		// both, matching the desktop client.
		req.Header.Set("token", session)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Loomy credentials and executes the request.
func (e *LoomyExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("loomy executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request.
//
// The gateway streams every response, so a non-streaming answer is produced from
// the streamed body. How depends on the client's shape: an OpenAI client gets the
// chunks folded into a chat.completion, while a client that speaks another
// protocol gets the folded completion translated into its own non-stream payload.
func (e *LoomyExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	if responseFormat == sdktranslator.FormatOpenAI {
		return e.executeFoldOpenAI(ctx, auth, req, opts)
	}
	return e.executeNonStream(ctx, auth, req, opts, responseFormat)
}

// executeFoldOpenAI streams and folds the OpenAI chunks into a chat.completion.
func (e *LoomyExecutor) executeFoldOpenAI(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	opts.Stream = true
	streamResult, errStream := e.ExecuteStream(ctx, auth, req, opts)
	if errStream != nil {
		return cliproxyexecutor.Response{}, errStream
	}
	if streamResult == nil {
		return cliproxyexecutor.Response{}, nil
	}
	var buffer bytes.Buffer
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			return cliproxyexecutor.Response{}, chunk.Err
		}
		if len(chunk.Payload) > 0 {
			_, _ = buffer.Write(chunk.Payload)
			_, _ = buffer.Write([]byte("\n"))
		}
	}
	payload := loomyAggregateChunks(buffer.Bytes())
	if len(bytes.TrimSpace(payload)) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: "loomy: empty upstream response"}
	}
	return cliproxyexecutor.Response{Payload: payload, Headers: streamResult.Headers}, nil
}

// executeNonStream translates the gateway's streamed body into the client's
// non-stream format.
//
// The body is folded into an OpenAI chat.completion first, then the shared
// non-stream translator converts it into the client's shape. Feeding the raw SSE
// frames to the translator is not possible: its non-stream entry point expects a
// bare JSON object, while the stream entry point only accepts one "data:" line.
func (e *LoomyExecutor) executeNonStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, responseFormat sdktranslator.Format) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FormatOpenAI

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := helps.TranslateRequestPairWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, false, isCompat)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	body = loomyForceStream(body)

	url := loomyChatURL(auth)
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errReq != nil {
		return resp, errReq
	}
	if errHeaders := e.applyHeaders(httpReq, auth, true); errHeaders != nil {
		return resp, errHeaders
	}
	e.recordRequest(ctx, url, httpReq.Header.Clone(), body, auth)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("loomy executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	raw, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 64<<20))
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return resp, loomyUpstreamError(httpResp.StatusCode, httpResp.Header, raw)
	}
	if streamErr, isError := loomyInBandError(raw); isError {
		return resp, streamErr
	}

	completion := loomyCompletionFromSSE(raw)
	if len(completion) == 0 {
		// A plain JSON body (an ad-hoc or future non-stream response) is already
		// a chat.completion and passes through.
		if trimmed := bytes.TrimSpace(raw); bytes.HasPrefix(trimmed, []byte("{")) && json.Valid(trimmed) {
			completion = trimmed
		}
	}
	if len(completion) == 0 {
		return resp, statusErr{code: http.StatusBadGateway, msg: "loomy: empty upstream response"}
	}
	reporter.ObserveResponseModel(completion)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(completion))

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, completion, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream performs a streaming request against the gateway.
func (e *LoomyExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FormatOpenAI

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}

	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, body := helps.TranslateRequestPairWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, true, isCompat)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	body = loomyForceStream(body)

	url := loomyChatURL(auth)
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	if errHeaders := e.applyHeaders(httpReq, auth, true); errHeaders != nil {
		return nil, errHeaders
	}
	e.recordRequest(ctx, url, httpReq.Header.Clone(), body, auth)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 8<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("loomy executor: close response body error: %v", errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
		return nil, loomyUpstreamError(httpResp.StatusCode, httpResp.Header, raw)
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("loomy executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			streamUsage.ObserveOpenAIStream(line)
			if streamErr, isError := loomyInBandError(line); isError {
				streamUsage.PublishFailure(ctx, reporter, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}
			chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			if responseFormat == sdktranslator.FormatOpenAIResponse {
				for i := range chunks {
					chunks[i] = helps.EnsureResponsesUsageDetails(chunks[i])
				}
			}
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			streamUsage.PublishFailure(ctx, reporter, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates input tokens with the OpenAI estimator, so token
// counting stays available without an upstream round trip.
func (e *LoomyExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FormatOpenAI
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, from != to, helps.APIKeyModelIsCompat(req))
	body, errThinking := helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if errThinking != nil {
		return cliproxyexecutor.Response{}, errThinking
	}
	enc, errEnc := helps.TokenizerForModel(baseModel)
	if errEnc != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("loomy executor: tokenizer init failed: %w", errEnc)
	}
	count, errCount := helps.CountOpenAIChatTokens(enc, body)
	if errCount != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("loomy executor: token counting failed: %w", errCount)
	}
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, helps.BuildOpenAIUsageJSON(count))
	return cliproxyexecutor.Response{Payload: out}, nil
}

// Refresh probes the session. Loomy has no refresh endpoint: the session is
// issued for a fixed 14-day window and only a new login replaces it.
//
// A rejected session is reported as an unauthorized error so the manager stops
// selecting this credential instead of retrying it after every cooldown. A
// transport failure is propagated unchanged: a network blip must not disable a
// credential.
func (e *LoomyExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("loomy executor: refresh called")
	if refreshed, handled, errHome := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, errHome
	}
	if auth == nil {
		return nil, fmt.Errorf("loomy executor: auth is nil")
	}
	session := loomySession(auth)
	if session == "" {
		return nil, &cliproxyauth.Error{Code: "unauthorized", Message: "loomy credential has no session", HTTPStatus: http.StatusUnauthorized}
	}
	// The credential's base URL is honored so a self-hosted deployment and the
	// offline suite probe the same origin the chat path uses.
	client := loomyauth.NewClientWithBaseURLs(e.cfg, auth.ProxyURL, loomyAttribute(auth, "base_url"), "")
	if errProbe := client.Probe(ctx, session); errProbe != nil {
		var upstream *loomyauth.Error
		if errors.As(errProbe, &upstream) && upstream.Code == loomyauth.AuthErrorCode {
			return nil, &cliproxyauth.Error{Code: "unauthorized", Message: "loomy session expired; sign in again", HTTPStatus: http.StatusUnauthorized}
		}
		// Any other failure (transport, business error) must not disable a
		// credential that may still be usable.
		return nil, errProbe
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	return auth, nil
}

// applyHeaders sets the Loomy credentials and request headers.
func (e *LoomyExecutor) applyHeaders(httpReq *http.Request, auth *cliproxyauth.Auth, stream bool) error {
	if httpReq == nil {
		return nil
	}
	session := loomySession(auth)
	if session != "" {
		httpReq.Header.Del("x-api-key")
		httpReq.Header.Set("Authorization", "Bearer "+session)
		// The chat endpoint accepts both headers; the desktop client sends both.
		httpReq.Header.Set("token", session)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("Accept", accept)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	return nil
}

func (e *LoomyExecutor) recordRequest(ctx context.Context, url string, headers http.Header, body []byte, auth *cliproxyauth.Auth) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers,
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

// loomyChatURL resolves the inference URL for the credential.
func loomyChatURL(auth *cliproxyauth.Auth) string {
	base := loomyAttribute(auth, "base_url")
	if base == "" {
		base = loomyauth.APIBase
	}
	return strings.TrimRight(base, "/") + "/chat/completions"
}

// loomySession resolves the session token that authorizes the gateway.
func loomySession(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if token := strings.TrimSpace(auth.Attributes["access_token"]); token != "" {
			return token
		}
		if token := strings.TrimSpace(auth.Attributes["api_key"]); token != "" {
			return token
		}
	}
	return loomyAttribute(auth, "access_token")
}

// loomyAttribute reads a credential attribute, falling back to metadata.
func loomyAttribute(auth *cliproxyauth.Auth, key string) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// loomyUpstreamError classifies a non-2xx response with the OpenAI-compatible
// status/retry semantics so the manager's cooldown policy sees the real status.
//
// A business envelope inside an HTTP error still carries meaning: the Loomy
// quota rejection (`积分不足`) must cool the credential rather than the model, so
// it is reported as 402 with credential scope.
func loomyUpstreamError(statusCode int, headers http.Header, body []byte) error {
	base := newOpenAICompatStatusError(statusCode, headers, body)
	if loomyIsQuotaBody(body) {
		return statusErr{code: http.StatusPaymentRequired, msg: string(body), retryAfter: base.retryAfter, credentialScoped: true}
	}
	// A business envelope can also ride on an HTTP error. Its code is more
	// specific than the transport status, so it wins for the two codes the
	// manager treats specially.
	switch loomyBusinessCode(body) {
	case loomyauth.AuthErrorCode:
		return statusErr{code: http.StatusUnauthorized, msg: string(body), retryAfter: base.retryAfter, credentialScoped: true}
	case loomyauth.BadRequestCode:
		return statusErr{code: http.StatusBadRequest, msg: string(body), retryAfter: base.retryAfter}
	}
	return base
}

// loomyInBandError reports the error the gateway may send inside an HTTP 200.
//
// Two shapes are observed: a business envelope `{code, desc}` and an SSE
// `data:` frame that contains `error`. A 200 carrying either must not be
// published as an empty completion. The business code maps to the status class
// the manager understands, so a stale session stops being selected and a
// parameter error does not cool a model:
//
//	100002 (session invalid) → 401
//	100001 (bad request)     → 400
//	points exhaustion        → 402, credential scoped
//	anything else            → 502
func loomyInBandError(line []byte) (error, bool) {
	payload := loomyErrorJSON(line)
	if payload == nil {
		return nil, false
	}
	message := loomyErrorMessage(payload)
	if message == "" {
		return nil, false
	}
	if loomyIsQuotaBody(payload) {
		return statusErr{code: http.StatusPaymentRequired, msg: "loomy: " + message, credentialScoped: true}, true
	}
	switch loomyBusinessCode(payload) {
	case loomyauth.AuthErrorCode:
		return statusErr{code: http.StatusUnauthorized, msg: "loomy: " + message, credentialScoped: true}, true
	case loomyauth.BadRequestCode:
		return statusErr{code: http.StatusBadRequest, msg: "loomy: " + message}, true
	}
	return statusErr{code: http.StatusBadGateway, msg: "loomy: " + message}, true
}

// loomyBusinessCode reads the envelope code from an error payload.
func loomyBusinessCode(payload []byte) string {
	var decoded struct {
		Code string `json:"code"`
	}
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Code)
}

// loomyErrorJSON extracts a JSON error object from a bare object or SSE frame.
func loomyErrorJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !bytes.Contains(trimmed, []byte(`"error"`)) && !bytes.Contains(trimmed, []byte(`"code"`)) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("{")) && json.Valid(trimmed) {
		if loomyErrorMessage(trimmed) != "" {
			return trimmed
		}
		return nil
	}
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		candidate := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if json.Valid(candidate) && (bytes.Contains(candidate, []byte(`"error"`)) || bytes.Contains(candidate, []byte(`"code"`))) {
			if loomyErrorMessage(candidate) != "" {
				return candidate
			}
		}
	}
	return nil
}

// loomyErrorMessage reads the first usable message from an error payload.
func loomyErrorMessage(payload []byte) string {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		return ""
	}
	if nested, ok := decoded["error"].(map[string]any); ok {
		if message := loomyString(nested["message"]); message != "" {
			return message
		}
		if message := loomyString(nested["code"]); message != "" {
			return message
		}
	}
	if message := loomyString(decoded["error"]); message != "" {
		return message
	}
	code := loomyString(decoded["code"])
	// A successful envelope (`000000`) is not an error.
	if code == "" || code == loomyauth.OKCode {
		return ""
	}
	if message := loomyString(decoded["desc"]); message != "" {
		return message
	}
	if message := loomyString(decoded["message"]); message != "" {
		return message
	}
	return "business error " + code
}

func loomyString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// loomyIsQuotaBody reports the points-exhaustion rejection.
func loomyIsQuotaBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"积分不足", "余额不足", "insufficient points", "insufficient balance", "insufficient_credits"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// loomyCompletionFromSSE extracts the JSON objects from an SSE body and folds
// them into a single chat.completion.
func loomyCompletionFromSSE(sse []byte) []byte {
	var lines bytes.Buffer
	for _, line := range bytes.Split(sse, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		candidate := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(candidate) == 0 || bytes.Equal(candidate, []byte("[DONE]")) || !json.Valid(candidate) {
			continue
		}
		lines.Write(candidate)
		lines.WriteByte('\n')
	}
	if lines.Len() == 0 {
		return nil
	}
	return loomyAggregateChunks(lines.Bytes())
}

// loomyAggregateChunks folds OpenAI chat.completion.chunk objects into a single
// chat.completion. The gateway always streams, so a stream:false client needs
// the chunks reassembled.
func loomyAggregateChunks(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	if trimmed := bytes.TrimSpace(raw); bytes.HasPrefix(trimmed, []byte("{")) && json.Valid(trimmed) {
		var decoded map[string]any
		if errUnmarshal := json.Unmarshal(trimmed, &decoded); errUnmarshal == nil {
			if object, _ := decoded["object"].(string); object == "chat.completion" {
				return trimmed
			}
		}
	}
	// aggregateCodeBuddyCNChunks already implements the chunk-folding contract
	// for OpenAI chat.completion.chunk objects, including streamed tool-call
	// argument fragments and reasoning_content.
	aggregated := aggregateCodeBuddyCNChunks(raw)
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(aggregated, &decoded); errUnmarshal != nil {
		return raw
	}
	if choices, ok := decoded["choices"].([]any); !ok || len(choices) == 0 {
		return raw
	}
	return aggregated
}

// loomyForceStream pins the upstream stream flag. The gateway streams every
// response; sending true keeps the wire shape aligned with the response so logs
// and diagnostics do not claim a non-stream request.
func loomyForceStream(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	return helps.SetBoolIfDifferent(body, "stream", true)
}
