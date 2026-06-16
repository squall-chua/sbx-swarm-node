package auth

import (
	"context"
	"net/http"
	"strings"
)

// Cookie/header names for sessions and CSRF (ADR-0006).
const (
	SessionCookie = "sbx_session"
	CSRFCookie    = "sbx_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

type ctxKey int

const roleKey ctxKey = 0

// KeyStore resolves a bearer key to a role.
type KeyStore interface {
	RoleForKey(key string) (string, bool)
}

// Middleware authenticates requests (bearer or cookie) and gates by role.
type Middleware struct {
	keys   KeyStore
	signer *Signer
}

// New builds the middleware.
func New(keys KeyStore, signer *Signer) *Middleware {
	return &Middleware{keys: keys, signer: signer}
}

// RoleFromContext returns the authenticated role, if any.
func RoleFromContext(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(roleKey).(string)
	return r, ok
}

// Authenticate resolves a role from a bearer token or a session cookie, and for
// cookie-authenticated unsafe methods enforces double-submit CSRF. On success it
// stores the role in the request context; otherwise it writes 401/403.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer path.
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if role, ok := m.keys.RoleForKey(strings.TrimPrefix(h, "Bearer ")); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
				return
			}
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}

		// Cookie session path.
		if c, err := r.Cookie(SessionCookie); err == nil {
			role, verr := m.signer.Verify(c.Value)
			if verr != nil {
				http.Error(w, "invalid session", http.StatusUnauthorized)
				return
			}
			if !isSafeMethod(r.Method) && !csrfOK(r) {
				http.Error(w, "csrf check failed", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
			return
		}

		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

// RequireRole wraps a handler so only the given role (or admin) may proceed.
func (m *Middleware) RequireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := RoleFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if got != role && got != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func csrfOK(r *http.Request) bool {
	c, err := r.Cookie(CSRFCookie)
	if err != nil {
		return false
	}
	return c.Value != "" && c.Value == r.Header.Get(CSRFHeader)
}
