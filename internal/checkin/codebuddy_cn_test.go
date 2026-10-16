package checkin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type buddyCheckinTransport struct {
	target *url.URL
	base   http.RoundTripper
	t      *testing.T
}

func (tr buddyCheckinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "copilot.tencent.com" {
		tr.t.Errorf("unexpected business host %s", req.URL.Host)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = tr.target.Scheme, tr.target.Host
	clone.Host = tr.target.Host
	return tr.base.RoundTrip(clone)
}

func buddyCheckinContext(t *testing.T, handler http.HandlerFunc) context.Context {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	return context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(buddyCheckinTransport{target, server.Client().Transport, t}))
}

func TestCodeBuddyCheckinRequiresExplicitBusinessEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name, status, claim, outcome string
		requests                     int
	}{
		{"successful_claim", `{"code":0,"data":{"today_checked_in":false}}`, `{"code":0}`, OutcomeSuccess, 2},
		{"already_checked_skips_claim", `{"code":0,"data":{"today_checked_in":true}}`, "", OutcomeAlreadyDone, 1},
		{"denied_status_even_with_true_flag", `{"code":7,"data":{"today_checked_in":true}}`, "", OutcomeFailed, 1},
		{"missing_status_field_is_not_success", `{"code":0,"data":{}}`, "", OutcomeFailed, 1},
		{"wrong_status_type_is_not_success", `{"code":0,"data":{"today_checked_in":"true"}}`, "", OutcomeFailed, 1},
		{"missing_claim_code_is_not_success", `{"code":0,"data":{"today_checked_in":false}}`, `{}`, OutcomeFailed, 2},
		{"denied_claim_does_not_leak_message", `{"code":0,"data":{"today_checked_in":false}}`, `{"code":8,"message":"sensitive-upstream-token"}`, OutcomeFailed, 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requests := 0
			ctx := buddyCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || string(body) != "{}" {
					t.Errorf("invalid request method/body: %s %s", r.Method, body)
				}
				switch r.URL.Path {
				case "/v2/billing/meter/checkin-activity-status":
					_, _ = io.WriteString(w, scenario.status)
				case "/v2/billing/meter/daily-checkin":
					_, _ = io.WriteString(w, scenario.claim)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			auth := &coreauth.Auth{Provider: "codebuddy-cn", Metadata: map[string]any{"access_token": "synthetic-token", "uid": "user-a"}}
			result := NewCodeBuddyAdapter().CheckIn(ctx, &config.Config{}, auth, nil, "2026-10-15")
			if result.Outcome != scenario.outcome || requests != scenario.requests {
				t.Fatalf("result=%+v requests=%d", result, requests)
			}
			if strings.Contains(result.Message, "sensitive-upstream-token") {
				t.Fatal("raw response leaked")
			}
		})
	}
}

func TestCodeBuddyCheckinAccountHeadersRemainIsolated(t *testing.T) {
	ctx := buddyCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		if uid != "user-a" && uid != "user-b" {
			t.Errorf("wrong user %q", uid)
		}
		if r.Header.Get("Authorization") != "Bearer token-"+uid || r.Header.Get("X-Enterprise-Id") != "enterprise-"+uid || r.Header.Get("X-Tenant-Id") != "tenant-override-"+uid || r.Header.Get("X-Domain") != "domain-"+uid || r.Header.Get("User-Agent") != "custom-workbuddy" {
			t.Errorf("account headers crossed or overrides lost for %s", uid)
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"today_checked_in":true}}`)
	})
	adapter := NewCodeBuddyAdapter()
	for _, uid := range []string{"user-a", "user-b"} {
		auth := &coreauth.Auth{Provider: "codebuddy-cn", Metadata: map[string]any{"auth": map[string]any{"accessToken": "token-" + uid, "domain": "domain-" + uid}, "account": map[string]any{"uid": uid, "enterpriseId": "enterprise-" + uid}}}
		headers := http.Header{"X-Tenant-Id": {"tenant-override-" + uid}, "User-Agent": {"custom-workbuddy"}, "Authorization": {"untrusted-override"}}
		result := adapter.CheckIn(ctx, nil, auth, headers, "2026-10-15")
		if result.Outcome != OutcomeAlreadyDone {
			t.Fatalf("result=%+v", result)
		}
	}
}

func TestCodeBuddyCheckinJWTSubjectAndRedirectIsolation(t *testing.T) {
	redirectRequests := 0
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectRequests++; w.WriteHeader(200) }))
	defer redirect.Close()
	ctx := buddyCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-Id") != "jwt-user" {
			t.Error("JWT subject not used")
		}
		http.Redirect(w, r, redirect.URL, http.StatusTemporaryRedirect)
	})
	payload, _ := json.Marshal(map[string]string{"sub": "jwt-user"})
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	result := NewCodeBuddyAdapter().CheckIn(ctx, nil, &coreauth.Auth{Provider: "codebuddy-cn", Metadata: map[string]any{"access_token": token, "base_url": "https://copilot.tencent.com/v2"}}, http.Header{"X-Sensitive": {"local-only"}}, "2026-10-15")
	if result.Outcome != OutcomeFailed || redirectRequests != 0 {
		t.Fatalf("redirect followed or success invented: %+v requests=%d", result, redirectRequests)
	}
}
