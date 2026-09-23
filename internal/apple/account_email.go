package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const accountPortalOrigin = "https://account.apple.com"
const accountManageURL = "https://appleid.apple.com/account/manage"

var (
	ErrAccountWebAuth      = errors.New("Apple account management login required")
	ErrAccountEmailInvalid = errors.New("Apple rejected the email address")
	ErrAccountEmailCode    = errors.New("Apple rejected the email verification code")
	ErrAccountWebIdentity  = errors.New("Apple account management identity mismatch")
)

// AccountWebSession is separate from iCloud authentication: the portal uses
// its own OAuth service key, SCNT and cookies. It contains no password or code.
type AccountWebSession struct {
	AppleID        string             `json:"apple_id"`
	AccountName    string             `json:"account_name,omitempty"`
	ServiceKey     string             `json:"service_key"`
	APIKey         string             `json:"api_key,omitempty"`
	SCNT           string             `json:"scnt,omitempty"`
	SessionID      string             `json:"session_id,omitempty"`
	AuthAttributes string             `json:"auth_attributes,omitempty"`
	FrameID        string             `json:"frame_id,omitempty"`
	Cookies        []PersistentCookie `json:"cookies,omitempty"`
}

type AccountEmail struct {
	ID        int64  `json:"id"`
	Address   string `json:"address"`
	Removable bool   `json:"removable"`
}

type AccountEmailProfile struct {
	AppleID string         `json:"apple_id"`
	CanAdd  bool           `json:"can_add"`
	Emails  []AccountEmail `json:"emails"`
}

type EmailVerification struct {
	ID      string `json:"verificationId"`
	Address string `json:"address"`
	Length  int    `json:"length"`
}

func (op *operation) authWidgetKey() string {
	if op.widgetKey != "" {
		return op.widgetKey
	}
	return defaultWidgetKey
}

func (op *operation) passwordSignIn(ctx context.Context, appleID, password string) (int, error) {
	secret := make([]byte, 32)
	if err := op.owner.readRandom(secret); err != nil {
		return 0, operationError("initialize SRP", ErrAuthentication, 0, err)
	}
	srp, err := newSRPClient(bytes.NewReader(secret))
	if err != nil {
		return 0, operationError("initialize SRP", ErrAuthentication, 0, err)
	}
	challenge, err := op.srpInit(ctx, appleID, srp.publicKey())
	if err != nil {
		return 0, err
	}
	key, err := deriveApplePassword(password, challenge.salt, challenge.iterations, challenge.protocol)
	if err != nil {
		return 0, operationError("derive SRP proof", ErrInvalidResponse, 0, err)
	}
	if err := srp.processChallenge([]byte(appleID), key, challenge.salt, challenge.serverPublic); err != nil {
		return 0, operationError("derive SRP proof", ErrInvalidResponse, 0, err)
	}
	return op.srpComplete(ctx, appleID, challenge.challenge, srp.m1, srp.m2)
}

func (c *Client) accountOperation(state *AccountWebSession) (*operation, func(), error) {
	session := &Session{Region: RegionGlobal, AppleID: state.AppleID, SCNT: state.SCNT,
		SessionID: state.SessionID, AuthAttributes: state.AuthAttributes, FrameID: state.FrameID,
		Cookies: append([]PersistentCookie(nil), state.Cookies...)}
	op, err := c.newOperation(session)
	if err != nil {
		return nil, nil, err
	}
	op.endpoints.Home = accountPortalOrigin
	op.widgetKey = state.ServiceKey
	return op, func() {
		state.SCNT, state.SessionID = session.SCNT, session.SessionID
		state.AuthAttributes, state.FrameID = session.AuthAttributes, session.FrameID
		state.Cookies = op.jar.Export()
	}, nil
}

func (op *operation) accountHeaders(apiKey string) http.Header {
	headers := op.serviceHeaders()
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Apple-I-Request-Context", "ca")
	headers.Set("X-Apple-I-TimeZone", "Asia/Shanghai")
	headers.Set("X-Apple-I-FD-Client-Info", `{"U":"`+userAgent+`","L":"zh-CN","Z":"GMT+08:00","V":"1.1","F":""}`)
	if op.session.SCNT != "" {
		headers.Set("scnt", op.session.SCNT)
	}
	if apiKey != "" {
		headers.Set("X-Apple-Api-Key", apiKey)
	}
	return headers
}

func accountResponseError(response responseData, operation string, invalid error) error {
	if response.status >= 200 && response.status < 300 {
		return nil
	}
	kind := ErrService
	switch response.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = ErrAccountWebAuth
	case http.StatusPreconditionFailed:
		kind = ErrTermsRequired
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		if invalid != nil {
			kind = invalid
		}
	}
	return nonRetryableAppleError(responseError(operation, kind, response))
}

