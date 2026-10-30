package ledger

import (
	"errors"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func sum(t Tx, account string) (debit, credit int64) {
	for _, e := range t.Entries {
		if e.Account != account {
			continue
		}
		if e.Direction == Debit {
			debit += e.Amount
		} else {
			credit += e.Amount
		}
	}
	return
}

func TestBuilders_AreBalanced(t *testing.T) {
	a := Accrual{SubOrderID: "so1", SellerID: "s1", PaidKurus: 17000, MarketplaceDiscount: 1000, CommissionKurus: 2700, ShippingFeeKurus: 2990}
	txs := []Tx{
		PaymentCaptured("o1", 17000, at),
		SubOrderDelivered(a, at),
		Returned("r1", []ReturnShare{{SellerID: "s1", GrossReversed: 9000, CommissionReversed: 1350, MarketplaceDiscountReversed: 500}}, 8500, at),
		Payout(1, "s1", 5000, at),
	}
	for _, tx := range txs {
		if err := tx.Validate(); err != nil {
			t.Errorf("%s dengesiz: %v", tx.Kind, err)
		}
	}
	if a.Gross() != 18000 || a.Net() != 18000-2700-2990 {
		t.Fatalf("brüt/net: %d %d", a.Gross(), a.Net())
	}
	if _, c := sum(txs[1], SellerPayable("s1")); c != a.Net() {
		t.Fatalf("satıcı alacağı net olmalı: %d", c)
	}
}

// Kargo ücreti satıştan büyükse net negatif: satıcı hesabı borçlanır, kayıt yine dengeli.
func TestSubOrderDelivered_NegativeNet(t *testing.T) {
	a := Accrual{SubOrderID: "so", SellerID: "s", PaidKurus: 1000, CommissionKurus: 150, ShippingFeeKurus: 2990}
	tx := SubOrderDelivered(a, at)
	if err := tx.Validate(); err != nil {
		t.Fatal(err)
	}
	if d, _ := sum(tx, SellerPayable("s")); d != 2140 {
		t.Fatalf("satıcı 21,40 TL borçlanmalı: %d", d)
	}
}

// İade sıfıra kırpıldığında (müşteriden ek ödeme istenmez) fark pazaryerine yazılır.
func TestReturned_ClampedRefundIsAbsorbedByMarketplace(t *testing.T) {
	// Kalan sepet kuponu kaybetti: pazaryeri indirimi "geri alınan" tarafta negatif,
	// modele göre müşteri ek ödeme yapmalıydı (pay negatif); gerçek iade 0.
	shares := []ReturnShare{{SellerID: "s", GrossReversed: 1000, MarketplaceDiscountReversed: 9000}}
	tx := Returned("r", shares, 0, at)
	if err := tx.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, c := sum(tx, Cash); c != 0 {
		t.Fatal("müşteriye para ödenmemeli")
	}
}

func TestValidate(t *testing.T) {
	bad := []Tx{
		{Kind: "x", IdempotencyKey: "k", Entries: []Entry{{"a", Debit, 10}}},
		{Kind: "x", IdempotencyKey: "k", Entries: []Entry{{"a", Debit, 10}, {"b", Credit, 9}}},
		{Kind: "x", IdempotencyKey: "k", Entries: []Entry{{"a", Debit, 0}, {"b", Credit, 0}}},
		{Kind: "x", IdempotencyKey: "k", Entries: []Entry{{"a", "yan", 10}, {"b", Credit, 10}}},
		{Kind: "", IdempotencyKey: "k", Entries: []Entry{{"a", Debit, 10}, {"b", Credit, 10}}},
	}
	for i, tx := range bad {
		if tx.Validate() == nil {
			t.Errorf("%d reddedilmeli", i)
		}
	}
	if err := bad[1].Validate(); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("dengesizlik hatası: %v", err)
	}
}

func TestAccountTypes(t *testing.T) {
	want := map[string]AccountType{Cash: Asset, CustomerAdvances: Liability, SellerPayable("x"): Liability,
		CommissionRevenue: Revenue, ShippingRevenue: Revenue, MarketplaceDiscount: Expense}
	for code, typ := range want {
		if AccountTypeOf(code) != typ {
			t.Errorf("%s → %s", code, AccountTypeOf(code))
		}
	}
	if natural(Asset, 10, 3) != 7 || natural(Liability, 3, 10) != 7 {
		t.Fatal("doğal bakiye yönü yanlış")
	}
}
