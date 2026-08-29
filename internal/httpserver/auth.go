package httpserver

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const (
	loginCSRFCookieMissing      = "cookie_missing"
	loginCSRFFormTokenMissing   = "form_token_missing"
	loginCSRFTokenMismatch      = "token_mismatch"
	loginCSRFFetchSiteCrossSite = "fetch_site_cross_site"
	loginCSRFOriginInvalid      = "origin_invalid"
	loginCSRFOriginHostMismatch = "origin_host_mismatch"
)

func requestCookieCount(r *http.Request, name string) int {
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			count++
		}
	}
	return count
}

func (s *Server) setSessionCookie(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: token, Path: s.cfg.AdminPath, MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) setSessionCookies(c *gin.Context, token string) {
	s.setSessionCookie(c, token)
	if s.cfg.AdminPath != "/admin" {
		http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: token, Path: "/admin", MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	}
}

func (s *Server) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: "", Path: s.cfg.AdminPath, MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
}

func (s *Server) clearSessionCookies(c *gin.Context) {
	s.clearSessionCookie(c)
	if s.cfg.AdminPath != "/admin" {
		http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	}
}

func (s *Server) audit(c *gin.Context, adminID *int64, username, action, resourceType, resourceID, result, detail string) {
	entry := domain.AuditLog{AdminID: adminID, Username: username, Action: action, ResourceType: resourceType, ResourceID: resourceID, Result: result, IP: c.ClientIP(), RequestID: requestID(c), Detail: detail, CreatedAt: time.Now().UTC()}
	if _, err := s.store.CreateAuditLog(c.Request.Context(), entry); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.logger.Error("写入操作记录失败", "error", err, "request_id", requestID(c))
	}
}

// credentialRotationReadGuard keeps public requests that consume or return
// alias credentials on one side of a successful global credential rotation.
// It must run before credential authentication and retains the read lock until
// the response has been written.
func (s *Server) credentialRotationReadGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.beforeCredentialRotationReadLock != nil {
			s.beforeCredentialRotationReadLock()
		}
		s.credentialRotationMu.RLock()
		defer s.credentialRotationMu.RUnlock()
		c.Next()
	}
}

// adminAPICredentialRotationReadGuard prevents an already authenticated
// request from crossing a successful global credential-rotation commit. The
// session is checked again only after acquiring the read lock, and the lock is
// retained until the handler has completed writing its response.
func (s *Server) adminAPICredentialRotationReadGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.credentialRotationMu.RLock()
		defer s.credentialRotationMu.RUnlock()

		raw, err := c.Cookie(sessionCookie)
		if err != nil || raw == "" {
			s.clearSessionCookies(c)
			writeAdminAPIError(c, http.StatusUnauthorized, "SESSION_EXPIRED", "登录会话已失效")
			c.Abort()
			return
		}
		previous := mustSession(c)
		current, err := s.store.GetSessionByHash(c.Request.Context(), secure.HashToken(raw))
		if errors.Is(err, store.ErrNotFound) ||
			(err == nil && (current.AdminID != previous.AdminID ||
				current.PasswordVersion != previous.PasswordVersion)) {
			s.clearSessionCookies(c)
			writeAdminAPIError(c, http.StatusUnauthorized, "SESSION_EXPIRED", "登录会话已失效")
			c.Abort()
			return
		}
		if err != nil {
			s.writeAdminAPIInternalError(c, err)
			c.Abort()
			return
		}
		c.Set(sessionKey, current)
		c.Next()
	}
}
