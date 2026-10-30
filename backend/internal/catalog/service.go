package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/audit"
	"carsihub/internal/platform/pagination"
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// SellerID kullanıcının bağlı olduğu satıcıyı döner.
func (s *Service) SellerID(ctx context.Context, userID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM sellers WHERE user_id = $1::uuid`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotSeller
	}
	return id, err
}

// CreateProduct ürünü, varyantlarını ve satıcının bu varyantlar için tekliflerini tek işlemde oluşturur.
func (s *Service) CreateProduct(ctx context.Context, userID string, in CreateProductInput, requestID string) (*Product, error) {
	sellerID, err := s.SellerID(ctx, userID)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	attrs := in.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	var productID string
	err = tx.QueryRow(ctx, `
		INSERT INTO products (category_id, name, description, attributes)
		VALUES ($1, $2, $3, $4) RETURNING id::text`, in.CategoryID, in.Name, in.Description, attrs).Scan(&productID)
	if isFK(err) {
		return nil, ErrBadCategory
	}
	if err != nil {
		return nil, err
	}
	for _, v := range in.Variants {
		va := v.Attributes
		if va == nil {
			va = map[string]string{}
		}
		var variantID string
		err := tx.QueryRow(ctx, `
			INSERT INTO product_variants (product_id, sku, attributes) VALUES ($1::uuid, $2, $3)
			RETURNING id::text`, productID, v.SKU, va).Scan(&variantID)
		if isUnique(err) {
			return nil, ErrDuplicateSKU
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO offers (seller_id, variant_id, price_kurus, stock_on_hand) VALUES ($1::uuid, $2::uuid, $3, $4)`,
			sellerID, variantID, v.PriceKurus, v.Stock); err != nil {
			return nil, err
		}
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: userID, ActorRole: "seller", Action: "product.created",
		EntityType: "product", EntityID: productID, New: in, RequestID: requestID,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Product(ctx, productID)
}

// SetStock satıcının kendi teklifindeki eldeki stok miktarını değiştirir.
// Tutulan (rezerve) miktarın altına inemez; bunu veritabanı kısıtı da zorlar.
func (s *Service) SetStock(ctx context.Context, userID, offerID string, stock int, requestID string) (*Offer, error) {
	sellerID, err := s.SellerID(ctx, userID)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var old, reserved int
	err = tx.QueryRow(ctx, `
		SELECT stock_on_hand, stock_reserved FROM offers
		WHERE id = $1::uuid AND seller_id = $2::uuid FOR UPDATE`, offerID, sellerID).Scan(&old, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound // başka satıcının teklifi de "bulunamadı"
	}
	if err != nil {
		return nil, err
	}
	if stock < reserved {
		return nil, ErrBelowReserved
	}
	if _, err := tx.Exec(ctx, `UPDATE offers SET stock_on_hand = $2, updated_at = now() WHERE id = $1::uuid`, offerID, stock); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorID: userID, ActorRole: "seller", Action: "offer.stock_set",
		EntityType: "offer", EntityID: offerID, RequestID: requestID,
		Old: map[string]int{"stock_on_hand": old}, New: map[string]int{"stock_on_hand": stock},
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.offer(ctx, offerID)
}

const offerCols = `
	o.id::text, o.seller_id::text, se.name, o.variant_id::text, p.name, v.sku,
	o.price_kurus, o.stock_on_hand, o.stock_reserved, o.stock_on_hand - o.stock_reserved, o.created_at
	FROM offers o
	JOIN sellers se ON se.id = o.seller_id
	JOIN product_variants v ON v.id = o.variant_id
	JOIN products p ON p.id = v.product_id`

func scanOffer(row pgx.Row) (Offer, error) {
	var o Offer
	err := row.Scan(&o.ID, &o.SellerID, &o.SellerName, &o.VariantID, &o.ProductName, &o.SKU,
		&o.PriceKurus, &o.StockOnHand, &o.Reserved, &o.Available, &o.CreatedAt)
	return o, err
}

