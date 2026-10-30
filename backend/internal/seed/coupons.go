package seed

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DemoCoupons elle deneme için kuponlardır; kuralları TESTING.md'de listelenir.
var DemoCoupons = []struct {
	Code, Scope, Seller, Kind string
	Value, MaxDiscount, Min   int64
	Category                  string // boşsa tüm kategoriler
	Stackable                 bool
	MaxRedemptions, PerUser   *int
}{
	{Code: "PAZAR50", Scope: "marketplace", Kind: "fixed", Value: 5000, Min: 30000, Stackable: true},
	{Code: "YUZDE10", Scope: "marketplace", Kind: "percent", Value: 1000, MaxDiscount: 15000, Stackable: false},
	{Code: "DEMOGIYIM20", Scope: "seller", Seller: "Demo Mağaza", Kind: "percent", Value: 2000, Category: "giyim", Stackable: true},
	{Code: "ILK100", Scope: "marketplace", Kind: "fixed", Value: 10000, Min: 50000, Stackable: true, MaxRedemptions: ptr(100), PerUser: ptr(1)},
}

func ptr(n int) *int { return &n }

// Coupons demo kuponları yükler; kod zaten varsa dokunmaz.
func Coupons(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	created := 0
	for _, c := range DemoCoupons {
		tag, err := pool.Exec(ctx, `
			INSERT INTO coupons (code, scope, seller_id, kind, value, max_discount_kurus, min_basket_kurus, category_ids,
			                     stackable, max_redemptions, per_user_limit)
			VALUES ($1, $2, (SELECT id FROM sellers WHERE name = NULLIF($3, '')), $4, $5, $6, $7,
			        COALESCE((SELECT ARRAY[id] FROM categories WHERE slug = NULLIF($8, '')), '{}'),
			        $9, $10, $11)
			ON CONFLICT (code) DO NOTHING`,
			c.Code, c.Scope, c.Seller, c.Kind, c.Value, c.MaxDiscount, c.Min, c.Category, c.Stackable, c.MaxRedemptions, c.PerUser)
		if err != nil {
			return created, err
		}
		created += int(tag.RowsAffected())
	}
	return created, nil
}
