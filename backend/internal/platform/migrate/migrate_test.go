package migrate_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"carsihub/internal/platform/dbtest"
	"carsihub/internal/platform/migrate"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestUp_IsIdempotent(t *testing.T) {
	pool := dbtest.New(t)
	applied, err := migrate.Up(context.Background(), pool, discard)
	if err != nil || applied != 0 {
		t.Fatalf("ikinci çalıştırma hiçbir şey uygulamamalı: applied=%d err=%v", applied, err)
	}
}

func TestMigrations_AreReversible(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	if err := migrate.DownAll(ctx, pool); err != nil {
		t.Fatalf("down başarısız: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('audit_log') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("down sonrası audit_log kalmamalı: exists=%v err=%v", exists, err)
	}
	if _, err := migrate.Up(ctx, pool, discard); err != nil {
		t.Fatalf("yeniden up başarısız: %v", err)
	}
}

func TestAuditLog_IsAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (action, entity_type, entity_id) VALUES ('test', 'x', '1')`); err != nil {
		t.Fatalf("ekleme izinli olmalı: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE audit_log SET action = 'degisti'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		_, err := pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s reddedilmeli (42501), gelen: %v", stmt, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'test'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("kayıt değişmemiş olmalı: n=%d err=%v", n, err)
	}
}
