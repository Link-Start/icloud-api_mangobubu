package hmesync

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/mail"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
)

const (
	CodeEmailAccountAuthRequired = "APPLE_EMAIL_ACCOUNT_AUTH_REQUIRED"
	CodeEmailInvalid             = "APPLE_EMAIL_INVALID"
	CodeEmailCodeInvalid         = "APPLE_EMAIL_CODE_INVALID"
	CodeEmailFlowExpired         = "APPLE_EMAIL_FLOW_EXPIRED"
	CodeEmailLastAddress         = "APPLE_EMAIL_LAST_ADDRESS"
	CodeEmailNotRemovable        = "APPLE_EMAIL_NOT_REMOVABLE"
)

var ErrEmailManagement = errors.New("Apple account email management failed")

type AccountEmailClient interface {
	SignInAccount(context.Context, apple.AccountWebSession, string) (apple.AccountWebSession, bool, error)
	VerifyAccountCode(context.Context, apple.AccountWebSession, string) (apple.AccountWebSession, error)
	ListAccountEmails(context.Context, apple.AccountWebSession) (apple.AccountEmailProfile, apple.AccountWebSession, error)
	BeginAccountEmail(context.Context, apple.AccountWebSession, string) (apple.EmailVerification, apple.AccountWebSession, error)
	VerifyAccountEmail(context.Context, apple.AccountWebSession, apple.EmailVerification, string) (apple.AccountWebSession, error)
	DeleteAccountEmail(context.Context, apple.AccountWebSession, apple.AccountEmail) (apple.AccountWebSession, error)
}

// Optional for compatibility with existing account-email client adapters.
type AccountSessionReuser interface {
	ResumeAccountSession(context.Context, apple.Session) (apple.AccountEmailProfile, apple.AccountWebSession, error)
}

type EmailActionResult struct {
	Status      string `json:"status"`
	ChallengeID string `json:"challenge_id,omitempty"`
	Address     string `json:"address,omitempty"`
}

type emailChallenge struct {
	id               string
	kind             string
	ownerID          int64
	accountID        int64
	identity         accountIdentity
	appleID          string
	dsid             string
	sessionCreatedAt time.Time
	expiresAt        time.Time
	attempts         int
	portal           apple.AccountWebSession
	verification     apple.EmailVerification
}

type emailAccountOperation struct {
	client        AccountEmailClient
	account       domain.Account
	session       apple.Session
	record        domain.AppleWebSession
	portal        apple.AccountWebSession
	portalChanged bool
}

// All portal writes share the account/Apple operation locks. A separate portal
// login is encrypted inside the iCloud session without replacing its tokens.
func (s *Service) withEmailAccount(ctx context.Context, accountID int64, fn func(*emailAccountOperation) error) error {
	if accountID < 1 {
		return errors.New("account ID must be positive")
	}
	client, ok := s.client.(AccountEmailClient)
	if !ok {
		return wrapError(CodeUpstreamError, ErrUpstream, nil)
	}
	release, err := s.acquireOperation(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	return s.locker.WithAccountLock(ctx, accountID, func() error {
		account, err := s.repo.GetAccount(ctx, accountID)
		if err != nil {
			return err
		}
		if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeICloud {
			return wrapError(CodeAccountChanged, ErrAccountChanged, nil)
		}
		record, session, err := s.loadSession(ctx, accountID)
		if err != nil {
			return err
		}
		if err := validateSessionDSID(session.DSID, session); err != nil {
			return err
		}
		op := &emailAccountOperation{client: client, account: account, record: record, session: session,
			portal: apple.AccountWebSession{AppleID: session.AppleID}}
		if session.AccountWeb != nil {
			op.portal = *session.AccountWeb
		}
		if !sameEmail(op.portal.AppleID, session.AppleID) {
			return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		operationErr := fn(op)
		if !op.portalChanged {
			return operationErr
		}
		if !sameEmail(op.portal.AppleID, session.AppleID) {
			return wrapError(CodeAccountMismatch, ErrAccountMismatch, nil)
		}
		session = op.session
		session.AccountWeb = &op.portal
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, saveErr := s.saveSession(persistCtx, accountID, session)
		return errors.Join(operationErr, saveErr)
	})
}

func (op *emailAccountOperation) profile(ctx context.Context) (apple.AccountEmailProfile, error) {
	var profile apple.AccountEmailProfile
	var updated apple.AccountWebSession
	var err error
	reuser, canReuse := op.client.(AccountSessionReuser)
	if op.portal.ServiceKey == "" && canReuse {
		profile, updated, err = reuser.ResumeAccountSession(ctx, op.session)
	} else {
		profile, updated, err = op.client.ListAccountEmails(ctx, op.portal)
		if errors.Is(err, apple.ErrAccountWebAuth) && canReuse {
			profile, updated, err = reuser.ResumeAccountSession(ctx, op.session)
		}
	}
	op.portal, op.portalChanged = updated, true
	return profile, mapEmailManagementError(err)
}

func (s *Service) GetAccountEmails(ctx context.Context, accountID int64) (profile apple.AccountEmailProfile, err error) {
	err = s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		var err error
		profile, err = op.profile(ctx)
		return err
	})
	return
}

