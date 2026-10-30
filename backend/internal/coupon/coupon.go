// Package coupon kupon tanımlarını ve kullanımlarını yönetir (T-3). İndirim
// hesabı pricing paketindedir; bu paket kuponları veritabanından çözer,
// kullanım limitlerini eşzamanlılığa dayanıklı biçimde uygular.
package coupon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/pagination"
	"carsihub/internal/pricing"
)

// Kupon çözümleme ve kullanım ret nedenleri (pricing.Reason* ile aynı listede gösterilir).
const (
	ReasonNotFound   = "not_found"
	ReasonNotStarted = "not_started"
	ReasonExpired    = "expired"
	ReasonUsageLimit = "usage_limit"
	ReasonUserLimit  = "user_limit"
)

var (
	ErrDuplicateCode = errors.New("bu kupon kodu zaten var")
	ErrNotSeller     = errors.New("bu kullanıcıya bağlı satıcı yok")
	ErrNotFound      = errors.New("kupon bulunamadı")
)

var codePattern = regexp.MustCompile(`^[A-Z0-9_-]{3,32}$`)

type Coupon struct {
	pricing.Coupon
	StartsAt       time.Time  `json:"starts_at"`
	EndsAt         *time.Time `json:"ends_at"`
	MaxRedemptions *int       `json:"max_redemptions"`
	PerUserLimit   *int       `json:"per_user_limit"`
	RedeemedCount  int        `json:"redeemed_count"`
	Active         bool       `json:"active"`
	CreatedAt      time.Time  `json:"created_at"`
}

type CreateInput struct {
	Code             string     `json:"code"`
	Kind             string     `json:"kind"`
	Value            int64      `json:"value"`
	MaxDiscountKurus int64      `json:"max_discount_kurus"`
	MinBasketKurus   int64      `json:"min_basket_kurus"`
	CategoryIDs      []int64    `json:"category_ids"`
	Stackable        *bool      `json:"stackable"`
	StartsAt         *time.Time `json:"starts_at"`
	EndsAt           *time.Time `json:"ends_at"`
	MaxRedemptions   *int       `json:"max_redemptions"`
	PerUserLimit     *int       `json:"per_user_limit"`
}

// NormalizeCode kodu büyük harfe çevirir; kullanıcı küçük harfle de girebilir.
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// Validate girdiyi doğrular; alan → hata eşlemesi döner.
func (in *CreateInput) Validate(scope pricing.Scope, sellerID string) (pricing.Coupon, map[string]string) {
	errs := map[string]string{}
	in.Code = NormalizeCode(in.Code)
	if !codePattern.MatchString(in.Code) {
		errs["code"] = "3-32 karakter; harf, rakam, _ ve -"
	}
	if in.MaxRedemptions != nil && *in.MaxRedemptions <= 0 {
		errs["max_redemptions"] = "pozitif olmalı"
	}
	if in.PerUserLimit != nil && *in.PerUserLimit <= 0 {
		errs["per_user_limit"] = "pozitif olmalı"
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		errs["ends_at"] = "başlangıçtan sonra olmalı"
	}
	stackable := true
	if in.Stackable != nil {
		stackable = *in.Stackable
	}
	c := pricing.Coupon{
		Code: in.Code, Scope: scope, SellerID: sellerID, Kind: pricing.Kind(in.Kind), Value: in.Value,
		MaxDiscountKurus: in.MaxDiscountKurus, MinBasketKurus: in.MinBasketKurus, CategoryIDs: in.CategoryIDs, Stackable: stackable,
	}
	if err := c.Validate(); err != nil {
		errs["coupon"] = err.Error()
	}
	return c, errs
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// SellerOf kullanıcının satıcı kimliğini döner.
func (s *Service) SellerOf(ctx context.Context, userID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM sellers WHERE user_id = $1::uuid`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotSeller
	}
	return id, err
}

func (s *Service) Create(ctx context.Context, actorID, actorRole string, c pricing.Coupon, in CreateInput, requestID string) (*Coupon, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	categories := c.CategoryIDs
	if categories == nil {
		categories = []int64{}
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO coupons (code, scope, seller_id, kind, value, max_discount_kurus, min_basket_kurus, category_ids,
		                     stackable, starts_at, ends_at, max_redemptions, per_user_limit, created_by)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, $7, $8, $9, COALESCE($10, now()), $11, $12, $13, $14::uuid)
		RETURNING id::text`,
		c.Code, c.Scope, c.SellerID, c.Kind, c.Value, c.MaxDiscountKurus, c.MinBasketKurus, categories,
		c.Stackable, in.StartsAt, in.EndsAt, in.MaxRedemptions, in.PerUserLimit, actorID).Scan(&id)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return nil, ErrDuplicateCode
	}
	if err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: actorID, ActorRole: actorRole, Action: "coupon.created", EntityType: "coupon", EntityID: id,
		New: c, RequestID: requestID,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	list, err := load(ctx, s.pool, `c.id = $1::uuid`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// List kuponları listeler; sellerID boş değilse yalnızca o satıcınınkiler.
func (s *Service) List(ctx context.Context, sellerID string, after *pagination.Cursor, limit int) (pagination.Page[Coupon], error) {
	where, args := `true`, []any{}
	if sellerID != "" {
		where, args = `c.seller_id = $1::uuid`, append(args, sellerID)
	}
	if after != nil {
		n := len(args)
		where += fmt.Sprintf(` AND (c.created_at, c.id) < ($%d, $%d::uuid)`, n+1, n+2)
		args = append(args, after.CreatedAt, after.ID)
	}
	list, err := load(ctx, s.pool, where+fmt.Sprintf(` ORDER BY c.created_at DESC, c.id DESC LIMIT %d`, limit+1), args...)
	if err != nil {
		return pagination.Page[Coupon]{}, err
	}
	page := pagination.Page[Coupon]{Items: list}
	if len(list) > limit {
		page.Items = list[:limit]
		last := page.Items[limit-1]
		page.NextCursor = pagination.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}.Encode()
	}
	return page, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func load(ctx context.Context, q querier, where string, args ...any) ([]Coupon, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id::text, c.code, c.scope, COALESCE(c.seller_id::text, ''), c.kind, c.value, c.max_discount_kurus,
		       c.min_basket_kurus, c.category_ids, c.stackable, c.starts_at, c.ends_at, c.max_redemptions,
		       c.per_user_limit, c.redeemed_count, c.active, c.created_at
		FROM coupons c WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Coupon, error) {
		var c Coupon
		err := r.Scan(&c.ID, &c.Code, &c.Scope, &c.SellerID, &c.Kind, &c.Value, &c.MaxDiscountKurus,
			&c.MinBasketKurus, &c.CategoryIDs, &c.Stackable, &c.StartsAt, &c.EndsAt, &c.MaxRedemptions,
			&c.PerUserLimit, &c.RedeemedCount, &c.Active, &c.CreatedAt)
		return c, err
	})
}

