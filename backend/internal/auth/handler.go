package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/pagination"
)

// Tarayıcı istemcisi refresh token'ı JavaScript'in okuyamadığı bu çerezde
// taşır (XSS ile çalınamaz). API istemcileri (verify, requests.http) aynı
// token'ı gövdeden de alıp gönderebilir.
const refreshCookie = "carsihub_refresh"

type Handler struct {
	svc          *Service
	tokens       *Tokens
	log          *slog.Logger
	secureCookie bool
}

func NewHandler(svc *Service, tokens *Tokens, log *slog.Logger, secureCookie bool) *Handler {
	return &Handler{svc: svc, tokens: tokens, log: log, secureCookie: secureCookie}
}

// Mount rotaları /api/v1 altına bağlar.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
		r.With(Authenticate(h.tokens)).Get("/me", h.me)
	})
	r.With(Authenticate(h.tokens), RequireRole(RoleAdmin)).Get("/admin/users", h.listUsers)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in LoginInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if fields := in.Normalize(); len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	session, err := h.svc.Login(r.Context(), in, httpx.RequestIDFrom(r.Context()))
	h.writeSession(w, r, session, err)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.refreshToken(w, r)
	if !ok {
		return
	}
	session, err := h.svc.Refresh(r.Context(), raw, httpx.RequestIDFrom(r.Context()))
	if err != nil && IsAuthError(err) {
		h.clearCookie(w)
	}
	h.writeSession(w, r, session, err)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	raw, ok := h.refreshToken(w, r)
	if !ok {
		return
	}
	if err := h.svc.Logout(r.Context(), raw, httpx.RequestIDFrom(r.Context())); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	h.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	u, err := h.svc.Me(r.Context(), p.UserID)
	switch {
	case errors.Is(err, ErrInvalidToken):
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, err.Error())
	case err != nil:
		httpx.Internal(w, r, h.log, err)
	default:
		httpx.JSON(w, http.StatusOK, u)
	}
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"), 20, 100)
	if err != nil {
		httpx.ValidationError(w, map[string]string{"limit": err.Error()})
		return
	}
	after, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		httpx.ValidationError(w, map[string]string{"cursor": err.Error()})
		return
	}
	page, err := h.svc.ListUsers(r.Context(), after, limit)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

// refreshToken önce gövdeye, yoksa çereze bakar.
func (h *Handler) refreshToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.ContentLength != 0 {
		var body refreshRequest
		if err := httpx.Decode(w, r, &body); err != nil {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
			return "", false
		}
		if body.RefreshToken != "" {
			return body.RefreshToken, true
		}
	}
	if c, err := r.Cookie(refreshCookie); err == nil {
		return c.Value, true
	}
	return "", true
}

func (h *Handler) writeSession(w http.ResponseWriter, r *http.Request, s *Session, err error) {
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case errors.Is(err, ErrTokenReuse):
		httpx.Error(w, http.StatusUnauthorized, "token_reused", err.Error())
	case errors.Is(err, ErrInvalidToken):
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, err.Error())
	case err != nil:
		httpx.Internal(w, r, h.log, err)
	default:
		http.SetCookie(w, &http.Cookie{
			Name: refreshCookie, Value: s.RefreshToken, Path: "/api/v1/auth",
			Expires: s.RefreshExpiresAt, HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteStrictMode,
		})
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, s)
	}
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookie, Value: "", Path: "/api/v1/auth", Expires: time.Unix(0, 0), MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteStrictMode,
	})
}