func (s *Service) StartEmailAccountAuth(ctx context.Context, ownerID, accountID int64, password string) (result EmailActionResult, err error) {
	if ownerID < 1 || password == "" {
		return result, wrapError(CodeCredentialsInvalid, ErrCredentialsInvalid, nil)
	}
	err = s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		updated, needsCode, err := op.client.SignInAccount(ctx, op.portal, password)
		if err != nil {
			return mapEmailManagementError(err)
		}
		if needsCode {
			flow, err := s.createEmailChallenge(op, ownerID, "auth", updated, apple.EmailVerification{})
			result = EmailActionResult{Status: StatusVerificationRequired, ChallengeID: flow.id}
			return err
		}
		op.portal, op.portalChanged = updated, true
		result.Status = StatusAuthenticated
		return nil
	})
	return
}

func (s *Service) VerifyEmailAccountAuth(ctx context.Context, ownerID, accountID int64, challengeID, code string) (result EmailActionResult, err error) {
	err = s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		flow, err := s.takeEmailChallengeAttempt(op, ownerID, challengeID, "auth")
		if err != nil {
			return err
		}
		updated, verifyErr := op.client.VerifyAccountCode(ctx, flow.portal, code)
		if verifyErr != nil {
			if errors.Is(verifyErr, apple.ErrTwoFactorCode) {
				s.updateEmailChallengePortal(flow.id, updated)
			} else {
				s.removeEmailChallenge(flow.id)
			}
			return mapEmailManagementError(verifyErr)
		}
		s.removeEmailChallenge(flow.id)
		op.portal, op.portalChanged = updated, true
		result.Status = StatusAuthenticated
		return nil
	})
	return
}

func (s *Service) BeginAccountEmail(ctx context.Context, ownerID, accountID int64, address string) (result EmailActionResult, err error) {
	address = domain.NormalizeEmail(address)
	parsed, parseErr := mail.ParseAddress(address)
	if ownerID < 1 || parseErr != nil || parsed.Name != "" || parsed.Address != address || len(address) > 320 {
		return result, emailManagementError(CodeEmailInvalid)
	}
	err = s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		profile, err := op.profile(ctx)
		if err != nil {
			return err
		}
		if !profile.CanAdd {
			return wrapError(CodeAccountActionRequired, ErrAccountActionRequired, nil)
		}
		for _, existing := range profile.Emails {
			if sameEmail(existing.Address, address) {
				return emailManagementError(CodeEmailInvalid)
			}
		}
		verification, updated, err := op.client.BeginAccountEmail(ctx, op.portal, address)
		op.portal, op.portalChanged = updated, true
		if err != nil {
			return mapEmailManagementError(err)
		}
		flow, err := s.createEmailChallenge(op, ownerID, "email", apple.AccountWebSession{}, verification)
		result = EmailActionResult{Status: "email_verification_required", ChallengeID: flow.id, Address: address}
		return err
	})
	return
}

func (s *Service) VerifyAccountEmail(ctx context.Context, ownerID, accountID int64, challengeID, code string) (result EmailActionResult, err error) {
	err = s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		flow, err := s.takeEmailChallengeAttempt(op, ownerID, challengeID, "email")
		if err != nil {
			return err
		}
		if _, err := op.profile(ctx); err != nil {
			return err
		}
		updated, verifyErr := op.client.VerifyAccountEmail(ctx, op.portal, flow.verification, code)
		op.portal, op.portalChanged = updated, true
		if verifyErr != nil {
			if !errors.Is(verifyErr, apple.ErrAccountEmailCode) {
				s.removeEmailChallenge(flow.id)
			}
			return mapEmailManagementError(verifyErr)
		}
		s.removeEmailChallenge(flow.id)
		result = EmailActionResult{Status: "complete", Address: flow.verification.Address}
		return nil
	})
	return
}

