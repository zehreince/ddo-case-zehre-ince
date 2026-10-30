package seed

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Demo kataloğu: elle deneme ve verify için küçük, okunur bir veri seti.
// 1 milyon ürünlük hacim simülatörün işidir (T-5).

type demoSeller struct {
	Name      string
	UserEmail string // boşsa giriş yapamayan satıcı
}

var demoSellers = []demoSeller{
	{"Demo Mağaza", "satici@carsihub.test"},
	{"Anadolu Ticaret", ""},
	{"Ege Elektronik", ""},
}

var demoCategories = []struct{ Slug, Name string }{
	{"giyim", "Giyim"},
	{"elektronik", "Elektronik"},
	{"ev-yasam", "Ev & Yaşam"},
	{"gida", "Gıda"},
	{"kirtasiye", "Kırtasiye"},
}

type demoOffer struct {
	Seller string
	Price  int64 // kuruş
	Stock  int
}

type demoVariant struct {
	SKU    string
	Attrs  map[string]string
	Offers []demoOffer
}

type demoProduct struct {
	Category string
	Name     string
	Desc     string
	Variants []demoVariant
}

var demoProducts = []demoProduct{
	{"giyim", "Pamuklu Basic Tişört", "%100 pamuk, normal kesim.", []demoVariant{
		{"TSH-BASIC-S", map[string]string{"beden": "S"}, []demoOffer{{"Demo Mağaza", 19990, 25}}},
		{"TSH-BASIC-M", map[string]string{"beden": "M"}, []demoOffer{{"Demo Mağaza", 19990, 25}, {"Anadolu Ticaret", 18990, 10}}},
		{"TSH-BASIC-L", map[string]string{"beden": "L"}, []demoOffer{{"Demo Mağaza", 19990, 25}}},
	}},
	{"giyim", "Şişme Mont", "Su itici dış yüzey. Stoğu bilerek düşük.", []demoVariant{
		{"MNT-SISME-M", map[string]string{"beden": "M"}, []demoOffer{{"Demo Mağaza", 189900, 2}}},
		{"MNT-SISME-L", map[string]string{"beden": "L"}, []demoOffer{{"Demo Mağaza", 189900, 1}}},
	}},
	{"elektronik", "Kablosuz Kulaklık", "Bluetooth 5.3, 30 saat pil.", []demoVariant{
		{"KLK-BT-SYH", map[string]string{"renk": "Siyah"}, []demoOffer{{"Ege Elektronik", 129900, 15}, {"Demo Mağaza", 134900, 5}}},
		{"KLK-BT-BYZ", map[string]string{"renk": "Beyaz"}, []demoOffer{{"Ege Elektronik", 129900, 15}}},
	}},
	{"elektronik", "Akıllı Saat", "Nabız ve uyku takibi.", []demoVariant{
		{"SAAT-42", map[string]string{"kasa": "42 mm"}, []demoOffer{{"Ege Elektronik", 349900, 8}}},
		{"SAAT-46", map[string]string{"kasa": "46 mm"}, []demoOffer{{"Ege Elektronik", 369900, 8}}},
	}},
	{"ev-yasam", "Seramik Kahve Kupası", "350 ml, bulaşık makinesinde yıkanabilir.", []demoVariant{
		{"KUPA-SRM-350", nil, []demoOffer{{"Demo Mağaza", 12990, 40}}},
	}},
	{"ev-yasam", "Çelik Termos 500 ml", "12 saat sıcak tutar. Stoğu bilerek düşük.", []demoVariant{
		{"TRMS-500", nil, []demoOffer{{"Anadolu Ticaret", 34950, 3}}},
	}},
	{"gida", "Türk Kahvesi 250 g", "Orta kavrulmuş.", []demoVariant{
		{"KHV-TURK-250", nil, []demoOffer{{"Anadolu Ticaret", 8990, 100}}},
	}},
	{"kirtasiye", "Çizgili Defter A5", "80 yaprak.", []demoVariant{
		{"DFT-A5-CZG", nil, []demoOffer{{"Demo Mağaza", 4500, 200}}},
	}},
}

// Catalog demo kataloğunu yükler. Her ürün ilk varyantının SKU'su ile
// tanınır; varsa atlanır, böylece tekrar çalıştırmak güvenlidir.
func Catalog(ctx context.Context, pool *pgxpool.Pool) (created int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sellers := map[string]string{}
	for _, s := range demoSellers {
		id, err := ensureSeller(ctx, tx, s)
		if err != nil {
			return 0, fmt.Errorf("satıcı %s: %w", s.Name, err)
		}
		sellers[s.Name] = id
	}
	categories := map[string]int64{}
	for _, c := range demoCategories {
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO categories (slug, name) VALUES ($1, $2)
			ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, c.Slug, c.Name).Scan(&id)
		if err != nil {
			return 0, err
		}
		categories[c.Slug] = id
	}

	for _, p := range demoProducts {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM product_variants WHERE sku = $1)`, p.Variants[0].SKU).Scan(&exists); err != nil {
			return 0, err
		}
		if exists {
			continue
		}
		var productID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO products (category_id, name, description) VALUES ($1, $2, $3) RETURNING id::text`,
			categories[p.Category], p.Name, p.Desc).Scan(&productID); err != nil {
			return 0, err
		}
		for _, v := range p.Variants {
			attrs := v.Attrs
			if attrs == nil {
				attrs = map[string]string{}
			}
			var variantID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO product_variants (product_id, sku, attributes) VALUES ($1::uuid, $2, $3) RETURNING id::text`,
				productID, v.SKU, attrs).Scan(&variantID); err != nil {
				return 0, err
			}
			for _, o := range v.Offers {
				if _, err := tx.Exec(ctx, `
					INSERT INTO offers (seller_id, variant_id, price_kurus, stock_on_hand) VALUES ($1::uuid, $2::uuid, $3, $4)`,
					sellers[o.Seller], variantID, o.Price, o.Stock); err != nil {
					return 0, err
				}
			}
		}
		created++
	}
	return created, tx.Commit(ctx)
}

func ensureSeller(ctx context.Context, tx pgx.Tx, s demoSeller) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM sellers WHERE name = $1`, s.Name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO sellers (user_id, name)
		VALUES ((SELECT id FROM users WHERE email = NULLIF($1, '')), $2)
		RETURNING id::text`, s.UserEmail, s.Name).Scan(&id)
	return id, err
}
