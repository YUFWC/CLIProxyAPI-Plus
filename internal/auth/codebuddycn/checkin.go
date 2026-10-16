package codebuddycn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// CheckinCredentials contains the existing account identity, never refreshed here.
type CheckinCredentials struct {
	AccessToken  string
	UID          string
	EnterpriseID string
	Domain       string
	Endpoint     string
}

// CheckinCredentialsFromMetadata supports CLI and desktop auth.info records.
func CheckinCredentialsFromMetadata(metadata map[string]any, attributes map[string]string) CheckinCredentials {
	nested, _ := metadata["auth"].(map[string]any)
	account, _ := metadata["account"].(map[string]any)
	creds := CheckinCredentials{
		AccessToken:  checkinFirst(checkinText(metadata, "access_token", "accessToken"), checkinText(nested, "accessToken", "token"), attributes["api_key"]),
		UID:          checkinFirst(checkinText(metadata, "uid"), checkinText(account, "uid"), attributes["uid"]),
		EnterpriseID: checkinFirst(checkinText(metadata, "enterprise_id", "enterpriseId"), checkinText(account, "enterpriseId"), attributes["enterprise_id"]),
		Domain:       checkinFirst(checkinText(metadata, "domain"), checkinText(nested, "domain")),
		Endpoint:     checkinFirst(checkinText(metadata, "base_url"), attributes["base_url"], APIBaseURL),
	}
	if creds.UID == "" {
		parts := strings.Split(creds.AccessToken, ".")
		if len(parts) == 3 {
			if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); err == nil {
				var claims map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.UseNumber()
				if decoder.Decode(&claims) == nil {
					creds.UID = checkinText(claims, "sub")
				}
			}
		}
	}
	return creds
}

func checkinFirst(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func checkinText(values map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := values[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case json.Number:
			return value.String()
		case float64:
			if value >= 0 && value <= 9007199254740991 && value == float64(int64(value)) {
				return strconv.FormatInt(int64(value), 10)
			}
		}
	}
	return ""
}

// CheckinResult reports only explicit upstream business evidence.
type CheckinResult struct{ AlreadyDone bool }

// CheckinClient is independent of the credential-acquisition client.
type CheckinClient struct{ httpClient *http.Client }

func NewCheckinClient(client *http.Client) *CheckinClient {
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &CheckinClient{httpClient: &copyClient}
}

// CheckIn queries today's state before issuing at most one claim.
func (c *CheckinClient) CheckIn(ctx context.Context, creds CheckinCredentials, headers http.Header) (CheckinResult, error) {
	endpoint, err := url.Parse(strings.TrimRight(creds.Endpoint, "/"))
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Host != "copilot.tencent.com" && endpoint.Host != "staging.codebuddy.cn") || (endpoint.Path != "" && endpoint.Path != "/v2") {
		return CheckinResult{}, errors.New("codebuddy-cn: unsupported checkin endpoint")
	}
	endpoint.Path = ""
	if creds.AccessToken == "" {
		return CheckinResult{}, errors.New("codebuddy-cn: missing access token")
	}
	h := http.Header{"Accept": {"application/json"}, "Content-Type": {"application/json"}, "User-Agent": {"WorkBuddy/5.3.14"}}
	if creds.UID != "" {
		h.Set("X-User-Id", creds.UID)
	}
	if creds.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", creds.EnterpriseID)
		h.Set("X-Tenant-Id", creds.EnterpriseID)
	}
	if creds.Domain != "" {
		h.Set("X-Domain", creds.Domain)
	}
	for key, values := range headers {
		h[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	if strings.TrimSpace(h.Get("X-User-Id")) == "" {
		return CheckinResult{}, errors.New("codebuddy-cn: missing user identity")
	}
	status, err := c.post(ctx, endpoint.String()+"/v2/billing/meter/checkin-activity-status", h)
	if err != nil {
		return CheckinResult{}, err
	}
	if status.Data.TodayCheckedIn == nil {
		return CheckinResult{}, errors.New("codebuddy-cn: status missing today_checked_in")
	}
	if *status.Data.TodayCheckedIn {
		return CheckinResult{AlreadyDone: true}, nil
	}
	_, err = c.post(ctx, endpoint.String()+"/v2/billing/meter/daily-checkin", h)
	return CheckinResult{}, err
}

type checkinPayload struct {
	Code *int `json:"code"`
	Data struct {
		TodayCheckedIn *bool `json:"today_checked_in"`
	} `json:"data"`
}

func (c *CheckinClient) post(ctx context.Context, endpoint string, headers http.Header) (checkinPayload, error) {
	var payload checkinPayload
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return payload, errors.New("codebuddy-cn: invalid checkin request")
	}
	req.Header = headers.Clone()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return payload, errors.New("codebuddy-cn: checkin transport failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("codebuddy-cn: close checkin response failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("codebuddy-cn: checkin HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return payload, errors.New("codebuddy-cn: invalid checkin response")
	}
	if payload.Code == nil {
		return payload, errors.New("codebuddy-cn: checkin missing business code")
	}
	if *payload.Code != 0 {
		return payload, fmt.Errorf("codebuddy-cn: checkin rejected (code %d)", *payload.Code)
	}
	return payload, nil
}
