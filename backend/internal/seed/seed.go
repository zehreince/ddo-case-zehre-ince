// Package seed demo verisini yükler. Tekrar çalıştırmak güvenlidir: var olan
// kayıtlara dokunmaz. Büyük hacimli veri (1M ürün) simülatörün işidir.
package seed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/auth"
)

// DemoPassword tüm demo hesaplarının parolasıdır; TESTING.md'de listelenir.
// Yalnızca yerel değerlendirme içindir.
const DemoPassword = "Demo123!"

type DemoUser struct {
	Email    string
	FullName string
	Role     auth.Role
}

var DemoUsers = []DemoUser{
	{"musteri@carsihub.test", "Ayşe Müşteri", auth.RoleCustomer},
	{"satici@carsihub.test", "Mehmet Satıcı", auth.RoleSeller},
	{"operasyon@carsihub.test", "Elif Operasyon", auth.RoleOperations},
	{"finans@carsihub.test", "Can Finans", auth.RoleFinance},
	{"kargo@carsihub.test", "Kargo Entegrasyonu", auth.RoleCarrier},
	{"yonetici@carsihub.test", "Deniz Yönetici", auth.RoleAdmin},
}

func Run(ctx context.Context, pool *pgxpool.Pool, bcryptCost int, log *slog.Logger) error {
	created := 0
	for _, u := range DemoUsers {
		hash, err := auth.HashPassword(DemoPassword, bcryptCost)
		if err != nil {
			return err
		}
		tag, err := pool.Exec(ctx, `
			INSERT INTO users (email, password_hash, full_name, role)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (email) DO NOTHING`, u.Email, hash, u.FullName, u.Role)
		if err != nil {
			return fmt.Errorf("demo kullanıcı %s: %w", u.Email, err)
		}
		created += int(tag.RowsAffected())
	}
	log.Info("demo kullanıcılar hazır", "created", created, "total", len(DemoUsers))

	products, err := Catalog(ctx, pool)
	if err != nil {
		return fmt.Errorf("demo katalog: %w", err)
	}
	log.Info("demo katalog hazır", "created_products", products, "total_products", len(demoProducts))

	// Komisyon: varsayılan %15, Elektronik %8 (2020'den beri geçerli; yeni oran eklenince çakışma veritabanında engelli).
	if _, err := pool.Exec(ctx, `
		INSERT INTO commission_rates (category_id, rate_bps, valid_from)
		SELECT c, r, '2020-01-01'::timestamptz FROM (VALUES
			(NULL::bigint, 1500),
			((SELECT id FROM categories WHERE slug = 'elektronik'), 800)
		) v(c, r)
		WHERE NOT EXISTS (SELECT 1 FROM commission_rates x WHERE x.category_id IS NOT DISTINCT FROM v.c)`); err != nil {
		return fmt.Errorf("komisyon oranları: %w", err)
	}

	coupons, err := Coupons(ctx, pool)
	if err != nil {
		return fmt.Errorf("demo kuponlar: %w", err)
	}
	log.Info("demo kuponlar hazır", "created", coupons, "total", len(DemoCoupons))
	return nil
}
