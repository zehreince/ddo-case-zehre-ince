package ledger

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Post işlemi deftere yazar. Aynı idempotency anahtarıyla daha önce yazılmışsa
// hiçbir şey yapmaz ve false döner. Çağıranın transaction'ı içinde çalışır;
// borç = alacak kuralını veritabanı da commit anında ayrıca denetler.
func Post(ctx context.Context, tx pgx.Tx, t Tx) (bool, error) {
	if err := t.Validate(); err != nil {
		return false, err
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (kind, reference_type, reference_id, idempotency_key, description, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, t.Kind, t.RefType, t.RefID, t.IdempotencyKey, t.Description, t.OccurredAt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range t.Entries {
		account, err := ensureAccount(ctx, tx, e.Account, t.SellerID)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, direction, amount_kurus) VALUES ($1, $2, $3, $4)`,
			id, account, e.Direction, e.Amount); err != nil {
			return false, err
		}
	}
	return true, nil
}

func ensureAccount(ctx context.Context, tx pgx.Tx, code, sellerID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code = $1`, code).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	// Satıcı hesabı ilk kullanımda açılır; aynı anda iki işlem açmaya çalışırsa biri kazanır.
	var seller *string
	if sellerID != "" && code == SellerPayable(sellerID) {
		seller = &sellerID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_accounts (code, name, type, seller_id) VALUES ($1, $1, $2, $3::uuid)
		ON CONFLICT (code) DO NOTHING`, code, AccountTypeOf(code), seller)
	if err != nil {
		return 0, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code = $1`, code).Scan(&id)
	return id, err
}

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type AccountBalance struct {
	Code     string      `json:"code"`
	Type     AccountType `json:"type"`
	SellerID *string     `json:"seller_id,omitempty"`
	Debit    int64       `json:"debit_kurus"`
	Credit   int64       `json:"credit_kurus"`
	// Balance hesabın doğal yönündeki bakiyedir: varlık ve giderde borç - alacak,
	// borç ve gelirde alacak - borç.
	Balance int64 `json:"balance_kurus"`
}

// Balances tüm hesapların bakiyesini döner (mizan).
func Balances(ctx context.Context, q Querier) ([]AccountBalance, error) {
	rows, err := q.Query(ctx, `
		SELECT a.code, a.type, a.seller_id::text,
		       COALESCE(sum(e.amount_kurus) FILTER (WHERE e.direction = 'debit'), 0),
		       COALESCE(sum(e.amount_kurus) FILTER (WHERE e.direction = 'credit'), 0)
		FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id ORDER BY a.code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountBalance, error) {
		var b AccountBalance
		if err := r.Scan(&b.Code, &b.Type, &b.SellerID, &b.Debit, &b.Credit); err != nil {
			return b, err
		}
		b.Balance = natural(b.Type, b.Debit, b.Credit)
		return b, nil
	})
}

// Balance tek bir hesabın doğal yöndeki bakiyesidir (hesap yoksa 0).
func Balance(ctx context.Context, q Querier, code string) (int64, error) {
	var debit, credit int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(sum(e.amount_kurus) FILTER (WHERE e.direction = 'debit'), 0),
		       COALESCE(sum(e.amount_kurus) FILTER (WHERE e.direction = 'credit'), 0)
		FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE a.code = $1`, code).Scan(&debit, &credit)
	return natural(AccountTypeOf(code), debit, credit), err
}

func natural(t AccountType, debit, credit int64) int64 {
	if t == Asset || t == Expense {
		return debit - credit
	}
	return credit - debit
}
