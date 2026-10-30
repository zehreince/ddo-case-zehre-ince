package order

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/coupon"
	"carsihub/internal/inventory"
	"carsihub/internal/outbox"
	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/pagination"
	"carsihub/internal/pricing"
)

// InsufficientStockError hangi kalemlerin neden rezerve edilemediğini taşır.
type InsufficientStockError struct {
	Failures []inventory.Failure
}

func (e *InsufficientStockError) Error() string { return ErrInsufficientStock.Error() }
func (e *InsufficientStockError) Unwrap() error { return ErrInsufficientStock }

type Service struct {
	pool       *pgxpool.Pool
	policy     inventory.Policy
	paymentTTL time.Duration
	now        func() time.Time
}

func NewService(pool *pgxpool.Pool, policy inventory.Policy, paymentTTL time.Duration) *Service {
	return &Service{pool: pool, policy: policy, paymentTTL: paymentTTL, now: time.Now}
}

func (s *Service) Policy() inventory.Policy { return s.policy }

// Place doğrudan verilen kalemlerden sipariş oluşturur ("hemen al").
func (s *Service) Place(ctx context.Context, customerID string, items []inventory.Item, requestID string, opts ...PlaceOption) (*Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := s.PlaceInTx(ctx, tx, customerID, items, requestID, opts...)
	if err != nil {
		return nil, err
	}
	return o, tx.Commit(ctx)
}

