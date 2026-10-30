// Komut migrate, compose içinde API'den önce bir kez çalışır: şemayı
// günceller ve demo verisini yükler. Tekrar çalıştırmak güvenlidir.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"carsihub/internal/platform/config"
	"carsihub/internal/platform/db"
	"carsihub/internal/platform/logx"
	"carsihub/internal/platform/migrate"
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

	applied, err := migrate.Up(ctx, pool, log)
	if err != nil {
		log.Error("migrasyon başarısız", "error", err)
		os.Exit(1)
	}
	log.Info("migrasyon tamam", "applied", applied)
}
