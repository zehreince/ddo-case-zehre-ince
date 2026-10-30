// Package inventory satıcı × varyant (offer) bazlı stoğu ve stok
// rezervasyonlarını yönetir (T-1).
//
// Oversell önleme tek bir koşullu UPDATE'e dayanır:
//
//	UPDATE offers SET stock_reserved = stock_reserved + q
//	WHERE id = $1 AND stock_on_hand - stock_reserved >= q
//
// PostgreSQL aynı satırı güncelleyen eşzamanlı işlemleri satır kilidiyle
// sıraya sokar ve bekleyen işlem kilidi aldığında WHERE koşulunu güncel satır
// üzerinde yeniden değerlendirir. Böylece iki işlem aynı son birimi alamaz.
// offers tablosundaki CHECK (stock_reserved <= stock_on_hand) kısıtı ikinci
// savunma hattıdır.
//
// Kilit sırası (deadlock önleme) sistem genelinde aynıdır:
//
//	orders → offers (id sırasıyla, LockOffers) → stock_reservations
//
// Rezervasyonu değiştiren her işlem önce ilgili offer'ları kilitler, sonra
// rezervasyonun durumunu kilit altında yeniden kontrol eder.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInsufficientStock    = errors.New("yeterli stok yok")
	ErrOfferNotFound        = errors.New("satıştaki ürün bulunamadı")
	ErrReservationNotActive = errors.New("rezervasyon artık geçerli değil")
)

// Policy çok satıcılı sepette bazı kalemlerin stoğu yetmediğinde ne olacağını belirler.
type Policy string

const (
	// PolicyAllowPartial stoğu olan kalemler satılır, olmayanlar siparişten
	// çıkarılır ve yanıtta listelenir. Hiçbir kalem rezerve edilemezse sipariş oluşmaz.
	PolicyAllowPartial Policy = "allow"
	// PolicyReject tek bir kalem bile rezerve edilemezse hiçbiri rezerve edilmez.
	PolicyReject Policy = "reject"
)

func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(s); p {
	case PolicyAllowPartial, PolicyReject:
		return p, nil
	}
	return "", fmt.Errorf("kısmi satış kuralı allow ya da reject olmalı: %q", s)
}

type Item struct {
	OfferID  string `json:"offer_id"`
	Quantity int    `json:"quantity"`
}

type Reservation struct {
	ID             string
	OfferID        string
	SellerID       string
	Quantity       int
	UnitPriceKurus int64
	ExpiresAt      time.Time
}

// Failure rezerve edilemeyen bir kalemdir.
type Failure struct {
	OfferID   string `json:"offer_id"`
	Requested int    `json:"requested"`
	Available int    `json:"available"`
	Reason    string `json:"reason"` // insufficient_stock | offer_not_found
}

// MergeItems aynı offer'a ait kalemleri birleştirir ve offer id'ye göre sıralar.
// Sıralama kilit sırasını belirler (deadlock önleme).
func MergeItems(items []Item) []Item {
	byOffer := map[string]int{}
	for _, it := range items {
		byOffer[it.OfferID] += it.Quantity
	}
	out := make([]Item, 0, len(byOffer))
	for id, q := range byOffer {
		out = append(out, Item{OfferID: id, Quantity: q})
	}
	slices.SortFunc(out, func(a, b Item) int {
		switch {
		case a.OfferID < b.OfferID:
			return -1
		case a.OfferID > b.OfferID:
			return 1
		}
		return 0
	})
	return out
}

// LockOffers offer satırlarını id sırasıyla kilitler. Birden çok offer'a
// dokunacak her transaction önce bunu çağırmalıdır.
func LockOffers(ctx context.Context, tx pgx.Tx, offerIDs []string) error {
	ids := slices.Clone(offerIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM offers WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	return err
}

// Reserve tek bir kalem için stok ayırır. Stok yetmezse ErrInsufficientStock.
func Reserve(ctx context.Context, tx pgx.Tx, userID string, it Item, expiresAt time.Time) (*Reservation, error) {
	if it.Quantity <= 0 {
		return nil, fmt.Errorf("adet pozitif olmalı: %d", it.Quantity)
	}
	r := Reservation{OfferID: it.OfferID, Quantity: it.Quantity, ExpiresAt: expiresAt}
	err := tx.QueryRow(ctx, `
		UPDATE offers
		SET stock_reserved = stock_reserved + $2, updated_at = now()
		WHERE id = $1::uuid AND active AND stock_on_hand - stock_reserved >= $2
		RETURNING seller_id::text, price_kurus`, it.OfferID, it.Quantity).
		Scan(&r.SellerID, &r.UnitPriceKurus)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM offers WHERE id = $1::uuid AND active)`, it.OfferID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrOfferNotFound
		}
		return nil, ErrInsufficientStock
	}
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO stock_reservations (offer_id, user_id, quantity, status, expires_at)
		VALUES ($1::uuid, $2::uuid, $3, 'active', $4)
		RETURNING id::text`, it.OfferID, userID, it.Quantity, expiresAt).Scan(&r.ID)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ReserveBatch kalemleri tek transaction içinde rezerve eder. Her kalem kendi