func (s *Service) offer(ctx context.Context, id string) (*Offer, error) {
	o, err := scanOffer(s.pool.QueryRow(ctx, `SELECT `+offerCols+` WHERE o.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &o, err
}

// SellerOffers satıcının kendi tekliflerini keyset sayfalamayla listeler.
func (s *Service) SellerOffers(ctx context.Context, userID string, after *pagination.Cursor, limit int) (pagination.Page[Offer], error) {
	page := pagination.Page[Offer]{Items: []Offer{}}
	sellerID, err := s.SellerID(ctx, userID)
	if err != nil {
		return page, err
	}
	args := []any{sellerID, limit + 1}
	where := `o.seller_id = $1::uuid`
	if after != nil {
		where += ` AND (o.created_at, o.id) < ($3, $4::uuid)`
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+offerCols+` WHERE `+where+` ORDER BY o.created_at DESC, o.id DESC LIMIT $2`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			prev := page.Items[limit-1]
			page.NextCursor = pagination.Cursor{CreatedAt: prev.CreatedAt, ID: prev.ID}.Encode()
			break
		}
		page.Items = append(page.Items, o)
	}
	return page, rows.Err()
}

// Products ürünleri en düşük fiyat ve satılabilir stok özetiyle listeler.
// (Metin arama ve filtreler T-5'te ayrı bir uçta.)
func (s *Service) Products(ctx context.Context, after *pagination.Cursor, limit int) (pagination.Page[ProductSummary], error) {
	page := pagination.Page[ProductSummary]{Items: []ProductSummary{}}
	args := []any{limit + 1}
	where := `true`
	if after != nil {
		where = `(p.created_at, p.id) < ($2, $3::uuid)`
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.name, c.id, c.name, agg.min_price, coalesce(agg.available, 0), coalesce(agg.offers, 0), p.created_at
		FROM products p
		JOIN categories c ON c.id = p.category_id
		LEFT JOIN LATERAL (
			SELECT min(o.price_kurus) FILTER (WHERE o.stock_on_hand > o.stock_reserved) AS min_price,
			       sum(o.stock_on_hand - o.stock_reserved)::int AS available,
			       count(*)::int AS offers
			FROM product_variants v JOIN offers o ON o.variant_id = v.id AND o.active
			WHERE v.product_id = p.id
		) agg ON true
		WHERE `+where+`
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $1`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ProductSummary
		if err := rows.Scan(&p.ID, &p.Name, &p.CategoryID, &p.CategoryName, &p.MinPriceKurus, &p.Available, &p.OfferCount, &p.CreatedAt); err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			prev := page.Items[limit-1]
			page.NextCursor = pagination.Cursor{CreatedAt: prev.CreatedAt, ID: prev.ID}.Encode()
			break
		}
		page.Items = append(page.Items, p)
	}
	return page, rows.Err()
}

// Product ürünü varyantları ve her varyantın satıcı teklifleriyle döner.
func (s *Service) Product(ctx context.Context, id string) (*Product, error) {
	p := Product{ID: id, Variants: []Variant{}}
	err := s.pool.QueryRow(ctx, `
		SELECT p.name, p.description, c.id, c.name, p.attributes
		FROM products p JOIN categories c ON c.id = p.category_id WHERE p.id = $1::uuid`, id).
		Scan(&p.Name, &p.Description, &p.CategoryID, &p.CategoryName, &p.Attributes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT v.id::text, v.sku, v.attributes,
		       o.id::text, o.seller_id::text, se.name, o.price_kurus, o.stock_on_hand, o.stock_reserved, o.created_at
		FROM product_variants v
		LEFT JOIN offers o ON o.variant_id = v.id AND o.active
		LEFT JOIN sellers se ON se.id = o.seller_id
		WHERE v.product_id = $1::uuid
		ORDER BY v.sku, o.price_kurus NULLS LAST, o.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			v                       Variant
			offerID, sellerID, name *string
			price                   *int64
			onHand, reserved        *int
			created                 *time.Time
			o                       Offer
		)
		if err := rows.Scan(&v.ID, &v.SKU, &v.Attributes, &offerID, &sellerID, &name, &price, &onHand, &reserved, &created); err != nil {
			return nil, err
		}
		if n := len(p.Variants); n == 0 || p.Variants[n-1].ID != v.ID {
			v.Offers = []Offer{}
			p.Variants = append(p.Variants, v)
		}
		if offerID != nil {
			o.ID, o.SellerID, o.SellerName, o.VariantID = *offerID, *sellerID, *name, v.ID
			o.PriceKurus, o.StockOnHand, o.Reserved, o.CreatedAt = *price, *onHand, *reserved, *created
			o.Available = o.StockOnHand - o.Reserved
			last := &p.Variants[len(p.Variants)-1]
			last.Offers = append(last.Offers, o)
		}
	}
	return &p, rows.Err()
}

func (s *Service) Categories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, parent_id, name, slug FROM categories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Category, error) {
		var c Category
		err := r.Scan(&c.ID, &c.ParentID, &c.Name, &c.Slug)
		return c, err
	})
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isFK(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}
