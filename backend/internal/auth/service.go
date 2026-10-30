package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/pagination"
)

type Service struct {
	pool       *pgxpool.Pool
	tokens     *Tokens
	refreshTTL time.Duration
	now        func() time.Time
}

func NewService(pool *pgxpool.Pool, tokens *Tokens, refreshTTL time.Duration) *Service {
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL, now: time.Now}
}

// Session giriş ve yenileme yanıtıdır.
type Session struct {
	AccessToken      string    `json:"access_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        int       `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"-"`
	User             User      `json:"user"`
}

func (s *Service) Login(ctx context.Context, in LoginInput, requestID string) (*Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u, err := findUserByEmail(ctx, tx, in.Email)
	if err != nil {
		return nil, err
	}
	switch {
	case u == nil:
		burnPasswordCheck(in.Password)
		s.recordFailedLogin(ctx, in.Email, "", requestID)
		return nil, ErrInvalidCredentials
	case !CheckPassword(u.PasswordHash, in.Password), u.Disabled:
		s.recordFailedLogin(ctx, in.Email, u.ID, requestID)
		return nil, ErrInvalidCredentials
	}

	session, _, err := s.issue(ctx, tx, u.User, "")
	if err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: u.ID, ActorRole: string(u.Role), Action: "auth.login",
		EntityType: "user", EntityID: u.ID, RequestID: requestID,
	}); err != nil {
		return nil, err
	}
	return session, tx.Commit(ctx)
}

// Refresh refresh token'ı tek kullanımlık olarak döndürür (rotation). İptal
// edilmiş bir token gelirse bu, token'ın çalındığına işarettir: aynı oturum
// ailesindeki bütün token'lar iptal edilir ve kullanıcı yeniden giriş yapar.
func (s *Service) Refresh(ctx context.Context, raw, requestID string) (*Session, error) {
	if raw == "" {
		return nil, ErrInvalidToken
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := lockRefreshToken(ctx, tx, HashRefreshToken(raw))
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, ErrInvalidToken
	}
	if old.Revoked {
		revoked, err := revokeFamily(ctx, tx, old.FamilyID)
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			ActorID: old.UserID, Action: "auth.refresh_reuse_detected",
			EntityType: "refresh_token_family", EntityID: old.FamilyID,
			New: map[string]any{"revoked_tokens": revoked}, RequestID: requestID,
		}); err != nil {
			return nil, err
		}
		// İptal kalıcı olmalı; hata dönsek de transaction'ı onaylıyoruz.
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrTokenReuse
	}
	if !s.now().Before(old.ExpiresAt) {
		return nil, ErrInvalidToken
	}

	u, err := findUserByID(ctx, tx, old.UserID)
	if err != nil {
		return nil, err
	}
	if u == nil || u.Disabled {
		if _, err := revokeFamily(ctx, tx, old.FamilyID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrInvalidToken
	}

	session, newID, err := s.issue(ctx, tx, u.User, old.FamilyID)
	if err != nil {
		return nil, err
	}
	if err := markReplaced(ctx, tx, old.ID, newID); err != nil {
		return nil, err
	}
	return session, tx.Commit(ctx)
}

// Logout token'ın ait olduğu oturumu (aileyi) sonlandırır. Bilinmeyen ya da
// zaten iptal edilmiş token için de başarılıdır; çıkış idempotenttir.
func (s *Service) Logout(ctx context.Context, raw, requestID string) error {
	if raw == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tok, err := lockRefreshToken(ctx, tx, HashRefreshToken(raw))
	if err != nil || tok == nil {
		return err
	}
	revoked, err := revokeFamily(ctx, tx, tok.FamilyID)
	if err != nil {
		return err
	}
	if revoked > 0 {
		if err := audit.Record(ctx, tx, audit.Entry{
			ActorID: tok.UserID, Action: "auth.logout",
			EntityType: "refresh_token_family", EntityID: tok.FamilyID, RequestID: requestID,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) Me(ctx context.Context, userID string) (*User, error) {
	u, err := findUserByID(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	if u == nil || u.Disabled {
		return nil, ErrInvalidToken
	}
	return &u.User, nil
}

func (s *Service) ListUsers(ctx context.Context, after *pagination.Cursor, limit int) (pagination.Page[User], error) {
	users, next, err := listUsers(ctx, s.pool, after, limit)
	if err != nil {
		return pagination.Page[User]{}, err
	}
	page := pagination.Page[User]{Items: users}
	if next != nil {
		page.NextCursor = next.Encode()
	}
	return page, nil
}

func (s *Service) issue(ctx context.Context, tx pgx.Tx, u User, familyID string) (*Session, string, error) {
	access, _, err := s.tokens.IssueAccess(Principal{UserID: u.ID, Role: u.Role})
	if err != nil {
		return nil, "", err
	}
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return nil, "", err
	}
	refreshExp := s.now().Add(s.refreshTTL)
	id, err := insertRefreshToken(ctx, tx, u.ID, familyID, hash, refreshExp)
	if err != nil {
		return nil, "", err
	}
	return &Session{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int(s.tokens.accessTTL.Seconds()),
		RefreshToken:     raw,
		RefreshExpiresAt: refreshExp,
		User:             u,
	}, id, nil
}

// recordFailedLogin başarısız denemeyi ana transaction'dan bağımsız yazar;
// giriş reddedildiği için o transaction geri alınacaktır.
func (s *Service) recordFailedLogin(ctx context.Context, email, userID, requestID string) {
	entityID := userID
	if entityID == "" {
		entityID = email
	}
	_ = audit.Record(ctx, s.pool, audit.Entry{
		ActorID: userID, Action: "auth.login_failed",
		EntityType: "user", EntityID: entityID, RequestID: requestID,
	})
}

// IsAuthError oturumla ilgili (401'e çevrilecek) hataları ayırt eder.
func IsAuthError(err error) bool {
	return errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrTokenReuse)
}
