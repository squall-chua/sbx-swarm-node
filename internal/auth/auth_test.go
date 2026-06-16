package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type keyStore map[string]string // key -> role

func (k keyStore) RoleForKey(key string) (string, bool) { r, ok := k[key]; return r, ok }

func TestAuthenticate_BearerAndRoleGate(t *testing.T) {
	keys := keyStore{"adm": "admin", "ro": "read-only"}
	m := New(keys, NewSigner([]byte("k")))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// require admin
	h := m.Authenticate(m.RequireRole("admin", final))

	// no creds -> 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// read-only bearer on admin route -> 403
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer ro")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// admin bearer -> 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer adm")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_CookieAndCSRF(t *testing.T) {
	keys := keyStore{"adm": "admin"}
	signer := NewSigner([]byte("k"))
	m := New(keys, signer)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := m.Authenticate(final)

	tok := signer.Mint("admin", time.Now().Add(time.Hour))

	// cookie GET -> ok (no CSRF needed for safe method)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// cookie POST without CSRF header -> 403
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "abc"})
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// cookie POST with matching CSRF header+cookie -> ok
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: tok})
	req.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "abc"})
	req.Header.Set(CSRFHeader, "abc")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