// SignInAccount uses the service key obtained from the portal bootstrap, not
// the iCloud widget key or any credential captured in a HAR.
func (c *Client) SignInAccount(ctx context.Context, state AccountWebSession, password string) (result AccountWebSession, needsCode bool, err error) {
	result = state
	result.AppleID = strings.ToLower(strings.TrimSpace(result.AppleID))
	result.AccountName = ""
	if result.AppleID == "" || password == "" {
		return result, false, ErrAuthentication
	}
	op, persist, err := c.accountOperation(&result)
	if err != nil {
		return result, false, err
	}
	defer persist()
	response, err := op.request(ctx, "open Apple account portal", http.MethodGet, accountPortalOrigin+"/", nil, op.accountHeaders(""))
	if err != nil {
		return result, false, err
	}
	if err := accountResponseError(response, "open account portal", nil); err != nil {
		return result, false, err
	}
	response, err = op.request(ctx, "bootstrap Apple account portal", http.MethodGet, accountPortalOrigin+"/bootstrap/portal", nil, op.accountHeaders(""))
	if err != nil {
		return result, false, err
	}
	if err := accountResponseError(response, "bootstrap account portal", nil); err != nil {
		return result, false, err
	}
	var bootstrap struct {
		ServiceKey string `json:"serviceKey"`
	}
	if json.Unmarshal(response.body, &bootstrap) != nil || bootstrap.ServiceKey == "" || len(bootstrap.ServiceKey) > 512 {
		return result, false, ErrInvalidResponse
	}
	result.ServiceKey, op.widgetKey = bootstrap.ServiceKey, bootstrap.ServiceKey
	op.session.FrameID, err = c.newUUID()
	if err != nil {
		return result, false, err
	}
	if err := op.authorize(ctx); err != nil {
		return result, false, err
	}
	if err := op.federate(ctx, result.AppleID); err != nil {
		return result, false, err
	}
	status, err := op.passwordSignIn(ctx, result.AppleID, password)
	if err != nil {
		return result, false, err
	}
	if status == http.StatusConflict {
		_, _ = op.requestTrustedDeviceCode(ctx)
		return result, true, nil
	}
	_, err = op.finishAccountSignIn(ctx, &result)
	return result, false, err
}

func (c *Client) VerifyAccountCode(ctx context.Context, state AccountWebSession, code string) (result AccountWebSession, err error) {
	result = state
	if state.ServiceKey == "" {
		return result, ErrAccountWebAuth
	}
	if !validSecurityCode(code) {
		return result, ErrTwoFactorCode
	}
	op, persist, err := c.accountOperation(&result)
	if err != nil {
		return result, err
	}
	defer persist()
	response, err := op.request(ctx, "verify account two-factor code", http.MethodPost,
		op.endpoints.Auth+"/verify/trusteddevice/securitycode", map[string]any{"securityCode": map[string]string{"code": code}}, op.authHeaders())
	if err != nil {
		return result, err
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return result, ErrTwoFactorCode
	}
	if err := accountResponseError(response, "verify account two-factor code", ErrTwoFactorCode); err != nil {
		return result, err
	}
	_, err = op.finishAccountSignIn(ctx, &result)
	return result, err
}

func (op *operation) finishAccountSignIn(ctx context.Context, state *AccountWebSession) (AccountEmailProfile, error) {
	response, err := op.request(ctx, "initialize account management", http.MethodGet, accountManageURL+"/gs/ws/token", nil, op.accountHeaders(""))
	if err != nil {
		return AccountEmailProfile{}, err
	}
	if err := accountResponseError(response, "initialize account management", nil); err != nil {
		return AccountEmailProfile{}, err
	}
	return op.accountEmailProfile(ctx, state)
}

func (c *Client) ListAccountEmails(ctx context.Context, state AccountWebSession) (profile AccountEmailProfile, result AccountWebSession, err error) {
	result = state
	if result.ServiceKey == "" {
		return profile, result, ErrAccountWebAuth
	}
	op, persist, err := c.accountOperation(&result)
	if err != nil {
		return profile, result, err
	}
	defer persist()
	profile, err = op.accountEmailProfile(ctx, &result)
	return profile, result, err
}

