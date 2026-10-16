package trae

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const checkinHost = "https://api.trae.cn"

// CheckinCredentials reuses existing lawful device and account metadata.
type CheckinCredentials struct {
	AccessToken  string
	UID          string
	EnterpriseID string
	DeviceID     string
	DeviceBrand  string
	DeviceType   string
	OSVersion    string
	AppVersion   string
}

// CheckinCredentialsFromMetadata accepts CLI, flat desktop, and nested records.
func CheckinCredentialsFromMetadata(metadata map[string]any, attributes map[string]string) CheckinCredentials {
	nested, _ := metadata["auth"].(map[string]any)
	account, _ := metadata["account"].(map[string]any)
	creds := CheckinCredentials{
		AccessToken:  checkinFirst(checkinText(metadata, "access_token", "accessToken", "token"), checkinText(nested, "accessToken", "access_token", "token"), attributes["api_key"]),
		UID:          checkinFirst(checkinText(metadata, "uid"), checkinText(account, "uid"), checkinText(nested, "uid"), attributes["uid"]),
		EnterpriseID: checkinFirst(checkinText(metadata, "enterprise_id", "enterpriseId"), checkinText(account, "enterpriseId"), checkinText(nested, "enterprise_id", "enterpriseId"), attributes["enterprise_id"]),
		DeviceID:     checkinFirst(checkinText(metadata, "device_id", "deviceId"), checkinText(nested, "device_id", "deviceId"), attributes["device_id"]),
		DeviceBrand:  checkinFirst(checkinText(metadata, "device_brand", "deviceBrand"), checkinText(nested, "device_brand", "deviceBrand")),
		DeviceType:   checkinFirst(checkinText(metadata, "device_type", "deviceType"), checkinText(nested, "device_type", "deviceType")),
		OSVersion:    checkinFirst(checkinText(metadata, "os_version", "osVersion"), checkinText(nested, "os_version", "osVersion")),
		AppVersion:   checkinFirst(checkinText(metadata, "app_version", "appVersion"), checkinText(nested, "app_version", "appVersion"), "0.1.62"),
	}
	if creds.UID == "" {
		parts := strings.Split(creds.AccessToken, ".")
		if len(parts) == 3 {
			if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "=")); err == nil {
				var claims map[string]any
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.UseNumber()
				if decoder.Decode(&claims) == nil {
					creds.UID = checkinText(claims, "uid", "user_id", "sub")
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

// CheckinHeaders applies ordinary business overrides before checking device identity.
func CheckinHeaders(creds CheckinCredentials, custom http.Header) (http.Header, error) {
	h := http.Header{"Accept": {"application/json"}, "Content-Type": {"application/json"}}
	for key, value := range map[string]string{
		"X-Device-Id": creds.DeviceID, "X-Device-Brand": creds.DeviceBrand,
		"X-Device-Type": creds.DeviceType, "X-Os-Version": creds.OSVersion, "X-App-Version": creds.AppVersion,
	} {
		if value != "" {
			h.Set(key, value)
		}
	}
	for key, values := range custom {
		h[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	h.Set("Authorization", "Cloud-IDE-JWT "+creds.AccessToken)
	if creds.AccessToken == "" {
		return nil, errors.New("trae: missing access token")
	}
	did := strings.TrimSpace(h.Get("X-Device-Id"))
	if len(did) < 12 || len(did) > 20 {
		return nil, errors.New("trae: registered numeric device ID required")
	}
	for _, char := range did {
		if char < '0' || char > '9' {
			return nil, errors.New("trae: registered numeric device ID required")
		}
	}
	return h, nil
}

// CheckinResult is completed only after positive status or entitlement evidence.
type CheckinResult struct {
	AlreadyDone bool
	Unavailable bool
	Date        string
}

// CheckinClient never calls OAuth, refresh, inference, or arbitrary metadata hosts.
type CheckinClient struct {
	httpClient *http.Client
	now        func() time.Time
}

func NewCheckinClient(client *http.Client) *CheckinClient {
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &CheckinClient{httpClient: &copyClient, now: time.Now}
}

// CheckIn makes one claim at most. Device denial and throttling never rotate a DID.
func (c *CheckinClient) CheckIn(ctx context.Context, creds CheckinCredentials, custom http.Header) (CheckinResult, error) {
	headers, err := CheckinHeaders(creds, custom)
	if err != nil {
		return CheckinResult{}, err
	}
	status, err := c.post(ctx, "/trae/api/v2/ug/checkin_credits/status", "{}", headers)
	if err != nil {
		return CheckinResult{}, err
	}
	if err = status.accept(0); err != nil {
		return CheckinResult{}, err
	}
	data := status.data()
	if data.Enable == nil || data.CheckedIn == nil {
		return CheckinResult{}, errors.New("trae: status missing enable or checked_in")
	}
	if *data.CheckedIn {
		return CheckinResult{AlreadyDone: true}, nil
	}
	if !*data.Enable {
		return CheckinResult{Unavailable: true}, nil
	}
	claim, err := c.post(ctx, "/trae/api/v2/ug/checkin_credits/claim", "{}", headers)
	if err != nil {
		return CheckinResult{}, err
	}
	if err = claim.accept(0, 9095); err != nil {
		return CheckinResult{}, err
	}
	verified, statusErr := c.post(ctx, "/trae/api/v2/ug/checkin_credits/status", "{}", headers)
	if statusErr == nil && verified.accept(0) == nil && verified.data().CheckedIn != nil && *verified.data().CheckedIn {
		return CheckinResult{AlreadyDone: claim.Code != nil && *claim.Code == 9095}, nil
	}
	usage, err := c.post(ctx, "/trae/api/v2/pay/ide_user_ent_usage", `{"require_usage":true,"req_source":2}`, headers)
	if err != nil {
		return CheckinResult{}, err
	}
	if err = usage.accept(0); err != nil {
		return CheckinResult{}, err
	}
	// Entitlement identifiers use the upstream Beijing calendar, not scheduler TZ.
	day := c.now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	if creds.UID == "" {
		return CheckinResult{}, errors.New("trae: missing account identity for entitlement verification")
	}
	expected := "checkin_" + day.Format("20060102") + "_" + creds.UID
	for _, pack := range usage.data().Packs {
		if pack.Base.ID == expected {
			return CheckinResult{AlreadyDone: claim.Code != nil && *claim.Code == 9095, Date: day.Format("2006-01-02")}, nil
		}
	}
	return CheckinResult{}, errors.New("trae: claim not confirmed by today's status or entitlement")
}

type checkinData struct {
	Enable    *bool `json:"enable"`
	CheckedIn *bool `json:"checked_in"`
	Packs     []struct {
		Base struct {
			ID string `json:"entitlement_id"`
		} `json:"entitlement_base_info"`
	} `json:"user_entitlement_pack_list"`
}

type checkinPayload struct {
	Code *int         `json:"code"`
	Data *checkinData `json:"data"`
	checkinData
}

func (p checkinPayload) data() checkinData {
	if p.Data != nil {
		return *p.Data
	}
	return p.checkinData
}
func (p checkinPayload) accept(codes ...int) error {
	// Some verified desktop responses omit code; completion still requires explicit evidence.
	if p.Code == nil {
		return nil
	}
	for _, code := range codes {
		if *p.Code == code {
			return nil
		}
	}
	return fmt.Errorf("trae: checkin rejected (code %d)", *p.Code)
}

func (c *CheckinClient) post(ctx context.Context, path, body string, headers http.Header) (checkinPayload, error) {
	var payload checkinPayload
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinHost+path, strings.NewReader(body))
	if err != nil {
		return payload, errors.New("trae: invalid checkin request")
	}
	req.Header = headers.Clone()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return payload, errors.New("trae: checkin transport failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("trae: close checkin response failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("trae: checkin HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return payload, errors.New("trae: invalid checkin response")
	}
	return payload, nil
}
