// Komut migrate, compose içinde API'den önce bir kez çalışır: şemayı
// günceller ve demo verisini yükler. Tekrar çalıştırmak güvenlidir.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"carsihub/internal/platform/config"
	"carsihub/internal/platform/db"
	"carsihub/internal/platform/logx"
	"carsihub/migrations"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := logx.New(os.Stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		log.Error("veritabanına bağlanılamadı", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		log.Error("migrasyonlar okunamadı", "error", err)
		os.Exit(1)
	}
	results, err := provider.Up(ctx)
	for _, r := range results {
		log.Info("migrasyon uygulandı", "version", r.Source.Version, "file", r.Source.Path, "duration_ms", r.Duration.Milliseconds())
	}
	if err != nil {
		log.Error("migrasyon başarısız", "error", err)
		os.Exit(1)
	}

	log.Info("migrasyon tamam", "applied", len(results))
}