// savepoint'inde denenir; böylece başarısız bir kalem diğerlerini geri almaz.
// Hangi kalemlerin tutulacağına politika göre çağıran karar verir:
// PolicyReject'te Failures boş değilse transaction geri alınmalıdır.
// Tüm başarısız kalemler toplanır ki istemciye tek seferde bildirilsin.
func ReserveBatch(ctx context.Context, tx pgx.Tx, userID string, items []Item, expiresAt time.Time) ([]Reservation, []Failure, error) {
	items = MergeItems(items)
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.OfferID
	}
	if err := LockOffers(ctx, tx, ids); err != nil {
		return nil, nil, err
	}

	var (
		reserved []Reservation
		failed   []Failure
	)
	for _, it := range items {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		r, err := Reserve(ctx, sp, userID, it, expiresAt)
		switch {
		case err == nil:
			if err := sp.Commit(ctx); err != nil {
				return nil, nil, err
			}
			reserved = append(reserved, *r)
		case errors.Is(err, ErrInsufficientStock), errors.Is(err, ErrOfferNotFound):
			if err := sp.Rollback(ctx); err != nil {
				return nil, nil, err
			}
			f := Failure{OfferID: it.OfferID, Requested: it.Quantity, Reason: "insufficient_stock"}
			if errors.Is(err, ErrOfferNotFound) {
				f.Reason = "offer_not_found"
			} else if f.Available, err = available(ctx, tx, it.OfferID); err != nil {
				return nil, nil, err
			}
			failed = append(failed, f)
		default:
			_ = sp.Rollback(ctx)
			return nil, nil, err
		}
	}
	return reserved, failed, nil
}

func available(ctx context.Context, tx pgx.Tx, offerID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT stock_on_hand - stock_reserved FROM offers WHERE id = $1::uuid`, offerID).Scan(&n)
	return n, err
}

// Release aktif rezervasyonları bırakır (sepetten çıkarma, iptal, ödeme hatası).
// Aktif olmayanlara dokunmaz; tekrar çağrılması güvenlidir. Bırakılan sayıyı döner.
func Release(ctx context.Context, tx pgx.Tx, reservationIDs []string, status string) (int, error) {
	if status != "released" && status != "expired" {
		return 0, fmt.Errorf("geçersiz bırakma durumu: %q", status)
	}
	return settle(ctx, tx, reservationIDs, status, `stock_reserved = o.stock_reserved - agg.q`)
}

// Commit ödemesi alınmış rezervasyonları kesinleştirir: stok eldeki miktardan
// düşülür. Rezervasyonlardan biri bile aktif değilse ErrReservationNotActive;
// çağıran transaction'ı geri almalıdır.
func Commit(ctx context.Context, tx pgx.Tx, reservationIDs []string) error {
	n, err := settle(ctx, tx, reservationIDs, "committed",
		`stock_on_hand = o.stock_on_hand - agg.q, stock_reserved = o.stock_reserved - agg.q`)
	if err != nil {
		return err
	}
	if n != len(slices.Compact(slices.Sorted(slices.Values(reservationIDs)))) {
		return ErrReservationNotActive
	}
	return nil
}

func settle(ctx context.Context, tx pgx.Tx, reservationIDs []string, status, offerUpdate string) (int, error) {
	if len(reservationIDs) == 0 {
		return 0, nil
	}
	var offerIDs []string
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT offer_id::text FROM stock_reservations
		WHERE id = ANY($1::uuid[]) AND status = 'active'`, reservationIDs)
	if err != nil {
		return 0, err
	}
	if offerIDs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return 0, err
	}
	if err := LockOffers(ctx, tx, offerIDs); err != nil {
		return 0, err
	}

	var n int
	err = tx.QueryRow(ctx, `
		WITH r AS (
			UPDATE stock_reservations SET status = $2, updated_at = now()
			WHERE id = ANY($1::uuid[]) AND status = 'active'
			RETURNING offer_id, quantity
		), agg AS (
			SELECT offer_id, sum(quantity)::int AS q, count(*) AS n FROM r GROUP BY offer_id
		), upd AS (
			UPDATE offers o SET `+offerUpdate+`, updated_at = now()
			FROM agg WHERE o.id = agg.offer_id
			RETURNING 1
		)
		SELECT coalesce(sum(n), 0)::int FROM agg`, reservationIDs, status).Scan(&n)
	return n, err
}

// ExpireCartReservations süresi dolmuş sepet rezervasyonlarını bırakır
// (siparişe bağlı olanlar sipariş süpürücüsünün işidir).
//
// Adaylar kilitsiz seçilir; kilitler Release içinde global sırayla (önce
// offer, sonra rezervasyon) alınır ve durum kilit altında yeniden kontrol
// edilir. Rezervasyonu önce kilitlemek (FOR UPDATE SKIP LOCKED) sepet
// işlemleriyle ters sırada kilit almak olurdu ve deadlock'a açıktı.
// Aynı anda iki worker çalışırsa ikincisi zaten bırakılmış satırları atlar.
func ExpireCartReservations(ctx context.Context, tx pgx.Tx, now time.Time, limit int) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM stock_reservations
		WHERE status = 'active' AND order_id IS NULL AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n, err := Release(ctx, tx, ids, "expired")
	if err != nil {
		return 0, err
	}
	// Sepet kalemi sepette kalır; ödeme adımında yeniden rezerve edilir.
	_, err = tx.Exec(ctx, `UPDATE cart_items SET reservation_id = NULL WHERE reservation_id = ANY($1::uuid[])`, ids)
	return n, err
}
