package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"carsihub/internal/platform/httpx"
)

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom yalnızca Authenticate'ten geçmiş isteklerde ok=true döner.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticate Bearer access token'ı doğrular. Token yoksa ya da geçersizse 401.
func Authenticate(tokens *Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearer(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "kimlik doğrulama gerekli")
				return
			}
			p, err := tokens.ParseAccess(raw)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, ErrInvalidToken.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireRole yalnızca verilen rollere izin verir; diğerleri 403 alır.
// Authenticate'ten sonra kullanılmalıdır.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "kimlik doğrulama gerekli")
				return
			}
			if !slices.Contains(roles, p.Role) {
				httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "bu işlem için yetkiniz yok")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearer(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}
