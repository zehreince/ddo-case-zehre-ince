// Package migrate gömülü SQL migrasyonlarını uygular. Hem cmd/migrate hem
// entegrasyon testleri aynı yolu kullanır; testteki şema üretimdekiyle aynıdır.
package migrate

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"carsihub/migrations"
)

func provider(pool *pgxpool.Pool) (*goose.Provider, func(), error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return p, func() { _ = sqlDB.Close() }, nil
}

// Up bekleyen tüm migrasyonları uygular ve uygulanan sayısını döner.
func Up(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int, error) {
	p, closeFn, err := provider(pool)
	if err != nil {
		return 0, err
	}
	defer closeFn()

	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("migrasyon uygulandı", "version", r.Source.Version, "file", r.Source.Path, "duration_ms", r.Duration.Milliseconds())
	}
	return len(results), err
}

// DownAll tüm migrasyonları geri alır. Yalnızca testlerde geri dönüşü doğrulamak için.
func DownAll(ctx context.Context, pool *pgxpool.Pool) error {
	p, closeFn, err := provider(pool)
	if err != nil {
		return err
	}
	defer closeFn()
	_, err = p.DownTo(ctx, 0)
	return err
}
