package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// loomyTestTransport routes account and business calls to canned responses.
type loomyTestTransport func(*http.Request) (*http.Response, error)

func (f loomyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestLoomyLoginPersistsCredential verifies the two-step SMS flow persists the
// session and never echoes it.
func TestLoomyLoginPersistsCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: dir}, "", nil)

	var signedPaths []string
	rt := loomyTestTransport(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		// The allowance claim is a business call authenticated with the session
		// header, not a signed account call.
		if r.URL.Path == "/api/v1/points/first-login" {
			if r.Header.Get("token") != "abcdef0123456789abcdef0123456789" {
				t.Errorf("claim token header = %q", r.Header.Get("token"))
			}
			return jsonResponse(`{"code":"000000","data":{"alreadyProcessed":false,"dailyQuota":5000,"dailyConsumed":0}}`), nil
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "account ") {
			t.Errorf("account request is not signed: %s", r.URL.Path)
		}
		signedPaths = append(signedPaths, r.URL.Path)
		if strings.Contains(string(body), `"session"`) {
			t.Errorf("request body unexpectedly carries a session")
		}
		switch r.URL.Path {
		case "/login/phone/sendMsgCode":
			return jsonResponse(`{"code":"000000","data":{"msgid":"m-1"}}`), nil
		case "/login/phone/checkCode":
			return jsonResponse(`{"code":"000000","data":{"session":"abcdef0123456789abcdef0123456789","userid":"100000000000000001"}}`), nil
		default:
			t.Errorf("unexpected account path %q", r.URL.Path)
			return jsonResponse(`{"code":"100001","desc":"unexpected"}`), nil
		}
	})

	smsReq := httptest.NewRequest("POST", "/v8/management/loomy-sms-code", strings.NewReader(`{"phone":"13800000000"}`))
	smsReq.Header.Set("Content-Type", "application/json")
	smsReq = smsReq.WithContext(context.WithValue(smsReq.Context(), "cliproxy.roundtripper", rt))
	smsW := httptest.NewRecorder()
	smsCtx, _ := gin.CreateTestContext(smsW)
	smsCtx.Request = smsReq
	h.RequestLoomySMSCode(smsCtx)
	if smsW.Code != http.StatusOK {
		t.Fatalf("send step: %d %s", smsW.Code, smsW.Body.String())
	}
	if got := gjson.GetBytes(smsW.Body.Bytes(), "msg_id").String(); got != "m-1" {
		t.Fatalf("msg_id = %q", got)
	}

	loginReq := httptest.NewRequest("POST", "/v8/management/loomy-login", strings.NewReader(`{"phone":"13800000000","code":"123456","msg_id":"m-1"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq = loginReq.WithContext(context.WithValue(loginReq.Context(), "cliproxy.roundtripper", rt))
	loginW := httptest.NewRecorder()
	loginCtx, _ := gin.CreateTestContext(loginW)
	loginCtx.Request = loginReq
	h.PostLoomyLogin(loginCtx)
	if loginW.Code != http.StatusOK {
		t.Fatalf("login step: %d %s", loginW.Code, loginW.Body.String())
	}
	if strings.Contains(loginW.Body.String(), "abcdef0123456789") {
		t.Fatal("session leaked in the management response")
	}

	// Both login steps are signed account calls; the allowance claim is a
	// session-authenticated business call and is not part of this list.
	if len(signedPaths) != 2 {
		t.Fatalf("signed calls = %v", signedPaths)
	}

	filename := gjson.GetBytes(loginW.Body.Bytes(), "id").String()
	raw, errRead := os.ReadFile(filepath.Join(dir, filename))
	if errRead != nil {
		t.Fatalf("auth file missing: %v", errRead)
	}
	if got := gjson.GetBytes(raw, "access_token").String(); got != "abcdef0123456789abcdef0123456789" {
		t.Fatalf("access_token = %q", got)
	}
	if got := gjson.GetBytes(raw, "type").String(); got != "loomy" {
		t.Fatalf("type = %q", got)
	}
	if got := gjson.GetBytes(raw, "phone").String(); got != "13800000000" {
		t.Fatalf("phone = %q", got)
	}
	if got := gjson.GetBytes(raw, "expired").String(); got == "" {
		t.Fatal("derived expiry not persisted")
	}
	if got := gjson.GetBytes(raw, "auth_kind").String(); got != coreauth.AuthKindOAuth {
		t.Fatalf("auth_kind = %q", got)
	}
}

// TestLoomyLoginRejectsInvalidSMSCode verifies an upstream rejection surfaces as
// an error instead of being persisted.
func TestLoomyLoginRejectsInvalidSMSCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: dir}, "", nil)
	rt := loomyTestTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"code":"100001","desc":"验证码错误"}`), nil
	})
	req := httptest.NewRequest("POST", "/v8/management/loomy-login", strings.NewReader(`{"phone":"13800000000","code":"000000","msg_id":"m-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), "cliproxy.roundtripper", rt))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.PostLoomyLogin(c)
	if w.Code == http.StatusOK {
		t.Fatalf("invalid code accepted: %s", w.Body.String())
	}
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("failed login persisted %d files", len(entries))
	}
}

// TestLoomyQuotaReportsPoolsSeparately verifies the read-only view distinguishes
// a known daily quota from an unavailable one.
func TestLoomyQuotaReportsPoolsSeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: dir}, "", nil)
	auth := &coreauth.Auth{
		ID:         "loomy-1.json",
		Provider:   "loomy",
		Attributes: map[string]string{"access_token": "session-xyz", "base_url": "https://gateway.example/api/v1"},
	}
	h.authManager = newLoomyTestManager(t, auth)

	rt := loomyTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("token") != "session-xyz" {
			t.Errorf("quota read token header = %q", r.Header.Get("token"))
		}
		return jsonResponse(`{"code":"000000","data":{"balance":15000,"dailyBalance":4992,"availableBalance":19992}}`), nil
	})
	req := httptest.NewRequest("GET", "/v8/management/loomy-quota?auth_index="+auth.Index, nil)
	req = req.WithContext(context.WithValue(req.Context(), "cliproxy.roundtripper", rt))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.GetLoomyQuota(c)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	if gjson.GetBytes(body, "permanent").Int() != 15000 || gjson.GetBytes(body, "daily").Int() != 4992 {
		t.Fatalf("pools not reported separately: %s", body)
	}
	// A read from this endpoint does not return the daily cap, so the field must
	// be absent rather than reported as zero.
	if gjson.GetBytes(body, "daily_quota").Exists() {
		t.Fatalf("daily quota must be unknown for a read: %s", body)
	}
}

// TestLoomyQuotaRejectsOtherProvider verifies the handler refuses a credential
// belonging to another provider.
func TestLoomyQuotaRejectsOtherProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	auth := &coreauth.Auth{ID: "other.json", Provider: "xiaohuanxiong", Attributes: map[string]string{"access_token": "t"}}
	h.authManager = newLoomyTestManager(t, auth)
	req := httptest.NewRequest("GET", "/v8/management/loomy-quota?auth_index="+auth.Index, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.GetLoomyQuota(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// jsonResponse builds a JSON upstream response.
func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// loomyTestManager registers one auth so authByIndex can resolve it.
func newLoomyTestManager(t *testing.T, auth *coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return manager
}
