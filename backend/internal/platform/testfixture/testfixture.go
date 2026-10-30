// Package testfixture entegrasyon testleri için en küçük geçerli veriyi kurar
// (kullanıcı, satıcı, stoklu teklif). Yalnızca testlerden kullanılır.
package testfixture

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var seq atomic.Int64

func next() int64 { return seq.Add(1) }

// User şifre özeti geçersiz (giriş yapılamaz) bir kullanıcı oluşturur.
func User(t testing.TB, pool *pgxpool.Pool, role string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, full_name, role) VALUES ($1, 'x', 'Test', $2)
		RETURNING id::text`, fmt.Sprintf("u%d@test.co", next()), role).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Seller verilen kullanıcıya (boşsa kullanıcısız) bağlı bir satıcı oluşturur.
func Seller(t testing.TB, pool *pgxpool.Pool, userID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO sellers (user_id, name) VALUES (NULLIF($1, '')::uuid, $2) RETURNING id::text`,
		userID, fmt.Sprintf("Satıcı %d", next())).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func Category(t testing.TB, pool *pgxpool.Pool) int64 {
	t.Helper()
	n := next()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO categories (name, slug) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("Kategori %d", n), fmt.Sprintf("kategori-%d", n)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Offer yeni bir ürün + varyant ve satıcı için verilen fiyat ve stokta teklif oluşturur.
func Offer(t testing.TB, pool *pgxpool.Pool, sellerID string, priceKurus int64, stock int) string {
	t.Helper()
	ctx := context.Background()
	n := next()
	var productID, variantID, offerID string
	if err := pool.QueryRow(ctx, `INSERT INTO products (category_id, name) VALUES ($1, $2) RETURNING id::text`,
		Category(t, pool), fmt.Sprintf("Ürün %d", n)).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO product_variants (product_id, sku) VALUES ($1::uuid, $2) RETURNING id::text`,
		productID, fmt.Sprintf("SKU-%d", n)).Scan(&variantID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO offers (seller_id, variant_id, price_kurus, stock_on_hand) VALUES ($1::uuid, $2::uuid, $3, $4)
		RETURNING id::text`, sellerID, variantID, priceKurus, stock).Scan(&offerID); err != nil {
		t.Fatal(err)
	}
	return offerID
}

// Stock bir teklifin eldeki ve tutulan miktarını döner.
func Stock(t testing.TB, pool *pgxpool.Pool, offerID string) (onHand, reserved int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT stock_on_hand, stock_reserved FROM offers WHERE id = $1::uuid`, offerID).
		Scan(&onHand, &reserved); err != nil {
		t.Fatal(err)
	}
	return onHand, reserved
}

// ActiveReserved bir teklif için aktif rezervasyonların toplam adedini döner;
// offers.stock_reserved ile her zaman eşit olmalıdır (tutarlılık kontrolü).
func ActiveReserved(t testing.TB, pool *pgxpool.Pool, offerID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(quantity), 0)::int FROM stock_reservations WHERE offer_id = $1::uuid AND status = 'active'`, offerID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
