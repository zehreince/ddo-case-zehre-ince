package pricing

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

func line(id, seller string, cat int64, qty int, price int64) Line {
	return Line{ID: id, SellerID: seller, CategoryID: cat, Quantity: qty, UnitPriceKurus: price}
}

func market(code string, kind Kind, value int64) Coupon {
	return Coupon{Code: code, Scope: ScopeMarketplace, Kind: kind, Value: value, Stackable: true}
}

func sellerC(code, seller string, kind Kind, value int64) Coupon {
	return Coupon{Code: code, Scope: ScopeSeller, SellerID: seller, Kind: kind, Value: value, Stackable: true}
}

func applied(q Quote) []string {
	var out []string
	for _, a := range q.Applied {
		out = append(out, fmt.Sprintf("%s=%d", a.Code, a.DiscountKurus))
	}
	return out
}

func reasons(q Quote) map[string]string {
	out := map[string]string{}
	for _, r := range q.Rejected {
		out[r.Code] = r.Reason
	}
	return out
}

func TestAllocate_NoKurusDrift(t *testing.T) {
	cases := []struct {
		amount  int64
		weights []int64
		want    []int64
	}{
		{100, []int64{1000, 1000, 1000}, []int64{34, 33, 33}},     // eşit kalanda önceki sıra
		{1, []int64{500, 300, 200}, []int64{1, 0, 0}},             // tek kuruş en büyük paya
		{1000, []int64{3333, 3333, 3334}, []int64{333, 333, 334}}, // büyük ağırlık önce
		{10, []int64{0, 7, 3}, []int64{0, 7, 3}},                  // sıfır ağırlık pay almaz
		{50, []int64{20, 10}, []int64{20, 10}},                    // tutar ağırlığı aşamaz
		{0, []int64{5, 5}, []int64{0, 0}},
		{7, nil, []int64{}},
	}
	for _, tc := range cases {
		got := Allocate(tc.amount, tc.weights)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Allocate(%d, %v) = %v, beklenen %v", tc.amount, tc.weights, got, tc.want)
		}
	}
}

// Kuruş × kuruş çarpımı int64'ü aşsa da sonuç doğru (128-bit ara sonuç).
func TestAllocate_NoOverflow(t *testing.T) {
	w := []int64{math.MaxInt64 / 4, math.MaxInt64 / 4, math.MaxInt64/4 + 1}
	amount := int64(math.MaxInt64 / 3)
	got := Allocate(amount, w)
	var sum int64
	for i, s := range got {
		sum += s
		if s < 0 || s > w[i] {
			t.Fatalf("pay %d geçersiz: %d", i, s)
		}
	}
	if sum != amount {
		t.Fatalf("toplam %d, beklenen %d", sum, amount)
	}
}

func TestCalculate_PercentFixedAndCaps(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 1, 33333)} // 333,33 TL

	q := Calculate(lines, []Coupon{market("Y10", KindPercent, 1000)})
	if q.DiscountKurus != 3333 || q.TotalKurus != 30000 { // %10 = 33,333 → aşağı yuvarlanır
		t.Fatalf("yüzde: %+v", q)
	}
	capped := market("Y50", KindPercent, 5000)
	capped.MaxDiscountKurus = 5000
	if q := Calculate(lines, []Coupon{capped}); q.DiscountKurus != 5000 {
		t.Fatalf("üst sınır: %d", q.DiscountKurus)
	}
	if q := Calculate(lines, []Coupon{market("BUYUK", KindFixed, 999999)}); q.DiscountKurus != 33333 || q.TotalKurus != 0 {
		t.Fatalf("sabit indirim tutarı aşamaz: %+v", q)
	}
}

