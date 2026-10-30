package coupon

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/auth"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/pagination"
	"carsihub/internal/pricing"
)

type Handler struct {
	svc    *Service
	tokens *auth.Tokens
	log    *slog.Logger
}

func NewHandler(svc *Service, tokens *auth.Tokens, log *slog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, log: log}
}

// Mount: pazaryeri kuponunu yönetici/operasyon, satıcı kuponunu satıcı açar.
// Kapsam rolden çıkar; istemci seçemez.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/coupons", func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens), auth.RequireRole(auth.RoleAdmin, auth.RoleOperations, auth.RoleSeller))
		r.Post("/", h.create)
		r.Get("/", h.list)
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	scope, sellerID := pricing.ScopeMarketplace, ""
	if p.Role == auth.RoleSeller {
		id, err := h.svc.SellerOf(r.Context(), p.UserID)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		scope, sellerID = pricing.ScopeSeller, id
	}
	c, fields := in.Validate(scope, sellerID)
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	created, err := h.svc.Create(r.Context(), p.UserID, string(p.Role), c, in, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, created)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
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
	p, _ := auth.PrincipalFrom(r.Context())
	sellerID := ""
	if p.Role == auth.RoleSeller {
		if sellerID, err = h.svc.SellerOf(r.Context(), p.UserID); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	page, err := h.svc.List(r.Context(), sellerID, after, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrDuplicateCode):
		httpx.Error(w, http.StatusConflict, "duplicate_code", err.Error())
	case errors.Is(err, ErrNotSeller):
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