func (s *Service) DeleteAccountEmail(ctx context.Context, accountID int64, address string) error {
	address = domain.NormalizeEmail(address)
	return s.withEmailAccount(ctx, accountID, func(op *emailAccountOperation) error {
		// Check the fresh HME choices, not a client-supplied count or ID.
		list, updated, err := s.client.ListAliases(ctx, op.session)
		if err != nil {
			return mapAppleError(err, false)
		}
		updated, err = s.registrationSession(op.record, op.session, updated)
		if err != nil {
			return err
		}
		op.session = updated
		settings := forwardingSettingsFromList(list)
		if len(settings.ForwardToEmails) <= 1 {
			return emailManagementError(CodeEmailLastAddress)
		}
		if !containsForwardingCandidate(settings.ForwardToEmails, address) {
			return emailManagementError(CodeEmailNotRemovable)
		}
		profile, err := op.profile(ctx)
		if err != nil {
			return err
		}
		if len(profile.Emails) <= 1 {
			return emailManagementError(CodeEmailLastAddress)
		}
		for _, email := range profile.Emails {
			if !sameEmail(email.Address, address) {
				continue
			}
			if !email.Removable {
				return emailManagementError(CodeEmailNotRemovable)
			}
			updated, err := op.client.DeleteAccountEmail(ctx, op.portal, email)
			op.portal, op.portalChanged = updated, true
			return mapEmailManagementError(err)
		}
		return emailManagementError(CodeEmailNotRemovable)
	})
}

func (s *Service) createEmailChallenge(op *emailAccountOperation, ownerID int64, kind string, portal apple.AccountWebSession, verification apple.EmailVerification) (emailChallenge, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return emailChallenge{}, err
	}
	flow := emailChallenge{id: base64.RawURLEncoding.EncodeToString(secret), kind: kind, ownerID: ownerID,
		accountID: op.account.ID, identity: identityOf(op.account), appleID: op.session.AppleID, dsid: op.session.DSID,
		sessionCreatedAt: op.record.CreatedAt, expiresAt: s.now().Add(s.challengeTTL), portal: portal, verification: verification}
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	if s.emailChallenges == nil {
		s.emailChallenges = make(map[string]emailChallenge)
	}
	for id, previous := range s.emailChallenges {
		if !s.now().Before(previous.expiresAt) || (previous.ownerID == ownerID && previous.accountID == op.account.ID && previous.kind == kind) {
			delete(s.emailChallenges, id)
		}
	}
	s.emailChallenges[flow.id] = flow
	return flow, nil
}

func (s *Service) takeEmailChallengeAttempt(op *emailAccountOperation, ownerID int64, id, kind string) (emailChallenge, error) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	flow, ok := s.emailChallenges[id]
	if !ok || ownerID < 1 || flow.ownerID != ownerID || flow.accountID != op.account.ID || flow.kind != kind {
		return emailChallenge{}, emailManagementError(CodeEmailFlowExpired)
	}
	if !s.now().Before(flow.expiresAt) || flow.attempts >= s.maxAttempts ||
		!sameIdentity(flow.identity, identityOf(op.account)) || !sameEmail(flow.appleID, op.session.AppleID) ||
		flow.dsid != op.session.DSID || !flow.sessionCreatedAt.Equal(op.record.CreatedAt) {
		delete(s.emailChallenges, id)
		return emailChallenge{}, emailManagementError(CodeEmailFlowExpired)
	}
	flow.attempts++
	s.emailChallenges[id] = flow
	return flow, nil
}

func (s *Service) removeEmailChallenge(id string) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	delete(s.emailChallenges, id)
}

func (s *Service) updateEmailChallengePortal(id string, portal apple.AccountWebSession) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	if flow, ok := s.emailChallenges[id]; ok {
		flow.portal = portal
		s.emailChallenges[id] = flow
	}
}

func emailManagementError(code string) error { return wrapError(code, ErrEmailManagement, nil) }

func mapEmailManagementError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, apple.ErrAccountWebAuth):
		return emailManagementError(CodeEmailAccountAuthRequired)
	case errors.Is(err, apple.ErrAccountEmailInvalid):
		return emailManagementError(CodeEmailInvalid)
	case errors.Is(err, apple.ErrAccountEmailCode):
		return emailManagementError(CodeEmailCodeInvalid)
	case errors.Is(err, apple.ErrAccountWebIdentity):
		return wrapError(CodeAccountMismatch, ErrAccountMismatch, err)
	}
	return mapAppleError(err, false)
}
