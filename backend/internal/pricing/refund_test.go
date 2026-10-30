package pricing

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// Sabit tutarlı kupon kalan sepette hâlâ geçerliyse indirimin tamamı kalan
// kalemlere kayar: müşteri iade ettiği kalemin tam fiyatını geri alır ve
// kalanlar için yine 30 TL indirimli öder.
func TestRefund_FixedCouponShiftsToRemainingItems(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 2, 10000), line("b", "s2", 1, 1, 10000)} // 300 TL
	coupons := []Coupon{market("P30", KindFixed, 3000)}

	rq, err := Refund(lines, coupons, nil, map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	// Önce 300 - 30 = 270; sonra 200 - 30 = 170 → iade 100 TL.
	if rq.BeforeKurus != 27000 || rq.AfterKurus != 17000 || rq.RefundKurus != 10000 || len(rq.LostCoupons) != 0 {
		t.Fatalf("iade: %+v", rq)
	}
	// Satıcı bazında: s1 bir kalemi geri aldı; indirim payları yeniden dağıldı.
	s1, s2 := rq.BySeller["s1"], rq.BySeller["s2"]
	if s1.SubtotalReturnedKurus != 10000 || s2.SubtotalReturnedKurus != 0 || s1.RefundKurus+s2.RefundKurus != rq.RefundKurus {
		t.Fatalf("satıcı dağılımı: %+v", rq.BySeller)
	}
}

// Yüzde kuponda iade edilen kalemin indirimi de geri alınır: indirimsiz fiyat iade edilmez.
func TestRefund_PercentCouponIsProportional(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 3, 10000)}
	coupons := []Coupon{market("Y10", KindPercent, 1000)}
	rq, err := Refund(lines, coupons, nil, map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if rq.RefundKurus != 9000 || rq.ClawbackKurus != 0 || rq.BySeller["s1"].MarketplaceDiscountReversed != 1000 {
		t.Fatalf("iade 100 - %%10 = 90 TL olmalı: %+v", rq)
	}
}

// Kalan sepet minimum tutarın altına düşerse kupon geri alınır ve iadeden kesilir.
func TestRefund_ClawbackWhenBelowMinBasket(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 1, 20000), line("b", "s1", 1, 1, 10000)} // 300 TL
	c := market("MIN250", KindFixed, 5000)
	c.MinBasketKurus = 25000
	coupons := []Coupon{c}

	rq, err := Refund(lines, coupons, nil, map[string]int{"b": 1})
	if err != nil {
		t.Fatal(err)
	}
	// Önce 250 TL ödendi. b iade edilince sepet 200 < 250 → kupon düşer, kalan 200 TL.
	// İade 50 TL; b'nin ödenmiş payı 83,33 TL'ydi → 33,33 TL kesinti.
	if rq.BeforeKurus != 25000 || rq.AfterKurus != 20000 || rq.RefundKurus != 5000 {
		t.Fatalf("iade: %+v", rq)
	}
	if rq.ClawbackKurus != 3333 || len(rq.LostCoupons) != 1 || rq.LostCoupons[0] != "MIN250" {
		t.Fatalf("clawback: %+v", rq)
	}
}

// Kupon kaybı iade edilen kalemden pahalıysa müşteriden ek ödeme istenmez: iade 0.
func TestRefund_NeverNegative(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 1, 29000), line("b", "s1", 1, 1, 1000)}
	c := market("MIN300", KindFixed, 10000)
	c.MinBasketKurus = 30000
	rq, err := Refund(lines, []Coupon{c}, nil, map[string]int{"b": 1})
	if err != nil {
		t.Fatal(err)
	}
	if rq.RefundKurus != 0 || rq.AfterKurus <= rq.BeforeKurus {
		t.Fatalf("iade negatif olmamalı: %+v", rq)
	}
}

func TestRefund_ValidatesQuantities(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 2, 1000)}
	for name, tc := range map[string]struct{ returned, returning map[string]int }{
		"olmayan satır": {nil, map[string]int{"x": 1}},
		"sıfır adet":    {nil, map[string]int{"a": 0}},
		"fazla adet":    {map[string]int{"a": 1}, map[string]int{"a": 2}},
	} {
		if _, err := Refund(lines, nil, tc.returned, tc.returning); err == nil {
			t.Errorf("%s reddedilmeli", name)
		}
	}
}

// Minimum sepetsiz kuponlarda adım adım tüm kalemler iade edilince toplam iade
// ödenen tutara kuruşu kuruşuna eşittir (kademeli iadelerde kuruş kayması yok).
func TestRefund_StepwiseFullReturnSumsToPaid(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	for n := range 500 {
		var lines []Line
		for i := range 1 + rng.IntN(5) {
			lines = append(lines, line(fmt.Sprintf("l%d", i), fmt.Sprintf("s%d", rng.IntN(3)), 1, 1+rng.IntN(4), 1+rng.Int64N(100000)))
		}
		coupons := []Coupon{market("P", KindPercent, 1+rng.Int64N(5000)), sellerC("S", "s0", KindFixed, 1+rng.Int64N(50000))}
		paid := Calculate(lines, coupons)
		var applied []Coupon
		for _, c := range coupons {
			for _, a := range paid.Applied {
				if a.Code == c.Code {
					applied = append(applied, c)
				}
			}
		}

		returned := map[string]int{}
		var refunded int64
		for _, l := range lines {
			for range l.Quantity {
				rq, err := Refund(lines, applied, returned, map[string]int{l.ID: 1})
				if err != nil {
					t.Fatal(err)
				}
				refunded += rq.RefundKurus
				returned[l.ID]++
			}
		}
		if refunded != paid.TotalKurus {
			t.Fatalf("#%d toplam iade %d, ödenen %d", n, refunded, paid.TotalKurus)
		}
	}
}
