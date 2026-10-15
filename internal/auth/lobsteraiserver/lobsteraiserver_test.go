package lobsteraiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildLoginURLUsesPortalSurfaceAndHashQuery(t *testing.T) {
	// A backend override that is neither test nor production keeps the lookup
	// away from the live config endpoint, so the portal fallback is exercised.
	client := NewClientWithProxyURL(nil, "", "https://lobsterai-server.youdao.com")
	got := client.BuildLoginURL("state-123", "http://127.0.0.1:9999/auth/callback")
	if !strings.HasPrefix(got, "https://lobsterai.youdao.com/portal#/login?") {
		t.Fatalf("login URL = %q", got)
	}
	// The parameters belong to the hash query, not the regular query.
	if !strings.Contains(got, "state=state-123") {
		t.Fatalf("login URL missing state: %q", got)
	}
	if !strings.Contains(got, "redirect_uri=http%3A%2F%2F127.0.0.1%3A9999%2Fauth%2Fcallback") {
		t.Fatalf("login URL missing encoded redirect_uri: %q", got)
	}
	// The portal only issues the authorization-code redirect for a recognized
	// source; "electron" is the value the desktop client sends.
	if !strings.Contains(got, "source=electron") {
		t.Fatalf("login URL missing electron source: %q", got)
	}
	if strings.Contains(got, "?state=state-123") {
		t.Fatalf("parameters must live in the hash query, not the regular query: %q", got)
	}
}

func TestBuildLoginURLDefaultsRedirectURIWhenCallerHasNoListener(t *testing.T) {
	got := appendLoginParams("https://example.com/login", map[string]string{"state": "abc", "empty": "  "})
	if got != "https://example.com/login?state=abc" {
		t.Fatalf("appendLoginParams = %q", got)
	}
}

func TestIsTestBackend(t *testing.T) {
	if !NewClientWithProxyURL(nil, "", DefaultTestBackendURL).isTestBackend() {
		t.Fatal("inner backend must be detected as test")
	}
	if NewClientWithProxyURL(nil, "", DefaultBackendURL).isTestBackend() {
		t.Fatal("production backend must not be detected as test")
	}
}

func TestExchangeCodeParsesEnvelopeAndKeyfrom(t *testing.T) {
	var sawBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/exchange" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&sawBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{
				"accessToken":  "acc-1",
				"refreshToken": "ref-1",
				"expiresIn":    3600,
				"user":         map[string]any{"id": "u1", "email": "a@b.c", "name": "Ann"},
				"quota":        map[string]any{"subscriptionStatus": "active", "credits": 42},
			},
		})
	}))
	defer server.Close()

	client := NewClientWithProxyURL(&config.Config{}, "", server.URL)
	client.SetKeyfromIdentity("official", "official", "2026.10.13", "uuid-1")
	token, user, quota, errExchange := client.ExchangeCode(context.Background(), "code-1", "state-1")
	if errExchange != nil {
		t.Fatalf("ExchangeCode: %v", errExchange)
	}
	if token.AccessToken != "acc-1" || token.RefreshToken != "ref-1" || token.ExpiresIn != 3600 {
		t.Fatalf("token = %+v", token)
	}
	if token.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt not derived from expiresIn")
	}
	if user == nil || user.ID != "u1" || user.Email != "a@b.c" {
		t.Fatalf("user = %+v", user)
	}
	if quota == nil || quota.SubscriptionStatus != "active" || !quota.CreditsKnown || quota.Credits != 42 {
		t.Fatalf("quota = %+v", quota)
	}
	if sawBody["authCode"] != "code-1" || sawBody["state"] != "state-1" {
		t.Fatalf("exchange body = %+v", sawBody)
	}
	// The upstream requires the attribution payload on login requests.
	if sawBody["firstKeyfrom"] != "official" || sawBody["latestKeyfrom"] != "official" {
		t.Fatalf("exchange body missing keyfrom: %+v", sawBody)
	}
	if sawBody["version"] != "2026.10.13" || sawBody["uuid"] != "uuid-1" {
		t.Fatalf("exchange body missing version/uuid: %+v", sawBody)
	}
}

func TestExchangeCodeErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	if _, _, _, errExchange := client.ExchangeCode(context.Background(), "bad", "state"); errExchange == nil {
		t.Fatal("expected error for non-2xx exchange")
	}
}

func TestExchangeCodeSurfacesNonZeroEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 40100, "message": "auth code expired"})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	_, _, _, errExchange := client.ExchangeCode(context.Background(), "expired", "state")
	if errExchange == nil || !strings.Contains(errExchange.Error(), "40100") {
		t.Fatalf("expected backend error code surfaced, got %v", errExchange)
	}
}

func TestRefreshPreservesTokenWhenOmitted(t *testing.T) {
	var sawBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&sawBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"accessToken": "acc-2", "expiresIn": 1800},
		})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	token, errRefresh := client.Refresh(context.Background(), "ref-old")
	if errRefresh != nil {
		t.Fatalf("Refresh: %v", errRefresh)
	}
	if token.AccessToken != "acc-2" {
		t.Fatalf("access token = %q", token.AccessToken)
	}
	if token.RefreshToken != "ref-old" {
		t.Fatalf("refresh token = %q, want the preserved previous token", token.RefreshToken)
	}
	if sawBody["refreshToken"] != "ref-old" {
		t.Fatalf("refresh body = %+v", sawBody)
	}
	if sawBody["version"] == nil {
		t.Fatalf("refresh body missing keyfrom version: %+v", sawBody)
	}
}

func TestFetchModelsFiltersInaccessibleAndParsesIdentity(t *testing.T) {
	var sawAuth, sawCaps, sawVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/available" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		sawCaps = r.Header.Get("X-LobsterAI-Client-Capabilities")
		sawVersion = r.Header.Get("X-LobsterAI-Client-Version")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": []any{
				map[string]any{"modelId": "deepseek-v4-flash", "modelName": "DeepSeek V4 Flash", "apiFormat": "openai", "supportsThinking": true, "accessible": true},
				map[string]any{"modelId": "deepseek-v4-pro", "modelName": "DeepSeek V4 Pro", "apiFormat": "openai", "supportsImage": true, "contextWindow": 200000, "accessible": true},
				map[string]any{"modelId": "kimi-k3", "apiFormat": "openai", "accessible": false},
				map[string]any{"modelId": "deepseek-v4-flash", "apiFormat": "openai", "accessible": true},
			},
		})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	models, errModels := client.FetchModels(context.Background(), "acc-1")
	if errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	// The inaccessible kimi-k3 entry is dropped, and the duplicate collapses.
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 after entitlement filter and dedupe", models)
	}
	if models[0].ID != "deepseek-v4-flash" || models[0].DisplayName != "DeepSeek V4 Flash" || !models[0].Thinking {
		t.Fatalf("models[0] = %+v", models[0])
	}
	if models[1].ID != "deepseek-v4-pro" || models[1].APIFormat != "openai" || !models[1].Image || models[1].ContextLimit != 200000 {
		t.Fatalf("models[1] = %+v", models[1])
	}
	if sawAuth != "Bearer acc-1" {
		t.Fatalf("Authorization = %q", sawAuth)
	}
	if !strings.Contains(sawCaps, "kimi-k3-agentic-v1") {
		t.Fatalf("capabilities header = %q", sawCaps)
	}
	if sawVersion == "" {
		t.Fatal("client version header missing")
	}
}

func TestFetchQuotaHonestAboutUnknownCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"subscriptionStatus": "free", "planName": "免费"},
		})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	quota, errQuota := client.FetchQuota(context.Background(), "acc-1")
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	if quota.SubscriptionStatus != "free" {
		t.Fatalf("subscription = %q", quota.SubscriptionStatus)
	}
	if quota.CreditsKnown {
		t.Fatalf("credits must stay unknown when the backend omits them: %+v", quota)
	}
}

