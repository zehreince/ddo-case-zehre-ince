// Package notification saga'nın "satıcıya bildir" adımıdır: ödemesi alınan her
// alt sipariş için ilgili satıcıya bir bildirim düşer.
package notification

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/auth"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/httpx"
	"carsihub/internal/platform/pagination"
)

type Notification struct {
	ID         string     `json:"id"`
	SubOrderID string     `json:"sub_order_id"`
	OrderID    string     `json:"order_id"`
	Kind       string     `json:"kind"`
	TotalKurus int64      `json:"total_kurus"`
	CreatedAt  time.Time  `json:"created_at"`
	ReadAt     *time.Time `json:"read_at"`
}

type paidPayload struct {
	SubOrders []struct {
		SubOrderID string `json:"sub_order_id"`
		SellerID   string `json:"seller_id"`
	} `json:"sub_orders"`
}

// Consumer order.paid olayında satıcılara bildirim yazar. (alt sipariş, tür)
// tekil olduğu ve işlem Once ile kaydedildiği için olay tekrar gelse de tek bildirim olur.
type Consumer struct{ pool *pgxpool.Pool }

func NewConsumer(pool *pgxpool.Pool) *Consumer { return &Consumer{pool: pool} }

func (c *Consumer) Name() string { return "notification.seller" }

func (c *Consumer) Handle(ctx context.Context, e outbox.Event) error {
	var p paidPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	_, err := outbox.Once(ctx, c.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
		for _, s := range p.SubOrders {
			if _, err := tx.Exec(ctx, `
				INSERT INTO seller_notifications (seller_id, sub_order_id, kind) VALUES ($1::uuid, $2::uuid, 'new_order')
				ON CONFLICT (sub_order_id, kind) DO NOTHING`, s.SellerID, s.SubOrderID); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// Handler satıcının kendi bildirimlerini listeler.
type Handler struct {
	pool   *pgxpool.Pool
	tokens *auth.Tokens
	log    *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, tokens *auth.Tokens, log *slog.Logger) *Handler {
	return &Handler{pool: pool, tokens: tokens, log: log}
}

func (h *Handler) Mount(r chi.Router) {
	r.With(auth.Authenticate(h.tokens), auth.RequireRole(auth.RoleSeller)).Get("/seller/notifications", h.list)
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
	args := []any{p.UserID, limit + 1}
	where := ``
	if after != nil {
		where = ` AND (n.created_at, n.id) < ($3, $4::uuid)`
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT n.id::text, n.sub_order_id::text, so.order_id::text, n.kind, so.total_kurus, n.created_at, n.read_at
		FROM seller_notifications n
		JOIN sellers s ON s.id = n.seller_id
		JOIN sub_orders so ON so.id = n.sub_order_id
		WHERE s.user_id = $1::uuid`+where+`
		ORDER BY n.created_at DESC, n.id DESC LIMIT $2`, args...)
	if err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	defer rows.Close()
	page := pagination.Page[Notification]{Items: []Notification{}}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.SubOrderID, &n.OrderID, &n.Kind, &n.TotalKurus, &n.CreatedAt, &n.ReadAt); err != nil {
			httpx.Internal(w, r, h.log, err)
			return
		}
		if len(page.Items) == limit {
			prev := page.Items[limit-1]
			page.NextCursor = pagination.Cursor{CreatedAt: prev.CreatedAt, ID: prev.ID}.Encode()
			break
		}
		page.Items = append(page.Items, n)
	}
	if err := rows.Err(); err != nil {
		httpx.Internal(w, r, h.log, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}
