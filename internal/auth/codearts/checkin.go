package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewCheckInClient creates an isolated business client. Headers apply only to
// check-in steps, before signing, never to OAuth, identity or catalog requests.
// Cancellation is controlled by the caller's context rather than a total timeout.
func NewCheckInClient(httpClient *http.Client, headers http.Header) *Client {
	client := &http.Client{}
	if httpClient != nil {
		*client = *httpClient
	}
	client.Timeout = 0
	for name, values := range headers {
		if !strings.EqualFold(name, "Accept-Encoding") || strings.Join(values, ",") != "" {
			continue
		}
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if standard, ok := transport.(*http.Transport); ok {
			// Automatic gzip would append a second value after the empty value was signed.
			cloned := standard.Clone()
			cloned.DisableCompression = true
			client.Transport = cloned
		}
		break
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return fmt.Errorf("codearts: check-in redirects are disabled")
	}
	return &Client{
		httpClient:     client,
		stsHost:        IAMSTSHost,
		portalHost:     PortalHost,
		snapHost:       SnapEngineURL,
		benefitURL:     BenefitAPIURL,
		checkinHeaders: headers.Clone(),
	}
}

func (c *Client) checkInBusinessHeaders(defaults map[string]string) map[string]string {
	headers := make(http.Header, len(defaults)+len(c.checkinHeaders))
	for key, value := range defaults {
		headers.Set(key, value)
	}
	for key, values := range c.checkinHeaders {
		switch strings.ToLower(key) {
		case "authorization", "host", "content-length", "connection", "proxy-connection",
			"keep-alive", "transfer-encoding", "te", "trailer", "upgrade",
			"proxy-authorization", "proxy-authenticate", "x-sdk-date", "x-security-token",
			"x-sdk-content-sha256":
			continue
		}
		headers.Set(key, strings.Join(values, ","))
	}
	merged := make(map[string]string, len(headers))
	for key, values := range headers {
		merged[key] = strings.Join(values, ",")
	}
	return merged
}

// WelfareStatus is the state of one welfare activity.
type WelfareStatus string

const (
	// WelfareEligible marks an activity that can be claimed now.
	WelfareEligible WelfareStatus = "ELIGIBLE"
	// WelfareClaimed marks an activity already claimed.
	WelfareClaimed WelfareStatus = "CLAIMED"
	// WelfareConfirmed marks an activity already confirmed.
	WelfareConfirmed WelfareStatus = "CONFIRMED"
	// WelfareConsumed marks an activity already consumed.
	WelfareConsumed WelfareStatus = "CONSUMED"
)

// flexString accepts a JSON string or number and exposes it as a string.
//
// The welfare API is inconsistent: campaignId is a number in
// GET /v1/ops/delivery (campaignId: 1) but the schema reads like a string. A
// plain `string` field makes encoding/json reject the whole response with an
// UnmarshalTypeError, which silently dropped the primary check-in path.
type flexString string

// UnmarshalJSON accepts a JSON string, number, or null.
func (f *flexString) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var decoded string
		if errUnmarshal := json.Unmarshal(data, &decoded); errUnmarshal != nil {
			return errUnmarshal
		}
		*f = flexString(decoded)
		return nil
	}
	var number json.Number
	if errUnmarshal := json.Unmarshal(data, &number); errUnmarshal != nil {
		return fmt.Errorf("codearts: identifier must be a string or number: %w", errUnmarshal)
	}
	*f = flexString(number)
	return nil
}

// String returns the underlying value.
func (f flexString) String() string { return strings.TrimSpace(string(f)) }

// Activity is one entry of the welfare activity list.
type Activity struct {
	// CampaignID identifies the activity.
	CampaignID string
	// Type is the activity kind: daily_claim, invite_user, student_certified, user_login.
	Type string
	// TriggerEvent is the upstream extra.triggerEvent value (e.g. user.login).
	// It is the most reliable signal for identifying the daily check-in:
	// the real daily activity is typed USER_LOGIN, not DAILY_CLAIM.
	TriggerEvent string
	// Title is the localized activity title.
	Title string
	// Description is the localized activity description.
	Description string
	// BenefitAmount is the credit granted by claiming.
	BenefitAmount int
	// Claimable reports whether the activity can be claimed.
	Claimable bool
	// Status is the raw upstream status.
	Status WelfareStatus
}