func TestCalculate_MinBasketAndCategory(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 2, 10000), line("b", "s1", 2, 1, 5000)} // kategori 1: 200 TL, kategori 2: 50 TL

	c := market("K1", KindFixed, 3000)
	c.CategoryIDs = []int64{1}
	q := Calculate(lines, []Coupon{c})
	if l, _ := q.Line("b"); l.DiscountKurus() != 0 {
		t.Fatal("kategori dışı satır indirim almamalı")
	}
	if l, _ := q.Line("a"); l.DiscountKurus() != 3000 {
		t.Fatalf("kategori satırı tüm indirimi almalı: %+v", l)
	}

	// Minimum sepet yalnızca uygun satırlara bakar: kategori 1 = 200 TL < 210 TL
	c.MinBasketKurus = 21000
	q = Calculate(lines, []Coupon{c})
	if reasons(q)["K1"] != ReasonMinBasket || q.DiscountKurus != 0 {
		t.Fatalf("minimum sepet: %+v", q)
	}

	other := market("YOK", KindFixed, 100)
	other.CategoryIDs = []int64{99}
	if reasons(Calculate(lines, []Coupon{other}))["YOK"] != ReasonNoEligible {
		t.Fatal("uygun satır yoksa reddedilmeli")
	}
}

// Satıcı kuponu yalnızca kendi satırlarına, önce uygulanır; pazaryeri kuponu kalan tutara.
func TestCalculate_SellerThenMarketplaceAndFunding(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 1, 10000), line("b", "s2", 1, 1, 10000)}
	q := Calculate(lines, []Coupon{
		market("PAZAR10", KindPercent, 1000), // sırası önemli değil: satıcı önce uygulanır
		sellerC("S1-20", "s1", KindFixed, 2000),
	})
	a, _ := q.Line("a")
	b, _ := q.Line("b")
	// s1: 100 - 20 = 80; pazaryeri %10'u 80 + 100 = 180 üzerinden = 18 → orantılı 8 + 10
	if a.SellerDiscount != 2000 || a.MarketplaceDiscount != 800 || a.TotalKurus != 7200 {
		t.Fatalf("a: %+v", a)
	}
	if b.SellerDiscount != 0 || b.MarketplaceDiscount != 1000 || b.TotalKurus != 9000 {
		t.Fatalf("b: %+v", b)
	}
	bs := q.BySeller()
	if bs["s1"].TotalKurus != 7200 || bs["s2"].MarketplaceDiscount != 1000 || q.TotalKurus != 16200 {
		t.Fatalf("satıcı toplamları: %+v", bs)
	}
}

func TestCalculate_CombinationRules(t *testing.T) {
	lines := []Line{line("a", "s1", 1, 1, 10000), line("b", "s2", 1, 1, 10000)}

	// Aynı kapsamda ikinci kupon ve aynı kod reddedilir.
	q := Calculate(lines, []Coupon{
		market("P1", KindFixed, 1000), market("P2", KindFixed, 5000),
		sellerC("S1A", "s1", KindFixed, 100), sellerC("S1B", "s1", KindFixed, 200),
		market("p1", KindFixed, 1000),
	})
	r := reasons(q)
	if r["P2"] != ReasonOnePerScope || r["S1B"] != ReasonOnePerScope || r["p1"] != ReasonDuplicate {
		t.Fatalf("ret nedenleri: %v", r)
	}

	// Birleştirilemeyen kupon daha avantajlıysa tek başına seçilir.
	big := market("TEK", KindFixed, 5000)
	big.Stackable = false
	q = Calculate(lines, []Coupon{sellerC("S1", "s1", KindFixed, 1000), sellerC("S2", "s2", KindFixed, 1000), big})
	if !slices.Equal(applied(q), []string{"TEK=5000"}) || reasons(q)["S1"] != ReasonNotCombinable {
		t.Fatalf("tek kupon seçilmeliydi: %v %v", applied(q), reasons(q))
	}

	// Birleştirilebilenlerin toplamı daha avantajlıysa onlar seçilir.
	small := market("KUCUK", KindFixed, 1500)
	small.Stackable = false
	q = Calculate(lines, []Coupon{sellerC("S1", "s1", KindFixed, 1000), sellerC("S2", "s2", KindFixed, 1000), small})
	if q.DiscountKurus != 2000 || reasons(q)["KUCUK"] != ReasonNotCombinable {
		t.Fatalf("birleşik kuponlar seçilmeliydi: %v %v", applied(q), reasons(q))
	}

	// Tek başına kullanılan birleştirilemeyen kupon sorunsuz uygulanır.
	if q := Calculate(lines, []Coupon{small}); q.DiscountKurus != 1500 {
		t.Fatal("tek kupon uygulanmalı")
	}
}

