package airjelly

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSignRequest(t *testing.T) {
	key := strings.Repeat("ab", 32)
	t.Setenv("AIRJELLY_GATEWAY_HMAC_KEY", key)
	req, err := http.NewRequest("POST", Gateway+"/api/v1/llm/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = SignRequest(req, "test-token"); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer test-token" {
		t.Fatal("missing bearer header")
	}
	mac := hmac.New(sha256.New, mustHex(t, key))
	_, _ = mac.Write([]byte(req.Header.Get("X-AirJelly-Timestamp") + req.Header.Get("X-AirJelly-Nonce") + "POST" + req.URL.Path))
	if !hmac.Equal(mustHex(t, req.Header.Get("X-AirJelly-Signature")), mac.Sum(nil)) {
		t.Fatal("signature mismatch")
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshUsesSupabaseGrant(t *testing.T) {
	t.Setenv("AIRJELLY_SUPABASE_ANON_KEY", "public-test-key")
	client := &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/auth/v1/token" || req.URL.Query().Get("grant_type") != "refresh_token" || req.Header.Get("apikey") != "public-test-key" {
			t.Fatalf("unexpected refresh request: %s", req.URL)
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"refresh_token":"old"`) { t.Fatal("refresh token omitted") }
		return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"next","expires_in":3600,"user":{"id":"account"}}`)),Header:make(http.Header)},nil
	})}
	s, err := Refresh(context.Background(),client,"old")
	if err != nil || s.AccessToken != "new" || s.RefreshToken != "next" || s.ExpiresAt <= 0 { t.Fatalf("refresh failed: %#v %v",s,err) }
}

// Runs only when explicitly given the local desktop session and a runtime key.
func TestLiveQuota(t *testing.T) {
	path := os.Getenv("AIRJELLY_TEST_SESSION_FILE")
	if path == "" || os.Getenv("AIRJELLY_GATEWAY_HMAC_KEY") == "" {
		t.Skip("live credentials not configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var container map[string]string
	if err = json.Unmarshal(b, &container); err != nil {
		t.Fatal(err)
	}
	var session Session
	for _, v := range container {
		if err = json.Unmarshal([]byte(v), &session); err != nil {
			t.Fatal(err)
		}
		break
	}
	if session.AccessToken == "" {
		t.Fatal("no access token")
	}
	q, err := ReadQuota(context.Background(), http.DefaultClient, session.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyLimit <= 0 || q.TotalRemaining < 0 {
		t.Fatalf("unexpected quota values: %+v", q)
	}
	t.Logf("plan=%s daily=%.0f/%.0f total=%.0f", q.Plan, q.DailyRemaining, q.DailyLimit, q.TotalRemaining)
}
