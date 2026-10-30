// Package audit kritik işlemleri append-only audit_log tablosuna yazar.
// Kayıt, işlemi yapan transaction'ın içinde atılmalıdır; böylece işlem geri
// alınırsa kaydı da geri alınır, işlem kalıcıysa kaydı da kalıcıdır.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer hem *pgxpool.Pool hem pgx.Tx tarafından karşılanır.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Entry struct {
	ActorID    string // boşsa sistem ya da anonim
	ActorRole  string
	Action     string // ör. "auth.login", "order.cancel"
	EntityType string
	EntityID   string
	Old        any
	New        any
	RequestID  string
}

func Record(ctx context.Context, db Execer, e Entry) error {
	oldJSON, err := marshal(e.Old)
	if err != nil {
		return err
	}
	newJSON, err := marshal(e.New)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_role, action, entity_type, entity_id, old_value, new_value, request_id)
		VALUES (NULLIF($1, '')::uuid, NULLIF($2, ''), $3, $4, $5, $6, $7, NULLIF($8, ''))`,
		e.ActorID, e.ActorRole, e.Action, e.EntityType, e.EntityID, oldJSON, newJSON, e.RequestID)
	if err != nil {
		return fmt.Errorf("audit kaydı yazılamadı: %w", err)
	}
	return nil
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
