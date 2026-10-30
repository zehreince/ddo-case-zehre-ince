// Package settlement hakediş tahakkukudur (T-4): saga olaylarını deftere bağlar.
//
//	order.paid          → ödeme kaydı (kasa / müşteri avansı)
//	shipment.delivered  → hakediş kalemi + tahakkuk kaydı
//	                       net = brüt - komisyon - kargo, vade = teslim + N gün
//
// Komisyon oranı ürün kategorisine ve SİPARİŞ ANINA göre seçilir (zaman
// geçerlilikli); kategoriye özel oran yoksa varsayılan oran kullanılır.
package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/ledger"
	"carsihub/internal/outbox"
)

var ErrNoRate = errors.New("geçerli komisyon oranı yok")

type Service struct {
	pool        *pgxpool.Pool
	payoutDelay time.Duration
	shippingFee int64
}

func NewService(pool *pgxpool.Pool, payoutDelay time.Duration, shippingFeeKurus int64) *Service {
	return &Service{pool: pool, payoutDelay: payoutDelay, shippingFee: shippingFeeKurus}
}

// RateAt kategori için verilen anda geçerli komisyon oranını (baz puan) döner.
func RateAt(ctx context.Context, q ledger.Querier, categoryID int64, at time.Time) (int, error) {
	var bps int
	err := q.QueryRow(ctx, `
		SELECT rate_bps FROM commission_rates
		WHERE (category_id = $1 OR category_id IS NULL)
		  AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)
		ORDER BY category_id NULLS LAST
		LIMIT 1`, categoryID, at).Scan(&bps)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: kategori %d, %s", ErrNoRate, categoryID, at.Format(time.RFC3339))
	}
	return bps, err
}

// ── order.paid → ödeme kaydı ─────────────────────────────────────────────

type CaptureConsumer struct{ s *Service }

func (s *Service) CaptureConsumer() *CaptureConsumer { return &CaptureConsumer{s} }

func (c *CaptureConsumer) Name() string { return "ledger.capture" }

func (c *CaptureConsumer) Handle(ctx context.Context, e outbox.Event) error {
	var p struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	_, err := outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx, `SELECT total_kurus FROM orders WHERE id = $1::uuid`, p.OrderID).Scan(&total); err != nil {
			return err
		}
		if total == 0 { // %100 kuponlu sipariş: tahsilat yok
			return nil
		}
		_, err := ledger.Post(ctx, tx, ledger.PaymentCaptured(p.OrderID, total, e.CreatedAt))
		return err
	})
	return err
}

// ── shipment.delivered → hakediş ─────────────────────────────────────────

type AccrueConsumer struct{ s *Service }

func (s *Service) AccrueConsumer() *AccrueConsumer { return &AccrueConsumer{s} }

func (c *AccrueConsumer) Name() string { return "ledger.accrue" }

func (c *AccrueConsumer) Handle(ctx context.Context, e outbox.Event) error {
	var p struct {
		SubOrderID  string    `json:"sub_order_id"`
		DeliveredAt time.Time `json:"delivered_at"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	_, err := outbox.Once(ctx, c.s.pool, c.Name(), e.ID, func(tx pgx.Tx) error {
		return c.s.Accrue(ctx, tx, p.SubOrderID, p.DeliveredAt)
	})
	return err
}

// Accrue alt siparişin hakediş kalemini ve tahakkuk kaydını yazar (tekrar güvenli).
func (s *Service) Accrue(ctx context.Context, tx pgx.Tx, subOrderID string, deliveredAt time.Time) error {
	var (
		a         ledger.Accrual
		orderedAt time.Time
	)
	a.SubOrderID = subOrderID
	err := tx.QueryRow(ctx, `
		SELECT so.seller_id::text, so.total_kurus, so.marketplace_discount_kurus, o.created_at
		FROM sub_orders so JOIN orders o ON o.id = so.order_id WHERE so.id = $1::uuid`, subOrderID).
		Scan(&a.SellerID, &a.PaidKurus, &a.MarketplaceDiscount, &orderedAt)
	if err != nil {
		return err
	}

	// Komisyon satır bazında, satırın kategorisine göre: satıcının satış tutarı
	// (satır - satıcı indirimi) üzerinden, aşağı yuvarlanarak.
	rows, err := tx.Query(ctx, `
		SELECT COALESCE(category_id, 0), line_total_kurus - seller_discount_kurus FROM order_items WHERE sub_order_id = $1::uuid`, subOrderID)
	if err != nil {
		return err
	}
	type line struct{ cat, gross int64 }
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (line, error) {
		var l line
		err := r.Scan(&l.cat, &l.gross)
		return l, err
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		bps, err := RateAt(ctx, tx, l.cat, orderedAt)
		if err != nil {
			return err
		}
		a.CommissionKurus += l.gross * int64(bps) / 10000
	}
	a.ShippingFeeKurus = s.shippingFee

	tag, err := tx.Exec(ctx, `
		INSERT INTO settlement_items (sub_order_id, seller_id, gross_kurus, commission_kurus, shipping_fee_kurus, net_kurus, delivered_at, due_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (sub_order_id) DO NOTHING`,
		subOrderID, a.SellerID, a.Gross(), a.CommissionKurus, a.ShippingFeeKurus, a.Net(), deliveredAt, deliveredAt.Add(s.payoutDelay))
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.SubOrderDelivered(a, deliveredAt))
	return err
}
