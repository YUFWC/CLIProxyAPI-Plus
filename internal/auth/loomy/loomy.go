// Package loomy implements credential acquisition and read-only account reads
// for iFlytek Loomy (讯飞 Loomy).
//
// The protocol was recovered from the Loomy desktop client bundle as vendored in
// the locally supplied `dsh-our-free-model` channel pack
// (`vendor/channel-pack/src/loomy*.ts`, hashes recorded in
// docs/providers/loomy-integration.md):
//
//   - Login is a phone SMS code exchange (or a WeChat QR scan) against the
//     iFlytek CAccount service (https://account.xfinfr.com). Every account call is
//     signed with HMAC-SHA1 and an `account {ak}:{sig}` Authorization header.
//   - The returned `session` is a 32-character lowercase hex token. The client
//     asks the server for `expire: 1209600` (14 days) and the server does not
//     return an expiry, so the expiry recorded here is derived locally.
//   - There is no refresh endpoint. `Refresh` therefore only probes liveness; an
//     expired credential requires a new login.
//   - Inference and account reads use two different authentication headers:
//     POST /chat/completions requires `Authorization: Bearer <session>`, while
//     /models and /points/* require the lowercase `token: <session>` header.
//     The chat endpoint accepts both at once, which is what the desktop client
//     sends and what this package documents for callers.
//   - Business failures return HTTP 200 with an envelope `{code, desc, data}`.
//     Only `code == "000000"` is success; `100002` means the session is invalid.
//
// The provider identity ("loomy") is kept separate from the wire format: the
// chat endpoint speaks OpenAI Chat Completions, so CLIProxyAPI reuses the
// existing OpenAI translators instead of impersonating another provider.
package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	// Provider is the canonical CLIProxyAPI provider key.
	Provider = "loomy"
	// Label is the human readable provider name.
	Label = "Loomy (讯飞)"

	// APIBase is the business origin for inference, model listing and points.
	APIBase = "https://loomyad.xunfei.cn/api/v1"
	// AccountBase is the iFlytek CAccount origin used for SMS/WeChat login.
	AccountBase = "https://account.xfinfr.com"

	// OKCode is the business success code.
	OKCode = "000000"
	// AuthErrorCode is the session-invalid code. It must never be retried.
	AuthErrorCode = "100002"
	// BadRequestCode is the generic parameter-error code.
	BadRequestCode = "100001"

	// AppID is the CAccount appid reported by the desktop client.
	AppID = "GM3LOOMY"
	// AccessKeyID is the public CAccount access key id shipped with the client.
	AccessKeyID = "2thryby66wxi53sk"
	// AccessKeySecret is the CAccount signing secret shipped with the client.
	AccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"
	// ClientUA is the hardcoded client identity the account service expects.
	ClientUA = "Loomy|Desktop|Electron|macOS"

	// SMSCodeTTLSeconds is the SMS code validity the client requests.
	SMSCodeTTLSeconds = 300
	// SessionTTLSeconds is the session validity the client requests (14 days).
	SessionTTLSeconds = 1_209_600

	// WeChatAppID is the WeChat open-platform website app id from the client.
	WeChatAppID = "wx18d60be432287cf8"
	// WeChatRedirectURI is the allow-listed redirect the QR page is fetched
	// with. It is never loaded (the authorization code arrives through the
	// long-poll), it only satisfies WeChat's domain check.
	WeChatRedirectURI = "https://loomy.xunfei.cn/oauth/wechat/callback"
	// WeChatPollTimeout is the long-poll read deadline.
	WeChatPollTimeout = 40 * time.Second
	// WeChatOrigin is the long-poll origin.
	WeChatOrigin = "https://long.open.weixin.qq.com"

	// DefaultHTTPTimeout bounds credential acquisition calls only. Per
	// repository policy no timeout is applied once an inference connection is
	// established.
	DefaultHTTPTimeout = 60 * time.Second
)

