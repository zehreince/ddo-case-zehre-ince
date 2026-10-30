// Package outbox transactional outbox desenini uygular (T-2).
//
// Yazma: durum değişikliği yapan kod, olayı aynı transaction'da Write ile
// outbox_events tablosuna ekler. Değişiklik geri alınırsa olay da yok olur;
// kalıcı olursa olay da kalıcıdır. "Veritabanı güncellendi ama mesaj gitmedi"
// ya da tersi mümkün değildir.
//
// Yayımlama: Publisher olayları partiler halinde kiralar (lease), abone
// tüketicilere iletir. Tüm tüketiciler başarılıysa olay yayımlandı sayılır;
// biri hata verirse olay üstel bekleme ile yeniden denenir. Bu yüzden teslimat
// en az bir kezdir ve her tüketici idempotent olmalıdır (bkz. Once).
package outbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

type Event struct {
	ID            int64           `json:"id"`
	Type          string          `json:"type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
	Attempts      int             `json:"attempts"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Execer hem pgx.Tx hem havuz tarafından karşılanır; Write her zaman bir
// transaction içinden çağrılmalıdır.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func Write(ctx context.Context, tx Execer, eventType, aggregateType, aggregateID string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4)`, eventType, aggregateType, aggregateID, b)
	if err != nil {
		return fmt.Errorf("outbox yazılamadı: %w", err)
	}
	return nil
}

// Consumer bir olay türüne abone olan işleyicidir. Aynı olayı birden fazla
// kez alabilir; etkisi bir kez olmalıdır.
type Consumer interface {
	Name() string
	Handle(ctx context.Context, e Event) error
}

// Once fn'i (tüketici, olay) çifti için en fazla bir kez kalıcı olarak çalıştırır.
// İşlenmiş kaydı fn'in değişiklikleriyle aynı transaction'da atılır: fn hata
// verirse ikisi de geri alınır ve olay yeniden denendiğinde fn tekrar çalışır.
// Daha önce işlenmişse fn çağrılmaz ve false döner.
func Once(ctx context.Context, pool *pgxpool.Pool, consumer string, eventID int64, fn func(tx pgx.Tx) error) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (consumer, event_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, consumer, eventID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := fn(tx); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Backoff n'inci başarısız denemeden sonra beklenecek süredir: 1s, 2s, 4s … en fazla 5 dk.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 9 {
		return 5 * time.Minute
	}
	return min(time.Second<<(attempt-1), 5*time.Minute)
}

type Publisher struct {
	pool        *pgxpool.Pool
	log         *slog.Logger
	consumers   map[string][]Consumer
	batchSize   int
	lease       time.Duration
	maxAttempts int

	published prometheus.Counter
	failures  *prometheus.CounterVec
	dead      prometheus.Counter
}

func NewPublisher(pool *pgxpool.Pool, log *slog.Logger, batchSize, maxAttempts int) *Publisher {
	return &Publisher{
		pool: pool, log: log, consumers: map[string][]Consumer{},
		batchSize: batchSize, lease: 30 * time.Second, maxAttempts: maxAttempts,
		published: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_published_total", Help: "Tüm tüketicilere başarıyla iletilen olaylar.",
		}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_delivery_failures_total", Help: "Başarısız teslim denemeleri (yeniden denenecek).",
		}, []string{"consumer"}),
		dead: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_dead_total", Help: "Deneme sınırını aşıp bırakılan olaylar (elle müdahale gerekir).",
		}),
	}
}

func (p *Publisher) Collectors() []prometheus.Collector {
	return []prometheus.Collector{p.published, p.failures, p.dead}
}

func (p *Publisher) Subscribe(eventType string, c Consumer) {
	p.consumers[eventType] = append(p.consumers[eventType], c)
}

