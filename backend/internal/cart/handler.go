package cart

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/auth"
	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/httpx"
)

type Handler struct {
	svc         *Service
	tokens      *auth.Tokens
	idempotency func(http.Handler) http.Handler
	log         *slog.Logger
}

func NewHandler(svc *Service, tokens *auth.Tokens, idempotency func(http.Handler) http.Handler, log *slog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, idempotency: idempotency, log: log}
}

func (h *Handler) Mount(r chi.Router) {
	r.Route("/cart", func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens), auth.RequireRole(auth.RoleCustomer))
		r.Get("/", h.get)
		r.With(h.idempotency).Post("/items", h.setItem)
		r.Delete("/items/{id}", h.removeItem)
		r.With(h.idempotency).Post("/checkout", h.checkout)
		r.Post("/coupons", h.addCoupon)
		r.Delete("/coupons/{code}", h.removeCoupon)
	})
}

type couponRequest struct {
	Code string `json:"code"`
}

func (h *Handler) addCoupon(w http.ResponseWriter, r *http.Request) {
	var in couponRequest
	if err := httpx.Decode(w, r, &in); err != nil || in.Code == "" || len(in.Code) > 32 {
		httpx.ValidationError(w, map[string]string{"code": "kupon kodu gerekli"})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.svc.AddCoupon(r.Context(), p.UserID, in.Code)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) removeCoupon(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.svc.RemoveCoupon(r.Context(), p.UserID, chi.URLParam(r, "code"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.svc.Get(r.Context(), p.UserID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) setItem(w http.ResponseWriter, r *http.Request) {
	var in inventory.Item
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if fields := order.ValidateItems([]inventory.Item{in}); len(fields) > 0 {
		// items[0].x → x: tek kalemlik istekte alan adları sadeleşir.
		simple := map[string]string{}
		for k, v := range fields {
			simple[k[len("items[0]."):]] = v
		}
		httpx.ValidationError(w, simple)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.svc.SetItem(r.Context(), p.UserID, in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) removeItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, ErrItemNotFound.Error())
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	c, err := h.svc.RemoveItem(r.Context(), p.UserID, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	o, err := h.svc.Checkout(r.Context(), p.UserID, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, o)
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		stock   *order.InsufficientStockError
		coupErr *CouponError
	)
	switch {
	case errors.As(err, &coupErr):
		status := http.StatusUnprocessableEntity
		if coupErr.Reason == "not_found" {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, "coupon_"+coupErr.Reason, coupErr.Error())
	case errors.Is(err, ErrTooManyCoupons):
		httpx.Error(w, http.StatusConflict, "too_many_coupons", err.Error())
	case errors.As(err, &stock):
		httpx.ErrorWithDetails(w, http.StatusConflict, "insufficient_stock", err.Error(),
			map[string]any{"unavailable_items": stock.Failures})
	case errors.Is(err, inventory.ErrInsufficientStock):
		httpx.Error(w, http.StatusConflict, "insufficient_stock", err.Error())
	case errors.Is(err, inventory.ErrOfferNotFound), errors.Is(err, ErrItemNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case errors.Is(err, ErrEmpty):
		httpx.Error(w, http.StatusConflict, "cart_empty", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