// Envelope is the shared business response shape. Business failures arrive as
// HTTP 200, so callers must inspect Code rather than the status.
type Envelope struct {
	Code    string          `json:"code"`
	Desc    string          `json:"desc"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Error is a structured upstream failure.
type Error struct {
	// StatusCode is the HTTP status when the failure came from the transport.
	StatusCode int
	// Code is the business code (`100002` etc.) when one was returned.
	Code string
	// Message is a human readable message.
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return "loomy: unknown error"
	}
	if e.Code != "" {
		return fmt.Sprintf("loomy: HTTP %d code %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("loomy: HTTP %d: %s", e.StatusCode, e.Message)
}

// StatusCodeValue lets callers inspect the status without importing net/http.
func (e *Error) StatusCodeValue() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

// TokenData is the credential payload from a successful login.
type TokenData struct {
	// Session is the 32-character lowercase hex iFlytek session.
	Session string
	// UserID is the iFlytek user id.
	UserID string
	// Phone is the bound phone number when the flow returned one.
	Phone string
	// Nickname is the WeChat display name when the flow returned one.
	Nickname string
}

// Model is one entry of the business model catalogue.
type Model struct {
	ID              string
	DisplayName     string
	ContextLimit    int
	ReasoningLevels []string
	SupportsImage   bool
	SupportsReason  bool
}

// CreditDetail is the read-only points view.
//
// Loomy keeps two pools: permanent points (`balance`) and a daily allowance
// pool (`dailyBalance`) that is granted per business day and does not refill
// after consumption. `dailyQuota` is only returned by the write endpoint, so it
// is absent for a pure read and must not be assumed.
type CreditDetail struct {
	Permanent       int64
	Daily           int64
	Total           int64
	DailyQuota      int64
	DailyQuotaKnown bool
	DailyConsumed   int64
	DailyCycleDate  string
}

// Client performs Loomy account and business requests.
type Client struct {
	httpClient   *http.Client
	apiBase      string
	accountBase  string
	wechatOrigin string
}

// NewClient creates a proxy-aware client against the production origins.
func NewClient(cfg *config.Config) *Client {
	return NewClientWithProxyURL(cfg, "")
}

// NewClientWithProxyURL creates a client with an optional per-auth proxy.
func NewClientWithProxyURL(cfg *config.Config, proxyURL string) *Client {
	return NewClientWithBaseURLs(cfg, proxyURL, APIBase, AccountBase)
}

// NewClientWithBaseURLs creates a client against explicit origins. It exists so
// tests and self-hosted deployments can point at a different host.
func NewClientWithBaseURLs(cfg *config.Config, proxyURL, apiBase, accountBase string) *Client {
	httpClient := &http.Client{Timeout: DefaultHTTPTimeout}
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
	}
	if strings.TrimSpace(proxyURL) != "" {
		sdkCfg.ProxyURL = strings.TrimSpace(proxyURL)
	}
	httpClient = util.SetProxy(&sdkCfg, httpClient)

	resolvedAPI := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if resolvedAPI == "" {
		resolvedAPI = APIBase
	}
	resolvedAccount := strings.TrimRight(strings.TrimSpace(accountBase), "/")
	if resolvedAccount == "" {
		resolvedAccount = AccountBase
	}
	return &Client{httpClient: httpClient, apiBase: resolvedAPI, accountBase: resolvedAccount, wechatOrigin: WeChatOrigin}
}

// WithHTTPClient replaces the transport used by every request. Management
// callers pass the proxy-aware client built from the request context, which is
// the same pattern the other managed-account providers use.
func (c *Client) WithHTTPClient(httpClient *http.Client) *Client {
	if c == nil || httpClient == nil {
		return c
	}
	c.httpClient = httpClient
	return c
}

// WithWeChatOrigin points the QR long-poll at another origin. It exists so the
// poll state machine can be exercised against a local server.
func (c *Client) WithWeChatOrigin(origin string) *Client {
	if c == nil {
		return nil
	}
	if trimmed := strings.TrimRight(strings.TrimSpace(origin), "/"); trimmed != "" {
		c.wechatOrigin = trimmed
	}
	return c
}

// WeChatOriginBase returns the origin used for the QR long-poll.
func (c *Client) WeChatOriginBase() string {
	if c == nil || c.wechatOrigin == "" {
		return WeChatOrigin
	}
	return c.wechatOrigin
}

// APIBaseURL returns the business origin this client talks to.
func (c *Client) APIBaseURL() string {
	if c == nil || c.apiBase == "" {
		return APIBase
	}
	return c.apiBase
}

// AccountBaseURL returns the account origin this client talks to.
func (c *Client) AccountBaseURL() string {
	if c == nil || c.accountBase == "" {
		return AccountBase
	}
	return c.accountBase
}

// ChatURL is the only inference surface the provider implements.
func (c *Client) ChatURL() string { return c.APIBaseURL() + "/chat/completions" }

// ParseEnvelope decodes the envelope and reports success only for OKCode.
func ParseEnvelope(payload []byte) (Envelope, error) {
	var env Envelope
	if errUnmarshal := json.Unmarshal(payload, &env); errUnmarshal != nil {
		return Envelope{}, &Error{Message: "response is not a JSON object"}
	}
	if env.Code == OKCode {
		return env, nil
	}
	message := strings.TrimSpace(env.Desc)
	if message == "" {
		message = strings.TrimSpace(env.Message)
	}
	if message == "" {
		message = fmt.Sprintf("business error %s", env.Code)
	}
	status := http.StatusBadGateway
	switch env.Code {
	case AuthErrorCode:
		status = http.StatusUnauthorized
	case BadRequestCode:
		status = http.StatusBadRequest
	}
	return env, &Error{StatusCode: status, Code: env.Code, Message: message}
}

// BusinessGET performs a read-only business request with the `token` header.
func (c *Client) BusinessGET(ctx context.Context, session, path string) (json.RawMessage, error) {
	return c.businessRequest(ctx, http.MethodGet, session, path, nil)
}

// businessRequest performs one business call and unwraps the envelope.
func (c *Client) businessRequest(ctx context.Context, method, session, path string, body any) (json.RawMessage, error) {
	if strings.TrimSpace(session) == "" {
		return nil, &Error{StatusCode: http.StatusUnauthorized, Message: "loomy session is empty"}
	}
	var payload []byte
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return nil, errMarshal
		}
		payload = raw
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, c.APIBaseURL()+path, bytes.NewReader(payload))
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	// Business endpoints only accept the lowercase `token` header; sending
	// Bearer instead is answered with `100002 缺少 token`.
	req.Header.Set("token", session)
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, &Error{Message: errDo.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, &Error{StatusCode: resp.StatusCode, Message: errRead.Error()}
	}
	env, errEnvelope := ParseEnvelope(raw)
	if errEnvelope != nil {
		if upstream, ok := errEnvelope.(*Error); ok && upstream.StatusCode == 0 {
			upstream.StatusCode = resp.StatusCode
		}
		return nil, errEnvelope
	}
	return env.Data, nil
}

// Probe reports whether the session is still accepted.
//
// The read uses the cheapest side-effect-free endpoint so a probe never claims
// or consumes points. A transport failure is returned as a non-auth error: a
// network blip must not force the operator to log in again.
func (c *Client) Probe(ctx context.Context, session string) error {
	_, err := c.BusinessGET(ctx, session, "/points/records?pageNo=1&pageSize=1&recordType=all")
	if err == nil {
		return nil
	}
	var upstream *Error
	if errors.As(err, &upstream) && upstream.Code == AuthErrorCode {
		return &Error{StatusCode: http.StatusUnauthorized, Code: AuthErrorCode, Message: "session expired; sign in again"}
	}
	return err
}

// SessionExpiry derives the locally recorded expiry for a session issued now.
func SessionExpiry(now time.Time) time.Time {
	return now.Add(SessionTTLSeconds * time.Second)
}

// EscapeRFC3986 mirrors the client's percent-encoding: encodeURIComponent,
// which additionally escapes the five characters Go's query encoder leaves
// literal (`! ' ( ) *`). The space form differs too: Go writes `+`, the client
// writes `%20`.
func EscapeRFC3986(value string) string {
	escaped := strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
	for raw, encoded := range map[string]string{
		"!": "%21", "'": "%27", "(": "%28", ")": "%29", "*": "%2A",
	} {
		escaped = strings.ReplaceAll(escaped, raw, encoded)
	}
	return escaped
}
