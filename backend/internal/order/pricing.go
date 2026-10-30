package order

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"carsihub/internal/coupon"
	"carsihub/internal/inventory"
	"carsihub/internal/pricing"
)

// MaxCoupons bir siparişte denenebilecek kupon kodu sayısıdır.
const MaxCoupons = 5

// PlaceOption siparişe isteğe bağlı girdi ekler.
type PlaceOption func(*placeOpts)

type placeOpts struct {
	coupons []string
}

// WithCoupons siparişe uygulanmak istenen kupon kodlarını verir (sıra = öncelik).
func WithCoupons(codes []string) PlaceOption {
	return func(o *placeOpts) { o.coupons = codes }
}

// pricedOrder fiyat motorunun sipariş için ürettiği sonuçtur.
type pricedOrder struct {
	quote    pricing.Quote
	applied  []coupon.Coupon // fiilen indirim veren kuponlar (anlık görüntüleri saklanır)
	rejected []pricing.Rejected
}

// lines rezervasyonları fiyat motorunun satırlarına çevirir. Satır kimliği
// offer kimliğidir: rezervasyonlar offer başına birleştirildiği için siparişte tekildir.
func lines(ctx context.Context, tx pgx.Tx, reserved []inventory.Reservation) ([]pricing.Line, map[string]int64, error) {
	ids := make([]string, len(reserved))
	for i, r := range reserved {
		ids[i] = r.OfferID
	}
	rows, err := tx.Query(ctx, `
		SELECT o.id::text, p.category_id
		FROM offers o JOIN product_variants v ON v.id = o.variant_id JOIN products p ON p.id = v.product_id
		WHERE o.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, nil, err
	}
	categories := map[string]int64{}
	for rows.Next() {
		var id string
		var cat int64
		if err := rows.Scan(&id, &cat); err != nil {
			rows.Close()
			return nil, nil, err
		}
		categories[id] = cat
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := make([]pricing.Line, len(reserved))
	for i, r := range reserved {
		out[i] = pricing.Line{ID: r.OfferID, SellerID: r.SellerID, CategoryID: categories[r.OfferID], Quantity: r.Quantity, UnitPriceKurus: r.UnitPriceKurus}
	}
	return out, categories, nil
}

// price kuponları çözer, indirimi hesaplar ve kullanımları sipariş adına ayırır.
//
// Kullanım limiti dolmuş bir kupon (eşzamanlı başka bir sipariş son hakkı
// almış olabilir) reddedilir ve sepet onsuz yeniden hesaplanır. Her deneme
// kendi savepoint'inde yapılır; başarısız turun ayırdığı kullanımlar geri alınır.
func (s *Service) price(ctx context.Context, tx pgx.Tx, customerID, orderID string, ls []pricing.Line, codes []string) (*pricedOrder, error) {
	resolved, rejected, err := coupon.Resolve(ctx, tx, codes, s.now())
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		if attempt > MaxCoupons {
			return nil, errors.New("kupon kullanımı ayrılamadı")
		}
		pcs := make([]pricing.Coupon, len(resolved))
		for i, c := range resolved {
			pcs[i] = c.Coupon
		}
		q := pricing.Calculate(ls, pcs)

		round, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var (
			failed  string
			reason  string
			applied []coupon.Coupon
		)
		for _, a := range q.Applied {
			c := resolved[slices.IndexFunc(resolved, func(c coupon.Coupon) bool { return c.Code == a.Code })]
			r, err := coupon.Redeem(ctx, round, c, customerID, orderID, a.DiscountKurus)
			if err != nil {
				_ = round.Rollback(ctx)
				return nil, err
			}
			if r != "" {
				failed, reason = a.Code, r
				break
			}
			applied = append(applied, c)
		}
		if failed == "" {
			if err := round.Commit(ctx); err != nil {
				return nil, err
			}
			q.Rejected = append(rejected, q.Rejected...)
			if q.Rejected == nil {
				q.Rejected = []pricing.Rejected{}
			}
			return &pricedOrder{quote: q, applied: applied, rejected: q.Rejected}, nil
		}
		if err := round.Rollback(ctx); err != nil {
			return nil, err
		}
		rejected = append(rejected, pricing.Rejected{Code: failed, Reason: reason})
		resolved = slices.DeleteFunc(resolved, func(c coupon.Coupon) bool { return c.Code == failed })
	}
}

// applyQuote fiyat sonucunu alt siparişlere ve satırlara işler.
func applyQuote(subs []SubOrder, q pricing.Quote, categories map[string]int64) error {
	for i := range subs {
		sub := &subs[i]
		sub.MarketplaceDiscountKurus, sub.SellerDiscountKurus = 0, 0
		for j := range sub.Items {
			l := &sub.Items[j]
			r, ok := q.Line(l.OfferID)
			if !ok {
				return fmt.Errorf("fiyat sonucunda satır yok: %s", l.OfferID)
			}
			cat := categories[l.OfferID]
			l.CategoryID = &cat
			l.MarketplaceDiscountKurus, l.SellerDiscountKurus = r.MarketplaceDiscount, r.SellerDiscount
			l.PaidKurus = r.TotalKurus
			sub.MarketplaceDiscountKurus += r.MarketplaceDiscount
			sub.SellerDiscountKurus += r.SellerDiscount
		}
		sub.DiscountKurus = sub.MarketplaceDiscountKurus + sub.SellerDiscountKurus
		sub.TotalKurus = sub.SubtotalKurus - sub.DiscountKurus
	}
	return nil
}
