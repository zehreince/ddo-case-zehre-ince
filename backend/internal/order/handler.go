package order

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"carsihub/internal/auth"
	"carsihub/internal/inventory"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/pagination"
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
	r.Route("/orders", func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens))
		r.With(auth.RequireRole(auth.RoleCustomer), h.idempotency).Post("/", h.place)
		r.With(auth.RequireRole(auth.RoleCustomer)).Get("/", h.list)
		r.With(auth.RequireRole(auth.RoleCustomer, auth.RoleOperations, auth.RoleAdmin)).Get("/{id}", h.get)
		r.With(auth.RequireRole(auth.RoleCustomer), h.idempotency).Post("/{id}/cancel", h.cancel)
	})
}

type placeRequest struct {
	Items []inventory.Item `json:"items"`
}

func (h *Handler) place(w http.ResponseWriter, r *http.Request) {
	var in placeRequest
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "gövde geçerli JSON değil")
		return
	}
	if fields := ValidateItems(in.Items); len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	o, err := h.svc.Place(r.Context(), p.UserID, in.Items, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, o)
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
	page, err := h.svc.List(r.Context(), p.UserID, after, limit)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := orderID(w, r)
	if !ok {
		return
	}
	o, err := h.svc.Get(r.Context(), id)
	p, _ := auth.PrincipalFrom(r.Context())
	// Müşteri yalnızca kendi siparişini görür; başkasınınki "bulunamadı" sayılır.
	if err == nil && p.Role == auth.RoleCustomer && o.CustomerID != p.UserID {
		err = ErrNotFound
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := orderID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	o, err := h.svc.Cancel(r.Context(), p.UserID, id, httpx.RequestIDFrom(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

// orderID yol parametresini doğrular; bozuk kimlik veritabanına gitmeden 404 olur.
func orderID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, ErrNotFound.Error())
		return "", false
	}
	return id, true
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var stock *InsufficientStockError
	switch {
	case errors.As(err, &stock):
		httpx.ErrorWithDetails(w, http.StatusConflict, "insufficient_stock", err.Error(),
			map[string]any{"unavailable_items": stock.Failures, "policy": h.svc.Policy()})
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, err.Error())
	case errors.Is(err, ErrNotCancellable):
		httpx.Error(w, http.StatusConflict, "invalid_state", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}
