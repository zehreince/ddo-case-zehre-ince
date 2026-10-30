package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"carsihub/internal/platform/pagination"
)

// dbtx hem *pgxpool.Pool hem pgx.Tx tarafından karşılanır.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type userRecord struct {
	User
	PasswordHash string
	Disabled     bool
}

func findUserByEmail(ctx context.Context, q dbtx, email string) (*userRecord, error) {
	var u userRecord
	err := q.QueryRow(ctx, `
		SELECT id::text, email, full_name, role, password_hash, disabled_at IS NOT NULL
		FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.PasswordHash, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

func findUserByID(ctx context.Context, q dbtx, id string) (*userRecord, error) {
	var u userRecord
	err := q.QueryRow(ctx, `
		SELECT id::text, email, full_name, role, password_hash, disabled_at IS NOT NULL
		FROM users WHERE id = $1::uuid`, id).
		Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.PasswordHash, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

type refreshRecord struct {
	ID        string
	UserID    string
	FamilyID  string
	ExpiresAt time.Time
	Revoked   bool
}

// lockRefreshToken satırı kilitler: aynı token'la eşzamanlı iki yenileme
// isteğinden yalnızca biri döndürme yapabilir, diğeri iptal edilmiş görür.
func lockRefreshToken(ctx context.Context, tx dbtx, hash []byte) (*refreshRecord, error) {
	var r refreshRecord
	err := tx.QueryRow(ctx, `
		SELECT id::text, user_id::text, family_id::text, expires_at, revoked_at IS NOT NULL
		FROM refresh_tokens WHERE token_hash = $1
		FOR UPDATE`, hash).
		Scan(&r.ID, &r.UserID, &r.FamilyID, &r.ExpiresAt, &r.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// insertRefreshToken familyID boşsa yeni bir aile başlatır.
func insertRefreshToken(ctx context.Context, tx dbtx, userID, familyID string, hash []byte, expiresAt time.Time) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
		VALUES ($1::uuid, COALESCE(NULLIF($2, '')::uuid, gen_random_uuid()), $3, $4)
		RETURNING id::text`, userID, familyID, hash, expiresAt).Scan(&id)
	return id, err
}

func markReplaced(ctx context.Context, tx dbtx, oldID, newID string) error {
	_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now(), replaced_by = $2::uuid WHERE id = $1::uuid`, oldID, newID)
	return err
}

func revokeFamily(ctx context.Context, tx dbtx, familyID string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1::uuid AND revoked_at IS NULL`, familyID)
	return tag.RowsAffected(), err
}

// listUsers (created_at, id) üzerinde keyset sayfalaması yapar; users_keyset_idx kullanılır.
// Bir sonraki sayfa olup olmadığını anlamak için limit+1 satır okunur.
func listUsers(ctx context.Context, q dbtx, after *pagination.Cursor, limit int) ([]User, *pagination.Cursor, error) {
	var (
		rows pgx.Rows
		err  error
	)
	const cols = `SELECT id::text, email, full_name, role, created_at FROM users`
	if after == nil {
		rows, err = q.Query(ctx, cols+` ORDER BY created_at DESC, id DESC LIMIT $1`, limit+1)
	} else {
		rows, err = q.Query(ctx, cols+`
			WHERE (created_at, id) < ($1, $2::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $3`, after.CreatedAt, after.ID, limit+1)
	}
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	users := make([]User, 0, limit)
	var last pagination.Cursor
	for rows.Next() {
		var u User
		var created time.Time
		if err := rows.Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &created); err != nil {
			return nil, nil, err
		}
		if len(users) == limit {
			return users, &last, rows.Err() // limit+1'inci satır: sonraki sayfa var
		}
		users = append(users, u)
		last = pagination.Cursor{CreatedAt: created, ID: u.ID}
	}
	return users, nil, rows.Err()
}