// IsDailyCheckIn reports whether the activity is the daily check-in.
//
// Observed in production (2026-09): the daily activity is type USER_LOGIN with
// extra.triggerEvent == "user.login", title "每日签到领1000 积分". Matching only
// on DAILY_CLAIM therefore never finds it, so all three signals are accepted.
func (a Activity) IsDailyCheckIn() bool {
	switch strings.ToLower(strings.TrimSpace(a.TriggerEvent)) {
	case "user.login":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(a.Type)) {
	case "daily_claim", "user_login", "login":
		return true
	}
	return strings.Contains(a.Title, "每日签到")
}

// ClaimableNow reports whether a daily check-in is available right now.
func (a Activity) ClaimableNow() bool {
	if !a.IsDailyCheckIn() {
		return false
	}
	if !a.Claimable {
		return false
	}
	switch a.Status {
	case WelfareClaimed, WelfareConfirmed, WelfareConsumed:
		return false
	}
	return true
}

// CheckInResult is the outcome of a check-in attempt.
type CheckInResult struct {
	// Claimed reports whether a claim was performed in this call.
	Claimed bool
	// AlreadyClaimed reports whether the activity was already claimed today.
	AlreadyClaimed bool
	// CampaignID is the claimed activity identifier.
	CampaignID string
	// BenefitAmount is the credits granted.
	BenefitAmount int
	// UserBenefitID is the identifier returned by the claim call.
	UserBenefitID string
	// ConfirmError is set when the best-effort confirm call failed after a
	// successful claim. Credits are already granted at claim time, so this is
	// informational only.
	ConfirmError string
}

// welfareDeliveryResponse mirrors GET /v1/ops/delivery?channel=DESKTOP.
type welfareDeliveryResponse struct {
	Code    *int   `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Items []struct {
			CampaignID    flexString      `json:"campaignId"`
			CampaignIDAlt flexString      `json:"campaign_id"`
			Type          string          `json:"type"`
			Title         string          `json:"title"`
			Description   string          `json:"description"`
			BenefitAmount int             `json:"benefitAmount"`
			Claimable     *bool           `json:"claimable"`
			Status        string          `json:"status"`
			DisplayConfig json.RawMessage `json:"displayConfig"`
			Extra         struct {
				TriggerEvent string `json:"triggerEvent"`
			} `json:"extra"`
		} `json:"items"`
	} `json:"data"`
}

// ListActivities fetches the welfare activity list.
func (c *Client) ListActivities(ctx context.Context, creds Credentials) ([]Activity, error) {
	endpoint := strings.TrimRight(c.snapHost, "/") + WelfareDeliveryPath + "?channel=DESKTOP"
	raw, errFetch := c.signedGetJSON(ctx, endpoint, creds, c.checkInBusinessHeaders(map[string]string{
		"Agent-Type": AgentTypePromptCenter,
		"X-Language": "zh-cn",
	}))
	if errFetch != nil {
		return nil, errFetch
	}

	var parsed welfareDeliveryResponse
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("codearts: invalid welfare delivery response: %w", errUnmarshal)
	}
	if parsed.Code != nil && *parsed.Code != 0 && *parsed.Code != 200 {
		return nil, &Error{StatusCode: http.StatusOK, Message: firstNonEmpty(parsed.Message, fmt.Sprintf("code=%d", *parsed.Code))}
	}

	out := make([]Activity, 0, len(parsed.Data.Items))
	for _, item := range parsed.Data.Items {
		claimable := true
		if item.Claimable != nil {
			claimable = *item.Claimable
		}
		out = append(out, Activity{
			CampaignID:    firstNonEmpty(item.CampaignID.String(), item.CampaignIDAlt.String()),
			Type:          normalizeActivityType(item.Type),
			TriggerEvent:  strings.TrimSpace(item.Extra.TriggerEvent),
			Title:         strings.TrimSpace(item.Title),
			Description:   strings.TrimSpace(item.Description),
			BenefitAmount: item.BenefitAmount,
			Claimable:     claimable,
			Status:        WelfareStatus(strings.ToUpper(strings.TrimSpace(item.Status))),
		})
	}
	return out, nil
}

// ClaimActivity claims one welfare activity and confirms it.
func (c *Client) ClaimActivity(ctx context.Context, creds Credentials, campaignID string) (*CheckInResult, error) {
	campaignID = strings.TrimSpace(campaignID)
	if campaignID == "" {
		return nil, fmt.Errorf("codearts: campaign id is required")
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"campaignId":    campaignID,
		"idempotentKey": fmt.Sprintf("claim_%s_%d", campaignID, time.Now().UnixMilli()),
		"channel":       "DESKTOP",
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("codearts: encode claim request: %w", errMarshal)
	}

	endpoint := strings.TrimRight(c.snapHost, "/") + WelfareClaimPath
	raw, errClaim := c.signedPostJSON(ctx, endpoint, creds, payload, c.checkInBusinessHeaders(map[string]string{
		"Agent-Type": AgentTypePromptCenter,
		"X-Language": "zh-cn",
	}))
	if errClaim != nil {
		return nil, errClaim
	}

	var claimed struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ID            flexString `json:"id"`
			UserBenefitID flexString `json:"userBenefitId"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &claimed); errUnmarshal != nil {
		return nil, fmt.Errorf("codearts: invalid claim response: %w", errUnmarshal)
	}
	if claimed.Code != nil && *claimed.Code != 0 && *claimed.Code != 200 {
		return nil, &Error{StatusCode: http.StatusOK, Message: firstNonEmpty(claimed.Message, fmt.Sprintf("code=%d", *claimed.Code))}
	}
	benefitID := firstNonEmpty(claimed.Data.ID.String(), claimed.Data.UserBenefitID.String())
	if claimed.Code == nil && benefitID == "" {
		return nil, fmt.Errorf("codearts: claim response missing success evidence")
	}

	result := &CheckInResult{
		Claimed:       true,
		CampaignID:    campaignID,
		UserBenefitID: benefitID,
	}
	if result.UserBenefitID != "" {
		if errConfirm := c.confirmActivity(ctx, creds, result.UserBenefitID); errConfirm != nil {
			// Best effort only: credits are granted by the claim itself, and the
			// desktop client ignores confirm failures entirely (the renderer
			// awaits confirmWelfare without checking its result). A confirm
			// failure must not fail the check-in nor trigger the simple-benefit
			// fallback, which would double-report the reward.
			result.ConfirmError = errConfirm.Error()
		}
	}
	return result, nil
}