// Resolve kodları geçerli kuponlara çevirir. Bilinmeyen, pasif, başlamamış ya
// da süresi geçmiş kodlar ret nedeniyle döner. Sıra korunur (öncelik).
func Resolve(ctx context.Context, q querier, codes []string, now time.Time) ([]Coupon, []pricing.Rejected, error) {
	if len(codes) == 0 {
		return nil, nil, nil
	}
	norm := make([]string, len(codes))
	for i, c := range codes {
		norm[i] = NormalizeCode(c)
	}
	list, err := load(ctx, q, `c.code = ANY($1)`, norm)
	if err != nil {
		return nil, nil, err
	}
	byCode := map[string]Coupon{}
	for _, c := range list {
		byCode[c.Code] = c
	}
	var (
		found    []Coupon
		rejected []pricing.Rejected
	)
	for _, code := range norm {
		c, ok := byCode[code]
		switch {
		case !ok || !c.Active:
			rejected = append(rejected, pricing.Rejected{Code: code, Reason: ReasonNotFound})
		case now.Before(c.StartsAt):
			rejected = append(rejected, pricing.Rejected{Code: code, Reason: ReasonNotStarted})
		case c.EndsAt != nil && !now.Before(*c.EndsAt):
			rejected = append(rejected, pricing.Rejected{Code: code, Reason: ReasonExpired})
		default:
			found = append(found, c)
		}
	}
	return found, rejected, nil
}

// Redeem kuponun bir kullanımını sipariş adına ayırır. Toplam limit koşullu
// UPDATE ile korunur (T-1'deki stokla aynı desen); UPDATE kupon satırını
// kilitlediği için kullanıcı başına sayım da aynı kupon için sıraya girer.
// Limit doluysa ret nedeni döner (hata değil).
func Redeem(ctx context.Context, tx pgx.Tx, c Coupon, userID, orderID string, discount int64) (string, error) {
	var perUser *int
	err := tx.QueryRow(ctx, `
		UPDATE coupons SET redeemed_count = redeemed_count + 1
		WHERE id = $1::uuid AND (max_redemptions IS NULL OR redeemed_count < max_redemptions)
		RETURNING per_user_limit`, c.ID).Scan(&perUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReasonUsageLimit, nil
	}
	if err != nil {
		return "", err
	}
	if perUser != nil {
		var used int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM coupon_redemptions WHERE coupon_id = $1::uuid AND user_id = $2::uuid AND status = 'active'`,
			c.ID, userID).Scan(&used); err != nil {
			return "", err
		}
		if used >= *perUser {
			return ReasonUserLimit, nil // çağıran savepoint'i geri alır; sayaç artışı da geri gider
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO coupon_redemptions (coupon_id, user_id, order_id, discount_kurus, status)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'active')`, c.ID, userID, orderID, discount)
	return "", err
}

// Release siparişin aktif kupon kullanımlarını bırakır (iptal, süre dolumu, ödeme hatası).
func Release(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `
		WITH r AS (
			UPDATE coupon_redemptions SET status = 'released'
			WHERE order_id = $1::uuid AND status = 'active'
			RETURNING coupon_id
		)
		UPDATE coupons c SET redeemed_count = c.redeemed_count - 1
		FROM r WHERE c.id = r.coupon_id`, orderID)
	return err
}
