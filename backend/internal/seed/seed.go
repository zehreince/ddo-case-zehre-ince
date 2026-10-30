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
	return nil
}