// confirmActivity finalizes a claim.
//
// Mirrors the desktop client exactly: the body carries ONLY userBenefitId —
// sending campaignId as well is wrong (the official client never does it).
// Failures (e.g. HDN.1000 "userBenefitId : unknown exception") are non-fatal;
// the error envelope here is error_code/error_msg, unlike claim/delivery's
// code/message.
func (c *Client) confirmActivity(ctx context.Context, creds Credentials, userBenefitID string) error {
	payload, errMarshal := json.Marshal(map[string]any{
		"userBenefitId": userBenefitID,
	})
	if errMarshal != nil {
		return fmt.Errorf("encode confirm request: %w", errMarshal)
	}
	endpoint := strings.TrimRight(c.snapHost, "/") + WelfareConfirmPath
	raw, errConfirm := c.signedPostJSON(ctx, endpoint, creds, payload, c.checkInBusinessHeaders(map[string]string{
		"Agent-Type": AgentTypePromptCenter,
		"X-Language": "zh-cn",
	}))
	if errConfirm != nil {
		return errConfirm
	}
	var parsed struct {
		Code      *int   `json:"code"`
		Message   string `json:"message"`
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal == nil {
		if parsed.Code != nil && *parsed.Code != 0 && *parsed.Code != 200 {
			return fmt.Errorf("%s", firstNonEmpty(parsed.Message, fmt.Sprintf("code=%d", *parsed.Code)))
		}
		if parsed.ErrorCode != "" && parsed.ErrorCode != "0000" {
			return &Error{StatusCode: http.StatusOK, Code: parsed.ErrorCode, Message: parsed.ErrorMsg}
		}
	}
	return nil
}

// DailyCheckIn performs the daily check-in: it lists the activities, claims the
// first claimable daily check-in activity and confirms it (best effort).
//
// It falls back to the developer-gateway benefit endpoint when the welfare
// activity list is unavailable, mirroring the desktop client.
func (c *Client) DailyCheckIn(ctx context.Context, creds Credentials) (*CheckInResult, error) {
	activities, errActivities := c.ListActivities(ctx, creds)
	if errActivities == nil {
		for _, activity := range activities {
			if !activity.ClaimableNow() {
				continue
			}
			result, errClaim := c.ClaimActivity(ctx, creds, activity.CampaignID)
			if errClaim != nil {
				return nil, errClaim
			}
			result.BenefitAmount = activity.BenefitAmount
			return result, nil
		}
		// Only an explicit completed daily status proves today's reward was claimed.
		hasDaily := false
		for _, activity := range activities {
			if !activity.IsDailyCheckIn() {
				continue
			}
			hasDaily = true
			switch activity.Status {
			case WelfareClaimed, WelfareConfirmed, WelfareConsumed:
				return &CheckInResult{
					AlreadyClaimed: true,
					CampaignID:     activity.CampaignID,
					BenefitAmount:  activity.BenefitAmount,
				}, nil
			}
		}
		if hasDaily {
			// A daily activity can be unavailable without having been completed.
			return &CheckInResult{}, nil
		}
	}

	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Fallback: the simple developer-gateway benefit endpoint.
	result, errBenefit := c.claimSimpleBenefit(ctx, creds)
	if errBenefit != nil {
		if errActivities != nil {
			return nil, fmt.Errorf("codearts: check-in failed: welfare=%v benefit=%w", errActivities, errBenefit)
		}
		return nil, errBenefit
	}
	return result, nil
}

// claimSimpleBenefit posts to the developer-gateway benefit endpoint.
func (c *Client) claimSimpleBenefit(ctx context.Context, creds Credentials) (*CheckInResult, error) {
	endpoint := strings.TrimRight(c.benefitURL, "/") + BenefitClaimPath
	raw, errClaim := c.signedPostJSON(ctx, endpoint, creds, []byte("{}"), c.checkInBusinessHeaders(map[string]string{"X-Language": "zh-cn"}))
	if errClaim != nil {
		return nil, errClaim
	}
	var parsed struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("codearts: invalid benefit response: %w", errUnmarshal)
	}
	if parsed.ErrorCode != "0000" {
		return nil, &Error{StatusCode: http.StatusOK, Code: parsed.ErrorCode, Message: parsed.ErrorMsg}
	}
	return &CheckInResult{Claimed: true}, nil
}

