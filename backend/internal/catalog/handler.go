package catalog

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/auth"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/pagination"
)

type Handler struct {
	svc    *Service
	tokens *auth.Tokens
	log    *slog.Logger
}

func NewHandler(svc *Service, tokens *auth.Tokens, log *slog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, log: log}
}

// Mount rotaları bağlar. Katalog okuma oturum açmış her role açıktır; satıcı
// uçları yalnızca satıcıya ve yalnızca kendi tekliflerine.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens))
		r.Get("/categories", h.categories)
		r.Get("/products", h.products)
		r.Get("/products/{id}", h.product)

		r.Route("/seller", func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleSeller))
			r.Get("/offers", h.sellerOffers)
			r.Post("/products", h.createProduct)
			r.Put("/offers/{id}/stock", h.setStock)
		})
	})
}

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.Categories(r.Context())
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": cs})
}

func (h *Handler) products(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	page, err := h.svc.Products(r.Context(), after, limit)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) product(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, ErrNotFound.Error())
		return
	}
	p, err := h.svc.Product(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

func (h *Handler) sellerOffers(w http.ResponseWriter, r *http.Request) {
	after, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	page, err := h.svc.SellerOffers(r.Context(), p.UserID, after, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) createProduct(w http.ResponseWriter, r *http.Request) {
	var in CreateProductInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if fields := in.Normalize(); len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	product, err := h.svc.CreateProduct(r.Context(), p.UserID, in, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, product)
}

type stockRequest struct {
	StockOnHand *int `json:"stock_on_hand"`
}

func (h *Handler) setStock(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, ErrNotFound.Error())
		return
	}
	var in stockRequest
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if in.StockOnHand == nil || *in.StockOnHand < 0 || *in.StockOnHand > MaxStock {
		httpx.ValidationError(w, map[string]string{"stock_on_hand": "0 ile 1000000 arasında olmalı"})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	o, err := h.svc.SetStock(r.Context(), p.UserID, id, *in.StockOnHand, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

func pageParams(w http.ResponseWriter, r *http.Request) (*pagination.Cursor, int, bool) {
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"), 20, 100)
	if err != nil {
		httpx.ValidationError(w, map[string]string{"limit": err.Error()})
		return nil, 0, false
	}
	after, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		httpx.ValidationError(w, map[string]string{"cursor": err.Error()})
		return nil, 0, false
	}
	return after, limit, true
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case errors.Is(err, ErrNotSeller):
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, err.Error())
	case errors.Is(err, ErrBelowReserved):
		httpx.Error(w, http.StatusConflict, "below_reserved", err.Error())
	case errors.Is(err, ErrDuplicateSKU):
		httpx.Error(w, http.StatusConflict, "duplicate_sku", err.Error())
	case errors.Is(err, ErrBadCategory):
		httpx.ValidationError(w, map[string]string{"category_id": err.Error()})
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