func TestValidate(t *testing.T) {
	ok := []Coupon{market("A", KindPercent, 1000), sellerC("B", "s", KindFixed, 100)}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%s geçerli olmalı: %v", c.Code, err)
		}
	}
	bad := []Coupon{
		{Code: "x", Scope: "kimse", Kind: KindFixed, Value: 1},
		{Code: "x", Scope: ScopeSeller, Kind: KindFixed, Value: 1},
		{Code: "x", Scope: ScopeMarketplace, SellerID: "s", Kind: KindFixed, Value: 1},
		{Code: "x", Scope: ScopeMarketplace, Kind: KindPercent, Value: 10001},
		{Code: "x", Scope: ScopeMarketplace, Kind: KindFixed, Value: 0},
		{Code: "x", Scope: ScopeMarketplace, Kind: KindFixed, Value: 1, MaxDiscountKurus: 5},
		{Code: "x", Scope: ScopeMarketplace, Kind: "hediye", Value: 1},
		{Code: "x", Scope: ScopeMarketplace, Kind: KindFixed, Value: 1, MinBasketKurus: -1},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%d geçersiz olmalı: %+v", i, c)
		}
	}
}

// Rastgele 2.000 sepet: kuruş kayması yok, negatif satır yok, sıra bağımsız.
func TestCalculate_Invariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	sellers := []string{"s1", "s2", "s3"}
	for n := range 2000 {
		var lines []Line
		for i := range 1 + rng.IntN(8) {
			lines = append(lines, line(fmt.Sprintf("l%02d", i), sellers[rng.IntN(3)], int64(1+rng.IntN(3)), 1+rng.IntN(5), 1+rng.Int64N(500000)))
		}
		var coupons []Coupon
		for i := range rng.IntN(5) {
			c := Coupon{Code: fmt.Sprintf("C%d", i), Kind: KindPercent, Value: 1 + rng.Int64N(10000), Stackable: rng.IntN(4) != 0}
			if rng.IntN(2) == 0 {
				c.Kind, c.Value = KindFixed, 1+rng.Int64N(200000)
			}
			if rng.IntN(2) == 0 {
				c.Scope, c.SellerID = ScopeSeller, sellers[rng.IntN(3)]
			} else {
				c.Scope = ScopeMarketplace
			}
			if rng.IntN(3) == 0 {
				c.CategoryIDs = []int64{int64(1 + rng.IntN(3))}
			}
			if c.Kind == KindPercent && rng.IntN(3) == 0 {
				c.MaxDiscountKurus = 1 + rng.Int64N(50000)
			}
			c.MinBasketKurus = rng.Int64N(300000)
			coupons = append(coupons, c)
		}

		q := Calculate(lines, coupons)
		perCoupon := map[string]int64{}
		for _, a := range q.Allocations {
			perCoupon[a.CouponCode] += a.AmountKurus
		}
		var sumTotal, sumDisc int64
		for _, l := range q.Lines {
			if l.TotalKurus < 0 || l.DiscountKurus() > l.SubtotalKurus || l.TotalKurus != l.SubtotalKurus-l.DiscountKurus() {
				t.Fatalf("#%d satır tutarsız: %+v", n, l)
			}
			sumTotal += l.TotalKurus
			sumDisc += l.DiscountKurus()
		}
		for _, a := range q.Applied {
			if perCoupon[a.Code] != a.DiscountKurus {
				t.Fatalf("#%d kuruş kayması: %s dağıtılan=%d indirim=%d", n, a.Code, perCoupon[a.Code], a.DiscountKurus)
			}
		}
		if sumTotal != q.TotalKurus || sumDisc != q.DiscountKurus || q.TotalKurus != q.SubtotalKurus-q.DiscountKurus {
			t.Fatalf("#%d toplamlar tutarsız: %+v", n, q)
		}
		if len(q.Applied)+len(q.Rejected) != len(coupons) {
			t.Fatalf("#%d her kupon ya uygulanmalı ya reddedilmeli", n)
		}
		shuffled := slices.Clone(lines)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if !reflect.DeepEqual(Calculate(shuffled, coupons), q) {
			t.Fatalf("#%d sonuç satır sırasına bağlı olmamalı", n)
		}
	}
}
