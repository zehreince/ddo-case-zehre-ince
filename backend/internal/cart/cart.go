// Package cart müşteri sepetini yönetir (F-2). Sepete eklenen her kalem süreli
// bir stok rezervasyonuyla (TTL) tutulur; süre dolunca süpürücü stoğu bırakır,
// kalem sepette kalır ve ödeme adımında yeniden rezerve edilmeye çalışılır.
package cart

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/audit"
)

var (
	ErrItemNotFound = errors.New("sepet kalemi bulunamadı")
	ErrEmpty        = errors.New("sepet boş")
)

type Item struct {
	ID            string     `json:"id"`
	OfferID       string     `json:"offer_id"`
	ProductName   string     `json:"product_name"`
	SKU           string     `json:"sku"`
	SellerName    string     `json:"seller_name"`
	Quantity      int        `json:"quantity"`
	UnitPrice     int64      `json:"unit_price_kurus"`
	LineTotal     int64      `json:"line_total_kurus"`
	Reserved      bool       `json:"reserved"`
	ReservedUntil *time.Time `json:"reserved_until"`
}

type Cart struct {
	Items         []Item `json:"items"`
	SubtotalKurus int64  `json:"subtotal_kurus"`
}

type Service struct {
	pool   *pgxpool.Pool
	orders *order.Service
	ttl    time.Duration
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, orders *order.Service, ttl time.Duration) *Service {
	return &Service{pool: pool, orders: orders, ttl: ttl, now: time.Now}
}

func (s *Service) Get(ctx context.Context, userID string) (*Cart, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ci.id::text, ci.offer_id::text, p.name, v.sku, se.name, ci.quantity, o.price_kurus,
		       r.status = 'active' AND r.expires_at > now(), r.expires_at
		FROM cart_items ci
		JOIN offers o ON o.id = ci.offer_id
		JOIN sellers se ON se.id = o.seller_id
		JOIN product_variants v ON v.id = o.variant_id
		JOIN products p ON p.id = v.product_id
		LEFT JOIN stock_reservations r ON r.id = ci.reservation_id
		WHERE ci.user_id = $1::uuid
		ORDER BY ci.created_at, ci.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	c := &Cart{Items: []Item{}}
	for rows.Next() {
		var it Item
		var reserved *bool
		if err := rows.Scan(&it.ID, &it.OfferID, &it.ProductName, &it.SKU, &it.SellerName, &it.Quantity, &it.UnitPrice, &reserved, &it.ReservedUntil); err != nil {
			return nil, err
		}
		it.Reserved = reserved != nil && *reserved
		if !it.Reserved {
			it.ReservedUntil = nil
		}
		it.LineTotal = int64(it.Quantity) * it.UnitPrice
		c.SubtotalKurus += it.LineTotal
		c.Items = append(c.Items, it)
	}
	return c, rows.Err()
}

// SetItem kalemi sepete ekler ya da adedini değiştirir. Eski rezervasyon
// bırakılıp yeni adet için yeniden rezerve edilir; ikisi aynı transaction'da
// ve offer kilidi altında olduğu için arada stoğu başkası kapamaz.
func (s *Service) SetItem(ctx context.Context, userID string, it inventory.Item) (*Cart, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := inventory.LockOffers(ctx, tx, []string{it.OfferID}); err != nil {
		return nil, err
	}
	var oldReservation *string
	err = tx.QueryRow(ctx, `SELECT reservation_id::text FROM cart_items WHERE user_id = $1::uuid AND offer_id = $2::uuid`,
		userID, it.OfferID).Scan(&oldReservation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if oldReservation != nil {
		if _, err := inventory.Release(ctx, tx, []string{*oldReservation}, "released"); err != nil {
			return nil, err
		}
	}
	r, err := inventory.Reserve(ctx, tx, userID, it, s.now().Add(s.ttl))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart_items (user_id, offer_id, quantity, reservation_id) VALUES ($1::uuid, $2::uuid, $3, $4::uuid)
		ON CONFLICT (user_id, offer_id) DO UPDATE SET quantity = EXCLUDED.quantity, reservation_id = EXCLUDED.reservation_id`,
		userID, it.OfferID, it.Quantity, r.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
}

func (s *Service) RemoveItem(ctx context.Context, userID, itemID string) (*Cart, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var offerID string
	var reservation *string
	err = tx.QueryRow(ctx, `SELECT offer_id::text, reservation_id::text FROM cart_items WHERE id = $1::uuid AND user_id = $2::uuid`,
		itemID, userID).Scan(&offerID, &reservation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := inventory.LockOffers(ctx, tx, []string{offerID}); err != nil {
		return nil, err
	}
	if reservation != nil {
		if _, err := inventory.Release(ctx, tx, []string{*reservation}, "released"); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE id = $1::uuid`, itemID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
}

// Checkout sepetten sipariş oluşturur. Sepet rezervasyonları bırakılıp aynı
// transaction'da sipariş için (ödeme süresi boyunca) yeniden rezerve edilir;
// offer kilitleri baştan alındığı için arada stok kaybedilmez. Süresi dolmuş
// kalemler de bu sırada yeniden denenir.
//
// Kısmi satış kuralı order servisinin politikasıdır: stoğu yetmeyen kalemler
// "allow"da sepette kalır ve yanıtta listelenir; "reject"te hiçbir şey
// değişmez (transaction geri alınır, sepet rezervasyonları korunur).
func (s *Service) Checkout(ctx context.Context, userID, requestID string) (*order.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT offer_id::text, quantity, reservation_id::text FROM cart_items WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return nil, err
	}
	var (
		items        []inventory.Item
		offerIDs     []string
		reservations []string
	)
	for rows.Next() {
		var it inventory.Item
		var res *string
		if err := rows.Scan(&it.OfferID, &it.Quantity, &res); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
		offerIDs = append(offerIDs, it.OfferID)
		if res != nil {
			reservations = append(reservations, *res)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrEmpty
	}

	if err := inventory.LockOffers(ctx, tx, offerIDs); err != nil {
		return nil, err
	}
	if _, err := inventory.Release(ctx, tx, reservations, "released"); err != nil {
		return nil, err
	}
	o, err := s.orders.PlaceInTx(ctx, tx, userID, items, requestID)
	if err != nil {
		return nil, err
	}

	failed := make([]string, 0, len(o.Unavailable))
	for _, f := range o.Unavailable {
		failed = append(failed, f.OfferID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE user_id = $1::uuid AND NOT (offer_id = ANY($2::uuid[]))`, userID, failed); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE cart_items SET reservation_id = NULL WHERE user_id = $1::uuid`, userID); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: userID, ActorRole: "customer", Action: "cart.checked_out",
		EntityType: "order", EntityID: o.ID, RequestID: requestID,
		New: map[string]any{"items": len(items), "left_in_cart": len(failed)},
	}); err != nil {
		return nil, err
	}
	return o, tx.Commit(ctx)
}