// Balance queries the remaining token balance.
func (c *Client) Balance(ctx context.Context, creds Credentials) (json.RawMessage, error) {
	endpoint := strings.TrimRight(c.benefitURL, "/") + BalancePath
	raw, errFetch := c.signedGetJSON(ctx, endpoint, creds, map[string]string{"X-Language": "zh-cn"})
	if errFetch != nil {
		return nil, errFetch
	}

	var envelope struct {
		ErrorCode string          `json:"error_code"`
		ErrorMsg  string          `json:"error_msg"`
		Result    json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("codearts: invalid balance response: %w", errUnmarshal)
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, &Error{StatusCode: http.StatusOK, Code: envelope.ErrorCode, Message: envelope.ErrorMsg}
	}
	return envelope.Result, nil
}

// GetAuthenticationByTicket polls the snap engine for the credentials produced
// by the legacy ticket login flow. The desktop client retries every 2 seconds
// up to 10 times while the user finishes logging in.
func (c *Client) GetAuthenticationByTicket(ctx context.Context, ticketID, secret string) (*TokenData, error) {
	endpoint := strings.TrimRight(c.snapHost, "/") + "/snap-manager" + TicketPath +
		"?ticket_id=" + url.QueryEscape(strings.TrimSpace(ticketID)) +
		"&secret=" + url.QueryEscape(strings.TrimSpace(secret))

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("codearts: create ticket request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("plugin-name", PluginName)
	req.Header.Set("plugin-version", PluginVersion)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts: ticket request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			return
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("codearts: read ticket response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}

	var parsed struct {
		UserID     string `json:"user_id"`
		UserName   string `json:"user_name"`
		DomainID   string `json:"domain_id"`
		Credential struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"securitytoken"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
	}
	if errUnmarshal := json.Unmarshal(data, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("codearts: invalid ticket response: %w", errUnmarshal)
	}
	if strings.TrimSpace(parsed.Credential.Access) == "" {
		return nil, fmt.Errorf("codearts: ticket not ready")
	}

	token := &TokenData{
		AccessKey:     strings.TrimSpace(parsed.Credential.Access),
		SecretKey:     strings.TrimSpace(parsed.Credential.Secret),
		SecurityToken: strings.TrimSpace(parsed.Credential.SecurityToken),
	}
	if expiry := strings.TrimSpace(parsed.Credential.ExpiresAt); expiry != "" {
		if parsedTime, errTime := time.Parse(time.RFC3339, expiry); errTime == nil {
			token.ExpiresAt = parsedTime.UTC()
		}
	}
	if token.ExpiresAt.IsZero() {
		token.ExpiresAt = time.Now().UTC().Add(time.Hour)
	}
	return token, nil
}

// normalizeActivityType canonicalizes the upstream activity type values.
func normalizeActivityType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "daily_claim":
		return "daily_claim"
	case "invite_user":
		return "invite"
	case "student_certified":
		return "student_certify"
	case "user_login":
		return "login"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}