// Run bağlam bitene kadar her aralıkta bekleyen olayları boşaltır.
func (p *Publisher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for {
			n, err := p.PublishOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					p.log.Error("outbox turu başarısız", "error", err)
				}
				break
			}
			if n < p.batchSize {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// maxParallel bir partide aynı anda teslim edilen en fazla küme (aggregate) sayısıdır.
const maxParallel = 16

// PublishOnce bir parti olayı kiralar ve iletir; işlenen olay sayısını döner.
//
// Olaylar kümeye (aggregate) göre gruplanır: aynı siparişin olayları kendi
// içinde oluşma sırasıyla, farklı kümeler paralel iletilir. Böylece yavaş ya
// da ulaşılamayan bir bağımlılık (her deneme zaman aşımına kadar sürer)
// kuyruğun geri kalanını tıkamaz.
func (p *Publisher) PublishOnce(ctx context.Context) (int, error) {
	events, err := p.claim(ctx)
	if err != nil {
		return 0, err
	}
	var (
		groups = map[string][]Event{}
		order  []string
	)
	for _, e := range events {
		k := e.AggregateType + ":" + e.AggregateID
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for _, k := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(group []Event) {
			defer func() { <-sem; wg.Done() }()
			for _, e := range group {
				if !p.deliver(ctx, e) {
					// Kümenin önceki olayı başarısızsa sonrakiler sırayı bozmasın;
					// kiralamaları bırakılır, bir sonraki turda birlikte denenir.
					p.release(ctx, group)
					return
				}
			}
		}(groups[k])
	}
	wg.Wait()
	return len(events), nil
}

// release kiralamayı bırakır (yayımlanmamış olaylar için); olaylar bir sonraki turda yeniden alınır.
func (p *Publisher) release(ctx context.Context, events []Event) {
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	_, _ = p.pool.Exec(context.WithoutCancel(ctx), `
		UPDATE outbox_events SET lease_until = NULL WHERE id = ANY($1) AND published_at IS NULL`, ids)
}

// claim olayları kısa bir transaction'da kiralar. SKIP LOCKED ile aynı anda
// çalışan worker'lar aynı olayları seçmez; kiralama süresi dolan (çöken
// worker'ın bıraktığı) olaylar yeniden seçilebilir. Dış çağrılar transaction
// dışında yapılır; böylece yavaş bir bağımlılık kilit tutmaz.
//
// Küme içi sıra: aynı kümede daha önceki bir olay henüz yayımlanmadıysa
// (beklemede ya da başka worker'da) sonraki olay alınmaz. Bırakılmış (dead)
// olaylar sırayı tutmaz; aksi halde tek bir zehirli olay kümeyi sonsuza dek kilitlerdi.
func (p *Publisher) claim(ctx context.Context) ([]Event, error) {
	rows, err := p.pool.Query(ctx, `
		UPDATE outbox_events SET lease_until = now() + $2::interval
		WHERE id IN (
			SELECT e.id FROM outbox_events e
			WHERE e.published_at IS NULL AND NOT e.dead AND e.next_attempt_at <= now()
			  AND (e.lease_until IS NULL OR e.lease_until < now())
			  AND NOT EXISTS (
			      SELECT 1 FROM outbox_events prev
			      WHERE prev.aggregate_type = e.aggregate_type AND prev.aggregate_id = e.aggregate_id
			        AND prev.id < e.id AND prev.published_at IS NULL AND NOT prev.dead)
			ORDER BY e.id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, event_type, aggregate_type, aggregate_id, payload, attempts, created_at`,
		p.batchSize, p.lease.String())
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.Type, &e.AggregateType, &e.AggregateID, &e.Payload, &e.Attempts, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	// RETURNING sırası garanti değil; olaylar oluşma sırasıyla iletilsin.
	slices.SortFunc(events, func(a, b Event) int { return cmp.Compare(a.ID, b.ID) })
	return events, nil
}

// deliver olayı abonelere iletir ve sonucu kaydeder; başarılıysa true döner.
func (p *Publisher) deliver(ctx context.Context, e Event) bool {
	var errs []error
	for _, c := range p.consumers[e.Type] {
		if err := c.Handle(ctx, e); err != nil {
			p.failures.WithLabelValues(c.Name()).Inc()
			errs = append(errs, fmt.Errorf("%s: %w", c.Name(), err))
		}
	}
	// Sonucu kaydetmek isteğin bağlamından bağımsızdır: kapanış sırasında
	// yarım kalan teslim, kiralama dolunca yeniden denenir.
	ctx = context.WithoutCancel(ctx)
	if err := errors.Join(errs...); err != nil {
		attempt := e.Attempts + 1
		dead := attempt >= p.maxAttempts
		_, dbErr := p.pool.Exec(ctx, `
			UPDATE outbox_events
			SET attempts = $2, next_attempt_at = now() + $3::interval, last_error = $4, lease_until = NULL, dead = $5
			WHERE id = $1`, e.ID, attempt, Backoff(attempt).String(), truncate(err.Error(), 2000), dead)
		if dead {
			p.dead.Inc()
			p.log.Error("outbox olayı bırakıldı", "event_id", e.ID, "type", e.Type, "attempts", attempt, "error", err)
		} else {
			p.log.Warn("outbox teslimi başarısız, yeniden denenecek",
				"event_id", e.ID, "type", e.Type, "attempt", attempt, "retry_in", Backoff(attempt).String(), "error", err)
		}
		if dbErr != nil {
			p.log.Error("outbox durumu yazılamadı", "event_id", e.ID, "error", dbErr)
		}
		return false
	}
	if _, err := p.pool.Exec(ctx, `
		UPDATE outbox_events SET published_at = now(), lease_until = NULL, last_error = NULL WHERE id = $1`, e.ID); err != nil {
		// Teslim edildi ama işaretlenemedi: olay yeniden iletilir, tüketiciler idempotent.
		p.log.Error("outbox olayı işaretlenemedi", "event_id", e.ID, "error", err)
		return false
	}
	p.published.Inc()
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
