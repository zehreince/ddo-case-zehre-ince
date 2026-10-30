package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

func validInput(category int64) CreateProductInput {
	return CreateProductInput{
		CategoryID: category, Name: "  Kupa  ",
		Variants: []VariantInput{{SKU: "KUPA-1", PriceKurus: 4990, Stock: 5}},
	}
}

func TestNormalize(t *testing.T) {
	in := validInput(1)
	if errs := in.Normalize(); len(errs) != 0 || in.Name != "Kupa" {
		t.Fatalf("geçerli girdi: %v %q", errs, in.Name)
	}

	bad := CreateProductInput{
		Name: strings.Repeat("a", 301),
		Variants: []VariantInput{
			{SKU: "x", PriceKurus: 0, Stock: -1},
			{SKU: "AYNI", PriceKurus: 1},
			{SKU: "ayni", PriceKurus: 1},
		},
	}
	errs := bad.Normalize()
	for _, k := range []string{"category_id", "name", "variants[0].sku", "variants[0].price_kurus", "variants[0].stock", "variants[2].sku"} {
		if _, ok := errs[k]; !ok {
			t.Errorf("%s hatası bekleniyordu: %v", k, errs)
		}
	}
	empty := CreateProductInput{CategoryID: 1, Name: "x"}
	if _, ok := empty.Normalize()["variants"]; !ok {
		t.Error("varyantsız ürün reddedilmeli")
	}
}

func TestCreateProductAndStockRules(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	s := NewService(pool)
	sellerUser, otherUser, noSeller := fx.User(t, pool, "seller"), fx.User(t, pool, "seller"), fx.User(t, pool, "seller")
	fx.Seller(t, pool, sellerUser)
	fx.Seller(t, pool, otherUser)
	category := fx.Category(t, pool)

	if _, err := s.CreateProduct(ctx, noSeller, validInput(category), ""); !errors.Is(err, ErrNotSeller) {
		t.Fatalf("satıcısız kullanıcı: %v", err)
	}
	if _, err := s.CreateProduct(ctx, sellerUser, validInput(999999), ""); !errors.Is(err, ErrBadCategory) {
		t.Fatalf("olmayan kategori: %v", err)
	}
	p, err := s.CreateProduct(ctx, sellerUser, validInput(category), "")
	if err != nil || len(p.Variants) != 1 || p.Variants[0].Offers[0].Available != 5 {
		t.Fatalf("ürün: %+v %v", p, err)
	}
	if _, err := s.CreateProduct(ctx, sellerUser, validInput(category), ""); !errors.Is(err, ErrDuplicateSKU) {
		t.Fatalf("tekrarlanan SKU: %v", err)
	}

	offerID := p.Variants[0].Offers[0].ID
	if _, err := s.SetStock(ctx, otherUser, offerID, 1, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("başka satıcının teklifi: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE offers SET stock_reserved = 3 WHERE id = $1::uuid`, offerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStock(ctx, sellerUser, offerID, 2, ""); !errors.Is(err, ErrBelowReserved) {
		t.Fatalf("tutulanın altına inmemeli: %v", err)
	}
	o, err := s.SetStock(ctx, sellerUser, offerID, 3, "")
	if err != nil || o.Available != 0 {
		t.Fatalf("tam tutulan kadar olabilir: %+v %v", o, err)
	}
	if _, err := s.Product(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("olmayan ürün: %v", err)
	}
}
