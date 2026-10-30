// Package dbtest entegrasyon testleri için gerçek PostgreSQL 16 sağlar.
//
// Test ikilisi başına tek konteyner açılır ve migrasyonlar bir şablon
// veritabanına bir kez uygulanır. Her test bu şablondan kendi boş
// veritabanını kopyalar; testler birbirini kirletmez ve paralel koşabilir.
//
// Docker yoksa testler başarısız olmak yerine atlanır (-short ile de atlanır).
package dbtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"carsihub/internal/platform/migrate"
)

const (
	image    = "postgres:16.10-alpine"
	template = "carsihub_template"
)

var (
	once    sync.Once
	baseURL string // şablon dışındaki veritabanlarına bağlanmak için
	initErr error
	counter atomic.Int64
)

// New migrasyonları uygulanmış, bu teste özel bir veritabanına bağlı havuz döner.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("entegrasyon testi: -short ile atlandı")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	once.Do(func() { initErr = start() })
	if initErr != nil {
		t.Fatalf("test veritabanı başlatılamadı: %v", initErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), counter.Add(1))

	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, template)); err != nil {
		t.Fatalf("test veritabanı oluşturulamadı: %v", err)
	}

	pool, err := pgxpool.New(ctx, withDB(baseURL, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// URL bu paketin konteynerindeki bir veritabanının bağlantı adresini döner.
func URL(t testing.TB, pool *pgxpool.Pool) string {
	t.Helper()
	return withDB(baseURL, pool.Config().ConnConfig.Database)
}

func start() error {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return err
	}
	// Konteyner Ryuk tarafından test süreci bitince temizlenir.
	baseURL, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}

	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		return err
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+template); err != nil {
		return err
	}

	tmpl, err := pgxpool.New(ctx, withDB(baseURL, template))
	if err != nil {
		return err
	}
	_, err = migrate.Up(ctx, tmpl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tmpl.Close() // şablona açık bağlantı kalırsa CREATE DATABASE ... TEMPLATE başarısız olur
	return err
}

// withDB bağlantı adresindeki veritabanı adını değiştirir.
func withDB(url, db string) string {
	q := ""
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url, q = url[:i], url[i:]
	}
	return url[:strings.LastIndexByte(url, '/')+1] + db + q
}
