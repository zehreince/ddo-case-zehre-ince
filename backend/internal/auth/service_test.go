package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"carsihub/internal/platform/dbtest"
	"carsihub/internal/platform/pagination"
)

const testPassword = "Parola123!"

func newTestService(t *testing.T) (*Service, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return NewService(pool, NewTokens(secret, time.Minute), time.Hour), pool
}

func createUser(t *testing.T, pool *pgxpool.Pool, email string, role Role) string {
	t.Helper()
	hash, err := HashPassword(testPassword, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	err = pool.QueryRow(context.Background(), `
		INSERT INTO users (email, password_hash, full_name, role) VALUES ($1, $2, 'Test', $3)
		RETURNING id::text`, email, hash, role).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func auditCount(t *testing.T, pool *pgxpool.Pool, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func login(t *testing.T, s *Service, email string) *Session {
	t.Helper()
	sess, err := s.Login(context.Background(), LoginInput{Email: email, Password: testPassword}, "")
	if err != nil {
		t.Fatalf("giriş başarısız: %v", err)
	}
	return sess
}

func TestLogin(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	id := createUser(t, pool, "a@b.co", RoleSeller)
	createUser(t, pool, "kapali@b.co", RoleCustomer)
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE email = 'kapali@b.co'`); err != nil {
		t.Fatal(err)
	}

	sess := login(t, s, "a@b.co")
	if sess.User.ID != id || sess.User.Role != RoleSeller || sess.RefreshToken == "" {
		t.Fatalf("oturum yanlış: %+v", sess)
	}
	p, err := s.tokens.ParseAccess(sess.AccessToken)
	if err != nil || p.UserID != id || p.Role != RoleSeller {
		t.Fatalf("access token yanlış: %+v %v", p, err)
	}

	for name, in := range map[string]LoginInput{
		"yanlış parola":    {"a@b.co", "yanlis"},
		"bilinmeyen":       {"yok@b.co", testPassword},
		"devre dışı hesap": {"kapali@b.co", testPassword},
	} {
		if _, err := s.Login(ctx, in, ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: ErrInvalidCredentials bekleniyordu, gelen %v", name, err)
		}
	}

	if got := auditCount(t, pool, "auth.login"); got != 1 {
		t.Errorf("başarılı giriş audit sayısı %d", got)
	}
	if got := auditCount(t, pool, "auth.login_failed"); got != 3 {
		t.Errorf("başarısız giriş audit sayısı %d", got)
	}
}

func TestRefresh_RotatesToken(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	createUser(t, pool, "a@b.co", RoleCustomer)
	first := login(t, s, "a@b.co")

	second, err := s.Refresh(ctx, first.RefreshToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken || second.AccessToken == "" {
		t.Fatal("yenilemede yeni refresh token üretilmeli")
	}

	var replaced bool
	if err := pool.QueryRow(ctx, `SELECT replaced_by IS NOT NULL AND revoked_at IS NOT NULL FROM refresh_tokens WHERE token_hash = $1`,
		HashRefreshToken(first.RefreshToken)).Scan(&replaced); err != nil || !replaced {
		t.Fatalf("eski token iptal edilip yenisine bağlanmalı: %v %v", replaced, err)
	}
	if _, err := s.Refresh(ctx, second.RefreshToken, ""); err != nil {
		t.Fatalf("yeni token kullanılabilmeli: %v", err)
	}
}

func TestRefresh_ReuseRevokesWholeFamily(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	createUser(t, pool, "a@b.co", RoleCustomer)
	stolen := login(t, s, "a@b.co")

	// Meşru kullanıcı yeniler; saldırgan elindeki eski token'ı dener.
	legit, err := s.Refresh(ctx, stolen.RefreshToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, stolen.RefreshToken, ""); !errors.Is(err, ErrTokenReuse) {
		t.Fatalf("ErrTokenReuse bekleniyordu: %v", err)
	}
	// Ailenin tamamı iptal edildiği için meşru token da artık geçersiz.
	if _, err := s.Refresh(ctx, legit.RefreshToken, ""); !errors.Is(err, ErrTokenReuse) {
		t.Fatalf("aile iptal edilmeli: %v", err)
	}
	if auditCount(t, pool, "auth.refresh_reuse_detected") < 1 {
		t.Fatal("yeniden kullanım audit'e yazılmalı")
	}
}

func TestRefresh_ConcurrentRequestsOnlyOneWins(t *testing.T) {
	s, pool := newTestService(t)
	createUser(t, pool, "a@b.co", RoleCustomer)
	sess := login(t, s, "a@b.co")

	const n = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Refresh(context.Background(), sess.RefreshToken, ""); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("aynı token'la %d eşzamanlı yenilemeden tam 1'i başarılı olmalı, %d oldu", n, success)
	}
}

func TestRefresh_RejectsUnknownExpiredAndDisabled(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	createUser(t, pool, "a@b.co", RoleCustomer)

	if _, err := s.Refresh(ctx, "", ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("boş token: %v", err)
	}
	if _, err := s.Refresh(ctx, "bilinmeyen", ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("bilinmeyen token: %v", err)
	}

	sess := login(t, s, "a@b.co")
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.Refresh(ctx, sess.RefreshToken, ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("süresi dolmuş token: %v", err)
	}
	s.now = time.Now

	sess = login(t, s, "a@b.co")
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, sess.RefreshToken, ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("devre dışı kullanıcı: %v", err)
	}
}

func TestLogout_RevokesSessionAndIsIdempotent(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	createUser(t, pool, "a@b.co", RoleCustomer)
	sess := login(t, s, "a@b.co")

	for range 2 {
		if err := s.Logout(ctx, sess.RefreshToken, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Logout(ctx, "bilinmeyen", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, sess.RefreshToken, ""); err == nil {
		t.Fatal("çıkıştan sonra yenileme reddedilmeli")
	}
	if got := auditCount(t, pool, "auth.logout"); got != 1 {
		t.Fatalf("çıkış bir kez audit'e yazılmalı, %d", got)
	}
}

func TestMe(t *testing.T) {
	s, pool := newTestService(t)
	id := createUser(t, pool, "a@b.co", RoleFinance)
	u, err := s.Me(context.Background(), id)
	if err != nil || u.Email != "a@b.co" || u.Role != RoleFinance {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if _, err := s.Me(context.Background(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("silinmiş kullanıcı: %v", err)
	}
}

func TestListUsers_KeysetPagination(t *testing.T) {
	s, pool := newTestService(t)
	ctx := context.Background()
	const total = 23
	// Aynı created_at değerleri id ile sıralanmalı; bu yüzden bir kısmı aynı anda eklenir.
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email, password_hash, full_name, role, created_at)
		SELECT 'u' || i || '@b.co', 'x', 'U' || i, 'customer', '2026-01-01'::timestamptz + (i / 3) * interval '1 minute'
		FROM generate_series(1, $1) i`, total); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var cursor *pagination.Cursor
	pages := 0
	for {
		page, err := s.ListUsers(ctx, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, u := range page.Items {
			if seen[u.ID] {
				t.Fatalf("%s iki kez döndü", u.Email)
			}
			seen[u.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if cursor, err = pagination.Decode(page.NextCursor); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != total || pages != 3 {
		t.Fatalf("%d kullanıcı / %d sayfa, beklenen %d / 3", len(seen), pages, total)
	}
}

func TestListUsers_ExactPageHasNoNextCursor(t *testing.T) {
	s, pool := newTestService(t)
	for i := range 5 {
		createUser(t, pool, fmt.Sprintf("u%d@b.co", i), RoleCustomer)
	}
	page, err := s.ListUsers(context.Background(), nil, 5)
	if err != nil || len(page.Items) != 5 || page.NextCursor != "" {
		t.Fatalf("items=%d next=%q err=%v", len(page.Items), page.NextCursor, err)
	}
}
