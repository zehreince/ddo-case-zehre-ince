package settlement

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"carsihub/internal/auth"
	"carsihub/internal/ledger"
	"carsihub/internal/platform/httpx"
)

type Handler struct {
	svc    *Service
	tokens *auth.Tokens
	log    *slog.Logger
}

func NewHandler(svc *Service, tokens *auth.Tokens, log *slog.Logger) *Handler {
	return &Handler{svc: svc, tokens: tokens, log: log}
}

// Mount: finans mizanı ve komisyon oranlarını, satıcı kendi hakediş kalemlerini görür.
func (h *Handler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Authenticate(h.tokens))
		r.With(auth.RequireRole(auth.RoleFinance, auth.RoleAdmin)).Get("/finance/ledger/balances", h.balances)
		r.With(auth.RequireRole(auth.RoleFinance, auth.RoleAdmin)).Get("/finance/commission-rates", h.rates)
		r.With(auth.RequireRole(auth.RoleSeller)).Get("/seller/settlements", h.sellerItems)
	})
}

type trialBalance struct {
	Accounts    []ledger.AccountBalance `json:"accounts"`
	DebitTotal  int64                   `json:"debit_total_kurus"`
	CreditTotal int64                   `json:"credit_total_kurus"`
	Balanced    bool                    `json:"balanced"`
}

func (h *Handler) balances(w http.ResponseWriter, r *http.Request) {
	list, err := ledger.Balances(r.Context(), h.svc.pool)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	tb := trialBalance{Accounts: list}
	for _, a := range list {
		tb.DebitTotal += a.Debit
		tb.CreditTotal += a.Credit
	}
	tb.Balanced = tb.DebitTotal == tb.CreditTotal
	httpx.JSON(w, http.StatusOK, tb)
}

type rate struct {
	CategoryID *int64     `json:"category_id"`
	RateBps    int        `json:"rate_bps"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to"`
}

func (h *Handler) rates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.pool.Query(r.Context(), `
		SELECT category_id, rate_bps, valid_from, valid_to FROM commission_rates ORDER BY category_id NULLS FIRST, valid_from`)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rate, error) {
		var x rate
		err := row.Scan(&x.CategoryID, &x.RateBps, &x.ValidFrom, &x.ValidTo)
		return x, err
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": list})
}

type item struct {
	SubOrderID       string    `json:"sub_order_id"`
	GrossKurus       int64     `json:"gross_kurus"`
	CommissionKurus  int64     `json:"commission_kurus"`
	ShippingFeeKurus int64     `json:"shipping_fee_kurus"`
	NetKurus         int64     `json:"net_kurus"`
	DeliveredAt      time.Time `json:"delivered_at"`
	DueAt            time.Time `json:"due_at"`
	Paid             bool      `json:"paid"`
}

// sellerItems satıcının hakediş kalemlerini ve defterdeki güncel alacak bakiyesini döner.
func (h *Handler) sellerItems(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var sellerID string
	if err := h.svc.pool.QueryRow(r.Context(), `SELECT id::text FROM sellers WHERE user_id = $1::uuid`, p.UserID).Scan(&sellerID); err != nil {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "bu kullanıcıya bağlı satıcı yok")
		return
	}
	rows, err := h.svc.pool.Query(r.Context(), `
		SELECT sub_order_id::text, gross_kurus, commission_kurus, shipping_fee_kurus, net_kurus, delivered_at, due_at, period_id IS NOT NULL
		FROM settlement_items WHERE seller_id = $1::uuid ORDER BY delivered_at DESC LIMIT 100`, sellerID)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (item, error) {
		var x item
		err := row.Scan(&x.SubOrderID, &x.GrossKurus, &x.CommissionKurus, &x.ShippingFeeKurus, &x.NetKurus, &x.DeliveredAt, &x.DueAt, &x.Paid)
		return x, err
	})
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	payable, err := ledger.Balance(r.Context(), h.svc.pool, ledger.SellerPayable(sellerID))
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "payable_kurus": payable})
}
