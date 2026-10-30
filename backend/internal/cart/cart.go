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

	"carsihub/internal/coupon"
	"carsihub/internal/inventory"
	"carsihub/internal/order"
	"carsihub/internal/platform/audit"
	"carsihub/internal/pricing"
)

var (
	ErrItemNotFound   = errors.New("sepet kalemi bulunamadı")
	ErrEmpty          = errors.New("sepet boş")
	ErrTooManyCoupons = errors.New("sepete en fazla 5 kupon eklenebilir")
)

// CouponError kupon kodunun sepete neden eklenemediğini taşır.
type CouponError struct{ Reason string }

func (e *CouponError) Error() string { return "kupon kullanılamıyor: " + e.Reason }

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
	// Kupon indirimi sonrası satır tutarı (ödeme adımında kesinleşir).
	DiscountKurus int64 `json:"discount_kurus"`
	TotalKurus    int64 `json:"total_kurus"`
	sellerID      string
	categoryID    int64
}

type Cart struct {
	Items           []Item             `json:"items"`
	CouponCodes     []string           `json:"coupon_codes"`
	AppliedCoupons  []pricing.Applied  `json:"applied_coupons"`
	RejectedCoupons []pricing.Rejected `json:"rejected_coupons"`
	SubtotalKurus   int64              `json:"subtotal_kurus"`
	DiscountKurus   int64              `json:"discount_kurus"`
	TotalKurus      int64              `json:"total_kurus"`
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
		       r.status = 'active' AND r.expires_at > now(), r.expires_at, o.seller_id::text, p.category_id
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
		if err := rows.Scan(&it.ID, &it.OfferID, &it.ProductName, &it.SKU, &it.SellerName, &it.Quantity, &it.UnitPrice, &reserved, &it.ReservedUntil,
			&it.sellerID, &it.categoryID); err != nil {
			return nil, err
		}
		it.Reserved = reserved != nil && *reserved
		if !it.Reserved {
			it.ReservedUntil = nil
		}
		it.LineTotal = int64(it.Quantity) * it.UnitPrice
		c.Items = append(c.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return c, s.price(ctx, userID, c)
}

// price sepetteki kuponları güncel sepete uygular. Ön izlemedir: kullanım
// limitleri ödeme adımında ayrılır (o anda dolmuşsa kupon orada reddedilir).
func (s *Service) price(ctx context.Context, userID string, c *Cart) error {
	codes, err := s.couponCodes(ctx, userID)
	if err != nil {
		return err
	}
	resolved, rejected, err := coupon.Resolve(ctx, s.pool, codes, s.now())
	if err != nil {
		return err
	}
	lines := make([]pricing.Line, len(c.Items))
	for i, it := range c.Items {
		lines[i] = pricing.Line{ID: it.OfferID, SellerID: it.sellerID, CategoryID: it.categoryID, Quantity: it.Quantity, UnitPriceKurus: it.UnitPrice}
	}
	pcs := make([]pricing.Coupon, len(resolved))
	for i, r := range resolved {
		pcs[i] = r.Coupon
	}
	q := pricing.Calculate(lines, pcs)
	for i := range c.Items {
		if l, ok := q.Line(c.Items[i].OfferID); ok {
			c.Items[i].DiscountKurus, c.Items[i].TotalKurus = l.DiscountKurus(), l.TotalKurus
		}
	}
	c.CouponCodes = codes
	c.AppliedCoupons = q.Applied
	c.RejectedCoupons = append(rejected, q.Rejected...)
	if c.RejectedCoupons == nil {
		c.RejectedCoupons = []pricing.Rejected{}
	}
	c.SubtotalKurus, c.DiscountKurus, c.TotalKurus = q.SubtotalKurus, q.DiscountKurus, q.TotalKurus
	return nil
}

func (s *Service) couponCodes(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.code FROM cart_coupons cc JOIN coupons c ON c.id = cc.coupon_id
		WHERE cc.user_id = $1::uuid ORDER BY cc.added_at, c.code`, userID)
	if err != nil {
		return nil, err
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if codes == nil {
		codes = []string{}
	}
	return codes, err
}

// AddCoupon kodu sepete ekler. Bilinmeyen, başlamamış ya da süresi geçmiş kod
// reddedilir; minimum sepet gibi sepete bağlı koşullar sepet görünümünde
// "rejected_coupons" ile gösterilir (sepet değişince geçerli hâle gelebilir).
func (s *Service) AddCoupon(ctx context.Context, userID, code string) (*Cart, error) {
	resolved, rejected, err := coupon.Resolve(ctx, s.pool, []string{code}, s.now())
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, &CouponError{Reason: rejected[0].Reason}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM cart_coupons WHERE user_id = $1::uuid`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= order.MaxCoupons {
		return nil, ErrTooManyCoupons
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO cart_coupons (user_id, coupon_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`,
		userID, resolved[0].ID); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
}

func (s *Service) RemoveCoupon(ctx context.Context, userID, code string) (*Cart, error) {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM cart_coupons WHERE user_id = $1::uuid AND coupon_id = (SELECT id FROM coupons WHERE code = $2)`,
		userID, coupon.NormalizeCode(code)); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
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
	codes, err := s.couponCodes(ctx, userID)
	if err != nil {
		return nil, err
	}
	o, err := s.orders.PlaceInTx(ctx, tx, userID, items, requestID, order.WithCoupons(codes))
	if err != nil {
		return nil, err
	}
	// Kuponlar siparişte kullanıldı (ya da reddedildi); sepette kalmaz.
	if _, err := tx.Exec(ctx, `DELETE FROM cart_coupons WHERE user_id = $1::uuid`, userID); err != nil {
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
