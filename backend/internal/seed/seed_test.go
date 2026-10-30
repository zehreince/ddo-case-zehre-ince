package seed

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"carsihub/internal/auth"
	"carsihub/internal/platform/dbtest"
)

func TestRun_CreatesOneUserPerRoleAndIsIdempotent(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for range 2 {
		if err := Run(ctx, pool, bcrypt.MinCost, log); err != nil {
			t.Fatal(err)
		}
	}

	var users, roles int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT role) FROM users`).Scan(&users, &roles); err != nil {
		t.Fatal(err)
	}
	if users != len(DemoUsers) || roles != 6 {
		t.Fatalf("users=%d roles=%d, her rol için tam bir kullanıcı bekleniyordu", users, roles)
	}

	// Katalog iki kez çalıştırılsa da tek kopya; demo satıcı hesabı satıcıya bağlı.
	var products, offers, linked int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM products), (SELECT count(*) FROM offers),
		       (SELECT count(*) FROM sellers s JOIN users u ON u.id = s.user_id WHERE u.email = 'satici@carsihub.test')`).
		Scan(&products, &offers, &linked); err != nil {
		t.Fatal(err)
	}
	if products != len(demoProducts) || offers == 0 || linked != 1 {
		t.Fatalf("products=%d offers=%d linked=%d", products, offers, linked)
	}

	svc := auth.NewService(pool, auth.NewTokens("0123456789abcdef0123456789abcdef", time.Minute), time.Hour)
	for _, u := range DemoUsers {
		sess, err := svc.Login(ctx, auth.LoginInput{Email: u.Email, Password: DemoPassword}, "")
		if err != nil || sess.User.Role != u.Role {
			t.Errorf("%s ile giriş yapılamadı: %v", u.Email, err)
		}
	}
}
