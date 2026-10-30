package db_test

import (
	"context"
	"testing"
	"time"

	"carsihub/internal/platform/db"
	"carsihub/internal/platform/dbtest"
)

func TestConnect_Succeeds(t *testing.T) {
	url := dbtest.URL(t, dbtest.New(t))
	pool, err := db.Connect(context.Background(), url, 3)
	if err != nil {
		t.Fatalf("bağlantı başarısız: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 3 {
		t.Fatalf("MaxConns=%d, beklenen 3", got)
	}
}

func TestConnect_RejectsInvalidURL(t *testing.T) {
	if _, err := db.Connect(context.Background(), "::geçersiz::", 1); err == nil {
		t.Fatal("geçersiz adres reddedilmeli")
	}
}

func TestConnect_GivesUpWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := db.Connect(ctx, "postgres://x:y@127.0.0.1:1/db?connect_timeout=1", 1)
	if err == nil {
		t.Fatal("ulaşılamayan veritabanında hata bekleniyordu")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("bağlam bittiğinde beklemeyi bırakmalı, %v sürdü", time.Since(start))
	}
}
