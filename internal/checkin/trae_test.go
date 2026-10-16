package checkin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type traeCheckinTransport struct {
	target *url.URL
	base   http.RoundTripper
	t      *testing.T
}

func (tr traeCheckinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.trae.cn" {
		tr.t.Errorf("unexpected business host %s", req.URL.Host)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = tr.target.Scheme, tr.target.Host
	clone.Host = tr.target.Host
	return tr.base.RoundTrip(clone)
}
func traeCheckinContext(t *testing.T, handler http.HandlerFunc) context.Context {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	return context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(traeCheckinTransport{target, server.Client().Transport, t}))
}

func TestTraeCheckinVerifiesClaimEvidence(t *testing.T) {
	day := time.Now().In(time.FixedZone("Beijing", 8*60*60)).Format("20060102")
	pack := `{"code":0,"data":{"user_entitlement_pack_list":[{"entitlement_base_info":{"entitlement_id":"checkin_` + day + `_user-a"}}]}}`
	for _, scenario := range []struct {
		name, initial, claim, verified, usage, outcome string
		requests                                       int
	}{
		{"status_proves_already_checked", `{"code":0,"data":{"enable":true,"checked_in":true}}`, "", "", "", OutcomeAlreadyDone, 1},
		{"disabled_is_not_completed", `{"enable":false,"checked_in":false}`, "", "", "", OutcomeUnavailable, 1},
		{"denied_initial_status", `{"code":9074,"data":{"enable":true,"checked_in":true}}`, "", "", "", OutcomeFailed, 1},
		{"missing_enable_is_not_eligible", `{"code":0,"checked_in":false}`, "", "", "", OutcomeFailed, 1},
		{"missing_checked_in_is_not_success", `{"code":0,"enable":true}`, "", "", "", OutcomeFailed, 1},
		{"claim_then_checked_in", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"code":0,"data":{"checked_in":true}}`, "", OutcomeSuccess, 3},
		{"claim_then_same_day_pack", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"checked_in":false}`, pack, OutcomeSuccess, 4},
		{"device_already_with_same_day_pack", `{"enable":true,"checked_in":false}`, `{"code":9095}`, `{"checked_in":false}`, pack, OutcomeAlreadyDone, 4},
		{"device_already_alone_is_not_success", `{"enable":true,"checked_in":false}`, `{"code":9095}`, `{"checked_in":false}`, `{"code":0,"data":{}}`, OutcomeFailed, 4},
		{"accepted_claim_alone_is_not_success", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"checked_in":false}`, `{"code":0,"data":{}}`, OutcomeFailed, 4},
		{"other_account_pack_is_not_success", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"checked_in":false}`, strings.ReplaceAll(pack, "user-a", "user-b"), OutcomeFailed, 4},
		{"old_pack_is_not_success", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"checked_in":false}`, strings.ReplaceAll(pack, day, "20000101"), OutcomeFailed, 4},
		{"denied_usage_even_with_pack", `{"enable":true,"checked_in":false}`, `{"code":0}`, `{"code":9,"checked_in":true}`, strings.Replace(pack, `"code":0`, `"code":9`, 1), OutcomeFailed, 4},
		{"9074_never_retries_or_replaces_device", `{"enable":true,"checked_in":false}`, `{"code":9074,"message":"sensitive-upstream-token"}`, "", "", OutcomeFailed, 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requests, statuses := 0, 0
			ctx := traeCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Cloud-IDE-JWT synthetic-token" || r.Header.Get("X-Device-Id") != "1234567890123456" {
					t.Error("invalid auth, method or device identity")
				}
				body, _ := io.ReadAll(r.Body)
				response := ""
				switch r.URL.Path {
				case "/trae/api/v2/ug/checkin_credits/status":
					statuses++
					response = scenario.initial
					if statuses > 1 {
						response = scenario.verified
					}
					if string(body) != "{}" {
						t.Error("wrong status payload")
					}
				case "/trae/api/v2/ug/checkin_credits/claim":
					response = scenario.claim
					if string(body) != "{}" {
						t.Error("wrong claim payload")
					}
				case "/trae/api/v2/pay/ide_user_ent_usage":
					response = scenario.usage
					var payload struct {
						RequireUsage bool `json:"require_usage"`
						ReqSource    int  `json:"req_source"`
					}
					if json.Unmarshal(body, &payload) != nil || !payload.RequireUsage || payload.ReqSource != 2 {
						t.Error("wrong usage payload")
					}
				default:
					t.Errorf("unexpected protocol path %s", r.URL.Path)
					w.WriteHeader(404)
				}
				_, _ = io.WriteString(w, response)
			})
			auth := &coreauth.Auth{Provider: "trae", Metadata: map[string]any{"access_token": "synthetic-token", "uid": "user-a", "device_id": "1234567890123456", "api_host": "https://api.trae.com.cn", "base_url": "https://trae-api-cn.mchost.guru"}}
			result := NewTraeAdapter().CheckIn(ctx, nil, auth, nil, "2026-10-15")
			if result.Outcome != scenario.outcome || requests != scenario.requests {
				t.Fatalf("result=%+v requests=%d", result, requests)
			}
			if strings.Contains(result.Message, "sensitive-upstream-token") {
				t.Fatal("raw response leaked")
			}
		})
	}
}

func TestTraeCheckinAccountHeadersAndDeviceOverridesRemainIsolated(t *testing.T) {
	ctx := traeCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-Account")
		if uid != "user-a" && uid != "user-b" {
			t.Errorf("wrong account %q", uid)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT token-"+uid || r.Header.Get("X-Device-Id") != "1234567890123456" || r.Header.Get("X-Device-Brand") != "brand-"+uid || r.Header.Get("X-App-Version") != "custom-version" {
			t.Errorf("lost or crossed account business headers for %s", uid)
		}
		_, _ = io.WriteString(w, `{"enable":true,"checked_in":true}`)
	})
	adapter := NewTraeAdapter()
	for _, uid := range []string{"user-a", "user-b"} {
		auth := &coreauth.Auth{Provider: "trae", Metadata: map[string]any{"auth": map[string]any{"accessToken": "token-" + uid, "deviceId": "hex-device-never-replaced"}, "account": map[string]any{"uid": uid}}}
		headers := http.Header{"X-Account": {uid}, "X-Device-Id": {"1234567890123456"}, "X-Device-Brand": {"brand-" + uid}, "X-App-Version": {"custom-version"}, "Authorization": {"untrusted-override"}}
		result := adapter.CheckIn(ctx, nil, auth, headers, "2026-10-15")
		if result.Outcome != OutcomeAlreadyDone {
			t.Fatalf("result=%+v", result)
		}
		if auth.Metadata["auth"].(map[string]any)["deviceId"] != "hex-device-never-replaced" {
			t.Fatal("device metadata was mutated")
		}
	}
}

func TestTraeCheckinBlocksMissingOrIncompatibleDevice(t *testing.T) {
	for _, device := range []string{"", "0123456789abcdef0123456789abcdef"} {
		auth := &coreauth.Auth{Provider: "trae", Metadata: map[string]any{"access_token": "synthetic-token", "device_id": device}}
		result := NewTraeAdapter().CheckIn(context.Background(), nil, auth, nil, "2026-10-15")
		if result.Outcome != OutcomeBlocked {
			t.Fatalf("missing or incompatible device was not blocked: %+v", result)
		}
	}
}

func TestTraeCheckinNeverFollowsRedirect(t *testing.T) {
	requests := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer destination.Close()
	ctx := traeCheckinContext(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	result := NewTraeAdapter().CheckIn(ctx, nil, &coreauth.Auth{Provider: "trae", Metadata: map[string]any{"access_token": "synthetic-token", "device_id": "1234567890123456"}}, http.Header{"X-Sensitive": {"local-only"}}, "2026-10-15")
	if result.Outcome != OutcomeFailed || requests != 0 {
		t.Fatalf("redirect followed or success invented: %+v requests=%d", result, requests)
	}
}
