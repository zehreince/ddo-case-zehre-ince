package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"carsihub/internal/pricing"
)

var ErrInvalidReturn = errors.New("iade isteği geçersiz")

// ReturnItem iade edilmek istenen sipariş satırı ve adedidir.
type ReturnItem struct {
	OrderItemID string `json:"order_item_id"`
	Quantity    int    `json:"quantity"`
}

// RefundQuote kısmi iadenin tutarını hesaplar (T-3): siparişte uygulanmış
// kuponlar sipariş anındaki tanımlarıyla kalan satırlara yeniden uygulanır.
// Kalan sepet minimum tutarın altına düşerse kupon geri alınır ve kaybedilen
// indirim iadeden kesilir. Daha önce iade edilen adetler hesaba katılır.
//
// Yalnızca hesaplar; iadenin kendisi (para, stok, hakediş) F-7'nin işidir.
func (s *Service) RefundQuote(ctx context.Context, orderID string, items []ReturnItem) (*pricing.RefundQuote, error) {
	var status Status
	if err := s.pool.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1::uuid`, orderID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if status != StatusPaid {
		return nil, ErrNotCancellable
	}

	rows, err := s.pool.Query(ctx, `
		SELECT oi.id::text, oi.offer_id::text, so.seller_id::text, COALESCE(oi.category_id, 0), oi.quantity,
		       oi.unit_price_kurus, oi.returned_quantity
		FROM order_items oi JOIN sub_orders so ON so.id = oi.sub_order_id
		WHERE oi.order_id = $1::uuid`, orderID)
	if err != nil {
		return nil, err
	}
	var (
		lines    []pricing.Line
		returned = map[string]int{}
		offerOf  = map[string]string{}
	)
	for rows.Next() {
		var (
			itemID string
			l      pricing.Line
			ret    int
		)
		if err := rows.Scan(&itemID, &l.ID, &l.SellerID, &l.CategoryID, &l.Quantity, &l.UnitPriceKurus, &ret); err != nil {
			rows.Close()
			return nil, err
		}
		lines = append(lines, l)
		returned[l.ID] = ret
		offerOf[itemID] = l.ID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	returning := map[string]int{}
	for _, it := range items {
		offer, ok := offerOf[it.OrderItemID]
		if !ok {
			return nil, fmt.Errorf("%w: satır bu siparişe ait değil: %s", ErrInvalidReturn, it.OrderItemID)
		}
		returning[offer] += it.Quantity
	}
	if len(returning) == 0 {
		return nil, fmt.Errorf("%w: en az bir satır gerekli", ErrInvalidReturn)
	}

	crows, err := s.pool.Query(ctx, `SELECT snapshot FROM order_coupons WHERE order_id = $1::uuid ORDER BY code`, orderID)
	if err != nil {
		return nil, err
	}
	snapshots, err := pgx.CollectRows(crows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	coupons := make([]pricing.Coupon, len(snapshots))
	for i, b := range snapshots {
		if err := json.Unmarshal(b, &coupons[i]); err != nil {
			return nil, err
		}
	}

	rq, err := pricing.Refund(lines, coupons, returned, returning)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidReturn, err)
	}
	return &rq, nil
}