// PlaceInTx stoğu ödeme süresi boyunca rezerve eder, kuponları uygular ve
// siparişi oluşturur. Sepetten ödeme de aynı yolu kullanır. Hata dönerse
// çağıran geri almalıdır.
//
// Kuponlar rezervasyondan SONRA hesaplanır: kısmi satışta satılamayan kalem
// düşünce minimum sepet şartı bozulabilir; kupon o durumda reddedilir ve
// yanıtta nedeniyle gösterilir.
func (s *Service) PlaceInTx(ctx context.Context, tx pgx.Tx, customerID string, items []inventory.Item, requestID string, opts ...PlaceOption) (*Order, error) {
	var po placeOpts
	for _, opt := range opts {
		opt(&po)
	}
	deadline := s.now().Add(s.paymentTTL)
	reserved, failed, err := inventory.ReserveBatch(ctx, tx, customerID, items, deadline)
	if err != nil {
		return nil, err
	}
	if len(reserved) == 0 || (len(failed) > 0 && s.policy == inventory.PolicyReject) {
		return nil, &InsufficientStockError{Failures: failed}
	}

	subs, subtotal := Split(reserved)
	o := &Order{
		CustomerID: customerID, Status: StatusPendingPayment,
		SubtotalKurus: subtotal, TotalKurus: subtotal, PaymentDeadline: deadline,
		Unavailable: failed,
	}
	// Sipariş kimliği kupon kullanımının yabancı anahtarıdır; tutarlar fiyat
	// hesabından sonra güncellenir.
	if err := tx.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, subtotal_kurus, discount_kurus, total_kurus, payment_deadline)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)
		RETURNING id::text, created_at`,
		customerID, o.Status, o.SubtotalKurus, o.DiscountKurus, o.TotalKurus, o.PaymentDeadline).
		Scan(&o.ID, &o.CreatedAt); err != nil {
		return nil, err
	}

	pls, categories, err := lines(ctx, tx, reserved)
	if err != nil {
		return nil, err
	}
	priced, err := s.price(ctx, tx, customerID, o.ID, pls, po.coupons)
	if err != nil {
		return nil, err
	}
	if err := applyQuote(subs, priced.quote, categories); err != nil {
		return nil, err
	}
	o.DiscountKurus, o.TotalKurus = priced.quote.DiscountKurus, priced.quote.TotalKurus
	o.AppliedCoupons, o.RejectedCoupons = priced.quote.Applied, priced.rejected
	if _, err := tx.Exec(ctx, `UPDATE orders SET discount_kurus = $2, total_kurus = $3 WHERE id = $1::uuid`,
		o.ID, o.DiscountKurus, o.TotalKurus); err != nil {
		return nil, err
	}
	for i, c := range priced.applied {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_coupons (order_id, coupon_id, code, snapshot, discount_kurus) VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
			o.ID, c.ID, c.Code, c.Coupon, priced.quote.Applied[i].DiscountKurus); err != nil {
			return nil, err
		}
	}

	reservationIDs := make([]string, 0, len(reserved))
	for i := range subs {
		sub := &subs[i]
		if err := tx.QueryRow(ctx, `
			INSERT INTO sub_orders (order_id, seller_id, status, subtotal_kurus, discount_kurus, total_kurus,
			                        marketplace_discount_kurus, seller_discount_kurus)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			o.ID, sub.SellerID, sub.Status, sub.SubtotalKurus, sub.DiscountKurus, sub.TotalKurus,
			sub.MarketplaceDiscountKurus, sub.SellerDiscountKurus).Scan(&sub.ID); err != nil {
			return nil, err
		}
		for j := range sub.Items {
			line := &sub.Items[j]
			if err := tx.QueryRow(ctx, `
				INSERT INTO order_items (order_id, sub_order_id, offer_id, reservation_id, quantity, unit_price_kurus, line_total_kurus,
				                         category_id, marketplace_discount_kurus, seller_discount_kurus)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10) RETURNING id::text`,
				o.ID, sub.ID, line.OfferID, line.ReservationID, line.Quantity, line.UnitPriceKurus, line.LineTotalKurus,
				line.CategoryID, line.MarketplaceDiscountKurus, line.SellerDiscountKurus).
				Scan(&line.ID); err != nil {
				return nil, err
			}
			reservationIDs = append(reservationIDs, line.ReservationID)
		}
	}
	o.SubOrders = subs

	if _, err := tx.Exec(ctx, `UPDATE stock_reservations SET order_id = $1::uuid WHERE id = ANY($2::uuid[])`, o.ID, reservationIDs); err != nil {
		return nil, err
	}
	// Kupon tutarın tamamını karşıladıysa tahsil edilecek bir şey yok: sipariş
	// doğrudan ödenmiş sayılır ve saga sonraki adımdan (bildirim, kargo) devam eder.
	if o.TotalKurus == 0 {
		if err := s.MarkPaidTx(ctx, tx, o.ID, StatusPendingPayment); err != nil {
			return nil, err
		}
		o.Status = StatusPaid
		return o, audit.Record(ctx, tx, audit.Entry{
			ActorID: customerID, ActorRole: "customer", Action: "order.placed", EntityType: "order", EntityID: o.ID,
			RequestID: requestID, New: map[string]any{"status": o.Status, "total_kurus": 0, "coupons": o.AppliedCoupons},
		})
	}
	// Ödeme kaydı ve saga'yı başlatan olay siparişle aynı transaction'da.
	var paymentID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO payments (order_id, amount_kurus, status) VALUES ($1::uuid, $2, 'pending') RETURNING id::text`,
		o.ID, o.TotalKurus).Scan(&paymentID); err != nil {
		return nil, err
	}
	if err := outbox.Write(ctx, tx, EventPlaced, "order", o.ID, map[string]any{
		"order_id": o.ID, "payment_id": paymentID, "total_kurus": o.TotalKurus,
	}); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: customerID, ActorRole: "customer", Action: "order.placed",
		EntityType: "order", EntityID: o.ID, RequestID: requestID,
		New: map[string]any{
			"status": o.Status, "total_kurus": o.TotalKurus, "discount_kurus": o.DiscountKurus,
			"sub_orders": len(subs), "unavailable": failed, "coupons": o.AppliedCoupons, "rejected_coupons": o.RejectedCoupons,
		},
	}); err != nil {
		return nil, err
	}
	return o, nil
}

// Cancel ödeme bekleyen siparişi müşteri isteğiyle iptal eder ve stoğu iade eder.
func (s *Service) Cancel(ctx context.Context, customerID, orderID, requestID string) (*Order, error) {
	err := s.inLockedOrder(ctx, orderID, func(tx pgx.Tx, owner string, status Status) error {
		if owner != customerID {
			return ErrNotFound // başkasının siparişinin varlığı sızdırılmaz
		}
		if status != StatusPendingPayment {
			return ErrNotCancellable
		}
		return s.close(ctx, tx, orderID, StatusCancelled, "released", audit.Entry{
			ActorID: customerID, ActorRole: "customer", Action: EventCancelled, RequestID: requestID,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, orderID)
}

// MarkPaid ödeme başarılı olduğunda stoğu kesinleştirir. Tekrar çağrılması
// güvenlidir (çift ödeme bildirimi): zaten ödenmişse hiçbir şey yapmaz.
func (s *Service) MarkPaid(ctx context.Context, orderID string) error {
	return s.inLockedOrder(ctx, orderID, func(tx pgx.Tx, _ string, status Status) error {
		return s.MarkPaidTx(ctx, tx, orderID, status)
	})
}

// MarkPaidTx MarkPaid'in transaction içi hâlidir; sipariş LockTx ile kilitlenmiş olmalıdır.
// Sipariş kapanmışsa (iptal, süre dolumu) ErrNotCancellable döner; çağıran iade başlatır.
func (s *Service) MarkPaidTx(ctx context.Context, tx pgx.Tx, orderID string, status Status) error {
	switch status {
	case StatusPaid:
		return nil
	case StatusPendingPayment:
	default:
		return ErrNotCancellable
	}
	ids, err := reservationIDs(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if err := inventory.Commit(ctx, tx, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'paid', updated_at = now() WHERE id = $1::uuid`, orderID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		UPDATE sub_orders SET status = 'awaiting_shipment' WHERE order_id = $1::uuid
		RETURNING id::text, seller_id::text`, orderID)
	if err != nil {
		return err
	}
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]string, error) {
		var id, seller string
		err := r.Scan(&id, &seller)
		return map[string]string{"sub_order_id": id, "seller_id": seller}, err
	})
	if err != nil {
		return err
	}
	if err := outbox.Write(ctx, tx, EventPaid, "order", orderID, map[string]any{"order_id": orderID, "sub_orders": subs}); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{
		Action: EventPaid, EntityType: "order", EntityID: orderID,
		Old: map[string]any{"status": status}, New: map[string]any{"status": StatusPaid},
	})
}

// FailPayment ödeme başarısız olduğunda siparişi kapatır ve stoğu iade eder.
// Tekrar çağrılması güvenlidir.
func (s *Service) FailPayment(ctx context.Context, orderID, reason string) error {
	return s.inLockedOrder(ctx, orderID, func(tx pgx.Tx, _ string, status Status) error {
		return s.FailPaymentTx(ctx, tx, orderID, status, reason)
	})
}

// FailPaymentTx FailPayment'in transaction içi hâlidir (telafi: stok iadesi).
func (s *Service) FailPaymentTx(ctx context.Context, tx pgx.Tx, orderID string, status Status, reason string) error {
	if status != StatusPendingPayment {
		return nil
	}
	return s.close(ctx, tx, orderID, StatusCancelled, "released", audit.Entry{
		Action: EventPaymentFailed, New: map[string]any{"reason": reason},
	})
}

// ExpireUnpaid ödeme süresi dolan siparişleri kapatır ve stoklarını iade eder.
// Siparişler FOR UPDATE SKIP LOCKED ile seçilir: o anda ödeme sonucu işlenen
// bir sipariş beklenmez, bir sonraki turda yeniden değerlendirilir.
func (s *Service) ExpireUnpaid(ctx context.Context, limit int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id::text FROM orders
		WHERE status = 'pending_payment' AND payment_deadline <= $1
		ORDER BY payment_deadline
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, s.now(), limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.close(ctx, tx, id, StatusExpired, "expired", audit.Entry{Action: EventExpired}); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

// close siparişin rezervasyonlarını bırakır, sipariş ile alt siparişlerin
// durumunu günceller ve audit kaydının eylem adıyla bir outbox olayı yazar.
func (s *Service) close(ctx context.Context, tx pgx.Tx, orderID string, to Status, reservationStatus string, e audit.Entry) error {
	ids, err := reservationIDs(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if _, err := inventory.Release(ctx, tx, ids, reservationStatus); err != nil {
		return err
	}
	if err := coupon.Release(ctx, tx, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET status = $2, updated_at = now() WHERE id = $1::uuid`, orderID, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sub_orders SET status = $2 WHERE order_id = $1::uuid`, orderID, to); err != nil {
		return err
	}
	e.EntityType, e.EntityID = "order", orderID
	if e.New == nil {
		e.New = map[string]any{}
	}
	if m, ok := e.New.(map[string]any); ok {
		m["status"] = to
		m["released_reservations"] = len(ids)
	}
	e.Old = map[string]any{"status": StatusPendingPayment}
	if err := outbox.Write(ctx, tx, e.Action, "order", orderID, map[string]any{"order_id": orderID, "status": to}); err != nil {
		return err
	}
	return audit.Record(ctx, tx, e)
}

// LockTx siparişi kilitler (global kilit sırasının ilk halkası) ve sahibini, durumunu döner.
func LockTx(ctx context.Context, tx pgx.Tx, orderID string) (owner string, status Status, err error) {
	err = tx.QueryRow(ctx, `SELECT customer_id::text, status FROM orders WHERE id = $1::uuid FOR UPDATE`, orderID).Scan(&owner, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return owner, status, err
}

// inLockedOrder siparişi kilitleyip fn'i aynı transaction'da çalıştırır.
func (s *Service) inLockedOrder(ctx context.Context, orderID string, fn func(tx pgx.Tx, owner string, status Status) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	owner, status, err := LockTx(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if err := fn(tx, owner, status); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func reservationIDs(ctx context.Context, tx pgx.Tx, orderID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT reservation_id::text FROM order_items WHERE order_id = $1::uuid`, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Get siparişi alt siparişleri ve kalemleriyle döner.
func (s *Service) Get(ctx context.Context, orderID string) (*Order, error) {
	o := &Order{ID: orderID}
	err := s.pool.QueryRow(ctx, `
		SELECT customer_id::text, status, subtotal_kurus, discount_kurus, total_kurus, payment_deadline, created_at
		FROM orders WHERE id = $1::uuid`, orderID).
		Scan(&o.CustomerID, &o.Status, &o.SubtotalKurus, &o.DiscountKurus, &o.TotalKurus, &o.PaymentDeadline, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	crows, err := s.pool.Query(ctx, `
		SELECT oc.coupon_id::text, oc.code, oc.snapshot->>'scope', oc.discount_kurus
		FROM order_coupons oc WHERE oc.order_id = $1::uuid ORDER BY oc.code`, orderID)
	if err != nil {
		return nil, err
	}
	if o.AppliedCoupons, err = pgx.CollectRows(crows, func(r pgx.CollectableRow) (pricing.Applied, error) {
		var a pricing.Applied
		err := r.Scan(&a.CouponID, &a.Code, &a.Scope, &a.DiscountKurus)
		return a, err
	}); err != nil {
		return nil, err
	}

	var pay PaymentInfo
	err = s.pool.QueryRow(ctx, `SELECT id::text, status, failure_reason, callback_count FROM payments WHERE order_id = $1::uuid`, orderID).
		Scan(&pay.ID, &pay.Status, &pay.FailureReason, &pay.CallbackCount)
	switch {
	case err == nil:
		o.Payment = &pay
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT so.id::text, so.seller_id::text, se.name, so.status, so.subtotal_kurus, so.discount_kurus, so.total_kurus,
		       so.marketplace_discount_kurus, so.seller_discount_kurus,
		       sh.tracking_number, sh.status,
		       oi.id::text, oi.offer_id::text, oi.quantity, oi.unit_price_kurus, oi.line_total_kurus,
		       oi.marketplace_discount_kurus, oi.seller_discount_kurus, oi.paid_kurus, oi.returned_quantity, oi.category_id
		FROM sub_orders so
		JOIN sellers se ON se.id = so.seller_id
		JOIN order_items oi ON oi.sub_order_id = so.id
		LEFT JOIN shipments sh ON sh.sub_order_id = so.id
		WHERE so.order_id = $1::uuid
		ORDER BY so.seller_id, oi.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			sub         SubOrder
			l           Line
			tracking    *string
			shipmentSts *string
		)
		if err := rows.Scan(&sub.ID, &sub.SellerID, &sub.SellerName, &sub.Status, &sub.SubtotalKurus, &sub.DiscountKurus, &sub.TotalKurus,
			&sub.MarketplaceDiscountKurus, &sub.SellerDiscountKurus,
			&tracking, &shipmentSts,
			&l.ID, &l.OfferID, &l.Quantity, &l.UnitPriceKurus, &l.LineTotalKurus,
			&l.MarketplaceDiscountKurus, &l.SellerDiscountKurus, &l.PaidKurus, &l.ReturnedQuantity, &l.CategoryID); err != nil {
			return nil, err
		}
		if shipmentSts != nil {
			sub.Shipment = &ShipmentInfo{TrackingNumber: tracking, Status: *shipmentSts}
		}
		if n := len(o.SubOrders); n == 0 || o.SubOrders[n-1].ID != sub.ID {
			o.SubOrders = append(o.SubOrders, sub)
		}
		last := &o.SubOrders[len(o.SubOrders)-1]
		last.Items = append(last.Items, l)
	}
	return o, rows.Err()
}

// List müşterinin siparişlerini yeniden eskiye, keyset sayfalamayla döner (kalemsiz özet).
func (s *Service) List(ctx context.Context, customerID string, after *pagination.Cursor, limit int) (pagination.Page[Order], error) {
	args := []any{customerID, limit + 1}
	where := `customer_id = $1::uuid`
	if after != nil {
		where += ` AND (created_at, id) < ($3, $4::uuid)`
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, customer_id::text, status, subtotal_kurus, discount_kurus, total_kurus, payment_deadline, created_at
		FROM orders WHERE `+where+`
		ORDER BY created_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		return pagination.Page[Order]{}, err
	}
	defer rows.Close()

	page := pagination.Page[Order]{Items: []Order{}}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.Status, &o.SubtotalKurus, &o.DiscountKurus, &o.TotalKurus, &o.PaymentDeadline, &o.CreatedAt); err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			prev := page.Items[limit-1]
			page.NextCursor = pagination.Cursor{CreatedAt: prev.CreatedAt, ID: prev.ID}.Encode()
			break
		}
		page.Items = append(page.Items, o)
	}
	return page, rows.Err()
}
