package order

import (
	"strings"
	"testing"

	"carsihub/internal/inventory"
)

const id1 = "11111111-1111-1111-1111-111111111111"

func TestValidateItems(t *testing.T) {
	cases := []struct {
		name  string
		items []inventory.Item
		want  []string
	}{
		{"geçerli", []inventory.Item{{OfferID: id1, Quantity: 2}}, nil},
		{"boş", nil, []string{"items"}},
		{"bozuk kimlik ve adet", []inventory.Item{{OfferID: "x", Quantity: 0}}, []string{"items[0].offer_id", "items[0].quantity"}},
		{"aynı ürün toplamı sınırı aşar", []inventory.Item{{OfferID: id1, Quantity: 60}, {OfferID: id1, Quantity: 60}}, []string{"items"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateItems(tc.items)
			if len(got) != len(tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			for _, k := range tc.want {
				if _, ok := got[k]; !ok {
					t.Errorf("%s hatası eksik: %v", k, got)
				}
			}
		})
	}
	many := make([]inventory.Item, MaxItems+1)
	if got := ValidateItems(many); !strings.Contains(got["items"], "en fazla") {
		t.Fatalf("kalem sınırı: %v", got)
	}
}

func TestSplit_GroupsBySellerDeterministically(t *testing.T) {
	res := []inventory.Reservation{
		{ID: "r1", OfferID: "o1", SellerID: "s2", Quantity: 2, UnitPriceKurus: 333},
		{ID: "r2", OfferID: "o2", SellerID: "s1", Quantity: 1, UnitPriceKurus: 1000},
		{ID: "r3", OfferID: "o3", SellerID: "s2", Quantity: 3, UnitPriceKurus: 1},
	}
	subs, subtotal := Split(res)
	if subtotal != 2*333+1000+3 || len(subs) != 2 {
		t.Fatalf("subtotal=%d subs=%d", subtotal, len(subs))
	}
	if subs[0].SellerID != "s1" || subs[1].SellerID != "s2" {
		t.Fatal("alt siparişler satıcıya göre sıralı olmalı")
	}
	if subs[1].TotalKurus != 669 || len(subs[1].Items) != 2 || subs[1].Items[0].LineTotalKurus != 666 {
		t.Fatalf("s2 alt siparişi yanlış: %+v", subs[1])
	}
}
