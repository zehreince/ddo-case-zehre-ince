package ledger

import (
	"context"
	"testing"

	"carsihub/internal/platform/dbtest"
	fx "carsihub/internal/platform/testfixture"
)

// Tam bir sipariş yaşam döngüsü deftere yazılır; mizan dengede, satıcı alacağı doğru.
func TestPost_LifecycleIsBalancedAndIdempotent(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	seller := fx.Seller(t, pool, "")
	a := Accrual{SubOrderID: "so1", SellerID: seller, PaidKurus: 17000, MarketplaceDiscount: 1000, CommissionKurus: 2700, ShippingFeeKurus: 2990}
	txs := []Tx{
		PaymentCaptured("o1", 17000, at),
		SubOrderDelivered(a, at),
		Returned("r1", []ReturnShare{{SellerID: seller, GrossReversed: 9000, CommissionReversed: 1350, MarketplaceDiscountReversed: 500}}, 8500, at),
		Payout(1, seller, 2000, at),
	}
	for range 2 { // ikinci tur: aynı olaylar tekrar gelir, deftere yazılmaz
		tx, _ := pool.Begin(ctx)
		for _, t2 := range txs {
			if _, err := Post(ctx, tx, t2); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions`).Scan(&n)
	if n != len(txs) {
		t.Fatalf("her olay bir kez yazılmalı: %d", n)
	}
	balances, err := Balances(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var debit, credit int64
	for _, b := range balances {
		debit += b.Debit
		credit += b.Credit
	}
	if debit != credit {
		t.Fatalf("mizan dengesiz: %d / %d", debit, credit)
	}
	// Satıcı: net 12310 - iade kesintisi (9000-1350) - ödeme 2000 = 2660
	if got, _ := Balance(ctx, pool, SellerPayable(seller)); got != 12310-7650-2000 {
		t.Fatalf("satıcı alacağı: %d", got)
	}
	// Kasa: 17000 tahsil - 8500 iade - 2000 ödeme
	if got, _ := Balance(ctx, pool, Cash); got != 6500 {
		t.Fatalf("kasa: %d", got)
	}
}

// Uygulama katmanı atlanıp dengesiz kayıt doğrudan yazılsa bile commit reddedilir;
// yazılmış kayıt da değiştirilemez.
func TestDatabaseEnforcesBalanceAndAppendOnly(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	if _, err := Post(ctx, tx, PaymentCaptured("o1", 500, at)); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)

	tx, _ = pool.Begin(ctx)
	_, err := tx.Exec(ctx, `
		WITH t AS (INSERT INTO ledger_transactions (kind, reference_type, reference_id, idempotency_key, occurred_at)
		           VALUES ('hack', 'x', 'x', 'hack', now()) RETURNING id)
		INSERT INTO ledger_entries (transaction_id, account_id, direction, amount_kurus)
		SELECT t.id, a.id, 'debit', 999 FROM t, ledger_accounts a WHERE a.code = $1`, Cash)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("dengesiz işlem commit'te reddedilmeli")
	}
	for _, stmt := range []string{`UPDATE ledger_entries SET amount_kurus = 1`, `DELETE FROM ledger_transactions`} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s reddedilmeli", stmt)
		}
	}
}
