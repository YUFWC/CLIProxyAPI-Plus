package airjelly

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	Provider  = "airjelly"
	Model     = "airjelly-lite"
	Gateway   = "https://airjelly-server-v2.up.railway.app"
	Supabase  = "https://cmxoaabrzxxchikydykw.supabase.co"
	Callback  = "http://localhost:19877"
	UserAgent = "AirJelly/2.0.10 (win32)"
)

type Session struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

type Quota struct {
	Plan               string  `json:"plan"`
	Status             string  `json:"status"`
	DailyRemaining     float64 `json:"dailyRemaining"`
	DailyUsed          float64 `json:"dailyUsed"`
	DailyLimit         float64 `json:"dailyLimit"`
	MonthlyRemaining   float64 `json:"monthlyRemaining"`
	MonthlyLimit       float64 `json:"monthlyLimit"`
	MonthlyPeriodEnd   string  `json:"monthlyPeriodEnd"`
	PermanentRemaining float64 `json:"permanentRemaining"`
	TotalRemaining     float64 `json:"totalRemaining"`
}

func RandomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func GoogleURL(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	v := url.Values{}
	v.Set("provider", "google")
	v.Set("redirect_to", Callback)
	v.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	v.Set("code_challenge_method", "s256")
	return Supabase + "/auth/v1/authorize?" + v.Encode()
}

func token(ctx context.Context, client *http.Client, grant string, payload any) (*Session, error) {
	key := strings.TrimSpace(os.Getenv("AIRJELLY_SUPABASE_ANON_KEY"))
	if key == "" {
		return nil, errors.New("AIRJELLY_SUPABASE_ANON_KEY is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Supabase+"/auth/v1/token?grant_type="+url.QueryEscape(grant), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airjelly auth: HTTP %d: %s", resp.StatusCode, truncate(b))
	}
	var s Session
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.AccessToken == "" || s.RefreshToken == "" || s.User.ID == "" {
		return nil, errors.New("airjelly auth: incomplete session")
	}
	if s.ExpiresAt == 0 && s.ExpiresIn > 0 {
		s.ExpiresAt = time.Now().Unix() + s.ExpiresIn
	}
	return &s, nil
}

func LoginPassword(ctx context.Context, client *http.Client, email, password string) (*Session, error) {
	return token(ctx, client, "password", map[string]string{"email": email, "password": password})
}

func ExchangeCode(ctx context.Context, client *http.Client, code, verifier string) (*Session, error) {
	return token(ctx, client, "pkce", map[string]string{"auth_code": code, "code_verifier": verifier})
}

func Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Session, error) {
	return token(ctx, client, "refresh_token", map[string]string{"refresh_token": refreshToken})
}

// SignRequest signs the exact upstream method/path. The gateway key is supplied
// at runtime; it is never persisted in an auth file or committed to source.
func SignRequest(req *http.Request, accessToken string) error {
	if req == nil || req.URL == nil {
		return errors.New("airjelly: nil request")
	}
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, "airjelly-server-v2.up.railway.app") {
		return errors.New("airjelly: unexpected gateway origin")
	}
	keyString := strings.TrimSpace(os.Getenv("AIRJELLY_GATEWAY_HMAC_KEY"))
	if keyString == "" {
		return errors.New("AIRJELLY_GATEWAY_HMAC_KEY is required")
	}
	key, err := hex.DecodeString(keyString)
	if err != nil {
		return errors.New("AIRJELLY_GATEWAY_HMAC_KEY must be hex")
	}
	if len(key) < 16 {
		return errors.New("AIRJELLY_GATEWAY_HMAC_KEY is too short")
	}
	if accessToken == "" {
		return errors.New("airjelly: missing access token")
	}
	nonceBytes := make([]byte, 16)
	_, err = rand.Read(nonceBytes)
	if err != nil {
		return err
	}
	nonceBytes[6] = (nonceBytes[6] & 0x0f) | 0x40
	nonceBytes[8] = (nonceBytes[8] & 0x3f) | 0x80
	nonce := fmt.Sprintf("%x-%x-%x-%x-%x", nonceBytes[:4], nonceBytes[4:6], nonceBytes[6:8], nonceBytes[8:10], nonceBytes[10:])
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(ts + nonce + strings.ToUpper(req.Method) + req.URL.Path))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-AirJelly-Timestamp", ts)
	req.Header.Set("X-AirJelly-Nonce", nonce)
	req.Header.Set("X-AirJelly-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("x-ai-feature", "user")
	return nil
}

func ReadQuota(ctx context.Context, client *http.Client, accessToken string) (*Quota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Gateway+"/api/stripe/subscription", nil)
	if err != nil {
		return nil, err
	}
	if err = SignRequest(req, accessToken); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airjelly quota: HTTP %d: %s", resp.StatusCode, truncate(b))
	}
	var envelope struct {
		Plan   string `json:"plan"`
		Status string `json:"status"`
		Quota  Quota  `json:"quota"`
	}
	if err = json.Unmarshal(b, &envelope); err != nil {
		return nil, err
	}
	envelope.Quota.Plan = envelope.Plan
	envelope.Quota.Status = envelope.Status
	return &envelope.Quota, nil
}

func truncate(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return string(b)
}