func TestFetchActivitySnapshotReadsSlotAndContext(t *testing.T) {
	var sawPlacement, sawContainerVersion, sawPlatform string
	var contextCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/client-activities/slot":
			sawPlacement = r.URL.Query().Get("placement")
			sawContainerVersion = r.URL.Query().Get("containerApiVersion")
			sawPlatform = r.URL.Query().Get("platform")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"slotState": "available",
					"activity":  map[string]any{"activityCode": "daily_check_in", "configRevision": 7},
				},
			})
		case r.URL.Path == "/api/client-activities/daily_check_in/context":
			contextCalled = true
			if got := r.URL.Query().Get("configRevision"); got != "7" {
				t.Errorf("configRevision = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"dailyCheckInRewardPerDay": 20, "dailyCheckInProgress": "1/7"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	snapshot, errSnapshot := client.FetchActivitySnapshot(context.Background(), "acc-1", ActivityPlacementDesktopSidebar, ActivityContainerAPIVersionDailyCheckIn)
	if errSnapshot != nil {
		t.Fatalf("FetchActivitySnapshot: %v", errSnapshot)
	}
	if !contextCalled {
		t.Fatal("activity context was not read")
	}
	if snapshot.SlotState != "available" || snapshot.ActivityCode != "daily_check_in" || snapshot.ConfigRevision != 7 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Context["dailyCheckInRewardPerDay"] != float64(20) {
		t.Fatalf("context = %+v", snapshot.Context)
	}
	if sawPlacement != ActivityPlacementDesktopSidebar {
		t.Fatalf("placement = %q", sawPlacement)
	}
	if sawContainerVersion != "2" {
		t.Fatalf("containerApiVersion = %q", sawContainerVersion)
	}
	// The backend keys activity availability on a real electron platform token;
	// an unrecognized value (e.g. "cli") silently yields an empty slot.
	if sawPlatform == "" || sawPlatform == "cli" {
		t.Fatalf("platform = %q, want a desktop platform token", sawPlatform)
	}
}

func TestFetchActivitySnapshotSkipsEmptySlot(t *testing.T) {
	var contextCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/context") {
			contextCalled = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"slotState": "empty"}})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	snapshot, errSnapshot := client.FetchActivitySnapshot(context.Background(), "acc-1", ActivityPlacementDesktopSidebar, 0)
	if errSnapshot != nil {
		t.Fatalf("FetchActivitySnapshot: %v", errSnapshot)
	}
	if snapshot.SlotState != "empty" || snapshot.ActivityCode != "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if contextCalled {
		t.Fatal("context must not be read for an empty slot")
	}
}

func TestFetchQuotaMapsLiveCreditFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"subscriptionStatus":   "free",
				"planName":             "免费",
				"freeCreditsTotal":     300,
				"freeCreditsRemaining": 299.5,
				"freeCreditsUsed":      0.5,
			},
		})
	}))
	defer server.Close()
	client := NewClientWithProxyURL(nil, "", server.URL)
	quota, errQuota := client.FetchQuota(context.Background(), "acc-1")
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	if quota.SubscriptionStatus != "free" {
		t.Fatalf("subscription = %q", quota.SubscriptionStatus)
	}
	if !quota.CreditsKnown || quota.Credits != 299 {
		t.Fatalf("credits = %d known=%v, want 299/true", quota.Credits, quota.CreditsKnown)
	}
}

func TestParseUserAcceptsNumericIDAndYid(t *testing.T) {
	// /api/user/profile reports the id as a NUMBER and the login identifier as
	// "yid"; a string-only reader silently dropped the whole identity.
	user := parseUser(map[string]any{
		"yid":      "urs-phoneyd.bc69332f76b2498aa@163.com",
		"nickname": "130****2189",
		"id":       float64(123318),
	})
	if user.ID != "123318" {
		t.Fatalf("id = %q, want 123318", user.ID)
	}
	if user.Email != "urs-phoneyd.bc69332f76b2498aa@163.com" {
		t.Fatalf("email = %q", user.Email)
	}
	if user.Name != "130****2189" {
		t.Fatalf("name = %q", user.Name)
	}
}