func (op *operation) accountEmailProfile(ctx context.Context, state *AccountWebSession) (AccountEmailProfile, error) {
	response, err := op.request(ctx, "list Apple account emails", http.MethodGet, accountManageURL, nil, op.accountHeaders(""))
	if err != nil {
		return AccountEmailProfile{}, err
	}
	if err := accountResponseError(response, "list account emails", nil); err != nil {
		return AccountEmailProfile{}, err
	}
	type emailRecord struct {
		ID            int64  `json:"id"`
		Address       string `json:"address"`
		Vetted        bool   `json:"vetted"`
		Primary       bool   `json:"primary"`
		SameAsAccount bool   `json:"isEmailSameAsAccountName"`
	}
	var body struct {
		APIKey  string `json:"apiKey"`
		AppleID struct {
			Name string `json:"name"`
		} `json:"appleID"`
		Primary    emailRecord   `json:"primaryEmailAddress"`
		Alternates []emailRecord `json:"alternateEmailAddresses"`
		CanAdd     bool          `json:"shouldAllowAddAlternateEmail"`
	}
	if json.Unmarshal(response.body, &body) != nil || body.APIKey == "" || body.AppleID.Name == "" || body.Primary.Address == "" || body.Alternates == nil {
		return AccountEmailProfile{}, ErrInvalidResponse
	}
	primary, err := normalizeHMEAddress(body.Primary.Address)
	if err != nil || body.Primary.ID < 1 {
		return AccountEmailProfile{}, ErrInvalidResponse
	}
	profile := AccountEmailProfile{AppleID: body.AppleID.Name, CanAdd: body.CanAdd,
		Emails: []AccountEmail{{ID: body.Primary.ID, Address: primary}}}
	identityMatches := strings.EqualFold(state.AppleID, body.AppleID.Name) ||
		(body.Primary.Vetted && strings.EqualFold(state.AppleID, body.Primary.Address))
	seen := map[string]bool{profile.Emails[0].Address: true}
	seenIDs := map[int64]bool{body.Primary.ID: true}
	for _, email := range body.Alternates {
		address, err := normalizeHMEAddress(email.Address)
		if err != nil || email.ID < 1 || seen[address] || seenIDs[email.ID] {
			return AccountEmailProfile{}, ErrInvalidResponse
		}
		seen[address] = true
		seenIDs[email.ID] = true
		identityMatches = identityMatches || (email.Vetted && strings.EqualFold(address, state.AppleID))
		profile.Emails = append(profile.Emails, AccountEmail{ID: email.ID, Address: address,
			Removable: !email.Primary && !email.SameAsAccount && !strings.EqualFold(address, body.AppleID.Name)})
	}
	if state.AccountName != "" {
		identityMatches = strings.EqualFold(state.AccountName, body.AppleID.Name)
	}
	if !identityMatches {
		return AccountEmailProfile{}, ErrAccountWebIdentity
	}
	state.AccountName = body.AppleID.Name
	state.APIKey = body.APIKey
	return profile, nil
}

func (c *Client) BeginAccountEmail(ctx context.Context, state AccountWebSession, address string) (verification EmailVerification, result AccountWebSession, err error) {
	result = state
	address, err = normalizeHMEAddress(address)
	if err != nil {
		return verification, result, ErrAccountEmailInvalid
	}
	response, result, err := c.accountEmailRequest(ctx, result, http.MethodPost, "/email/alternate/add/verification", map[string]string{"address": address}, ErrAccountEmailInvalid)
	if err != nil {
		return verification, result, err
	}
	if json.Unmarshal(response.body, &verification) != nil || verification.ID == "" || verification.Length != 6 || !strings.EqualFold(verification.Address, address) {
		return EmailVerification{}, result, ErrInvalidResponse
	}
	return verification, result, nil
}

func (c *Client) VerifyAccountEmail(ctx context.Context, state AccountWebSession, verification EmailVerification, code string) (result AccountWebSession, err error) {
	if !validSecurityCode(code) || verification.ID == "" {
		return state, ErrAccountEmailCode
	}
	response, result, err := c.accountEmailRequest(ctx, state, http.MethodPut, "/email/alternate/verification", map[string]any{
		"address": verification.Address, "verificationInfo": map[string]string{"id": verification.ID, "answer": code},
	}, ErrAccountEmailCode)
	if err != nil {
		return result, err
	}
	var added struct {
		ID      int64  `json:"id"`
		Address string `json:"address"`
		Vetted  bool   `json:"vetted"`
	}
	if json.Unmarshal(response.body, &added) != nil || added.ID < 1 || !added.Vetted || !strings.EqualFold(added.Address, verification.Address) {
		return result, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) DeleteAccountEmail(ctx context.Context, state AccountWebSession, email AccountEmail) (result AccountWebSession, err error) {
	if email.ID < 1 || !email.Removable || email.Address == "" {
		return state, ErrAccountEmailInvalid
	}
	response, result, err := c.accountEmailRequest(ctx, state, http.MethodDelete, "/email/alternate/"+strconv.FormatInt(email.ID, 10), nil, ErrAccountEmailInvalid)
	if err != nil {
		return result, err
	}
	var body struct {
		Removed AccountEmail `json:"emailAddressRemoved"`
	}
	if json.Unmarshal(response.body, &body) != nil || body.Removed.ID != email.ID || !strings.EqualFold(body.Removed.Address, email.Address) {
		return result, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) accountEmailRequest(ctx context.Context, state AccountWebSession, method, path string, body any, invalid error) (response responseData, result AccountWebSession, err error) {
	result = state
	if state.APIKey == "" || state.ServiceKey == "" {
		return response, result, ErrAccountWebAuth
	}
	op, persist, err := c.accountOperation(&result)
	if err != nil {
		return response, result, err
	}
	defer persist()
	response, err = op.request(ctx, "manage Apple account email", method, accountManageURL+path, body, op.accountHeaders(state.APIKey))
	if err != nil {
		return response, result, nonRetryableAppleError(err)
	}
	return response, result, accountResponseError(response, "manage account email", invalid)
}
