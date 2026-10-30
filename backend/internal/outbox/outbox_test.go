package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"carsihub/internal/platform/dbtest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeConsumer struct {
	name  string
	fail  atomic.Int64 // bu kadar çağrı başarısız olsun
	calls atomic.Int64
	seen  sync.Map
}

func (f *fakeConsumer) Name() string { return f.name }

func (f *fakeConsumer) Handle(_ context.Context, e Event) error {
	f.calls.Add(1)
	if f.fail.Load() > 0 {
		f.fail.Add(-1)
		return errors.New("bağımlılık yok")
	}
	f.seen.Store(e.ID, true)
	return nil
}

var aggSeq atomic.Int64

// write her olayı ayrı bir kümeye yazar; küme sırasını sınayan testler writeFor kullanır.
func write(t *testing.T, pool *pgxpool.Pool, typ string, commit bool) {
	t.Helper()
	writeFor(t, pool, typ, fmt.Sprintf("o%d", aggSeq.Add(1)), commit)
}

func writeFor(t *testing.T, pool *pgxpool.Pool, typ, aggregate string, commit bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(ctx, tx, typ, "order", aggregate, map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if commit {
		_ = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWrite_IsAtomicWithTransaction(t *testing.T) {
	pool := dbtest.New(t)
	write(t, pool, "x.rolled_back", false)
	write(t, pool, "x.committed", true)
	if n := count(t, pool, "true"); n != 1 {
		t.Fatalf("yalnızca commit edilen olay kalmalı: %d", n)
	}
}

func TestPublisher_DeliversAndRetriesWithBackoff(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	c := &fakeConsumer{name: "c"}
	c.fail.Store(2)
	p := NewPublisher(pool, discard, 10, 10)
	p.Subscribe("order.placed", c)
	write(t, pool, "order.placed", true)
	write(t, pool, "order.unrelated", true) // abonesi yok → doğrudan yayımlandı

	if _, err := p.PublishOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, "published_at IS NULL") != 1 || count(t, pool, "attempts = 1 AND next_attempt_at > now()") != 1 {
		t.Fatal("başarısız olay ertelenmeli, abonesiz olay yayımlanmalı")
	}
	// Bekleme süresi dolmadan tekrar denenmez.
	if n, _ := p.PublishOnce(ctx); n != 0 {
		t.Fatal("bekleme süresi dolmadan olay alınmamalı")
	}
	for range 2 {
		if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now()`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.PublishOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, pool, "published_at IS NULL") != 0 || c.calls.Load() != 3 {
		t.Fatalf("üçüncü denemede teslim edilmeli: calls=%d", c.calls.Load())
	}
}

func TestPublisher_GivesUpAfterMaxAttempts(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	c := &fakeConsumer{name: "c"}
	c.fail.Store(100)
	p := NewPublisher(pool, discard, 10, 3)
	p.Subscribe("e", c)
	write(t, pool, "e", true)
	for range 5 {
		_, _ = pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now()`)
		_, _ = p.PublishOnce(ctx)
	}
	if c.calls.Load() != 3 || count(t, pool, "dead AND last_error IS NOT NULL") != 1 {
		t.Fatalf("3 denemeden sonra bırakılmalı: calls=%d", c.calls.Load())
	}
}

// İki worker aynı anda çalışsa da her olay tam bir kez teslim edilir (kiralama + SKIP LOCKED).
func TestPublisher_ConcurrentWorkersDoNotDoubleDeliver(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	c := &fakeConsumer{name: "c"}
	const events = 200
	for range events {
		write(t, pool, "e", true)
	}
	var wg sync.WaitGroup
	for range 4 {
		p := NewPublisher(pool, discard, 7, 10)
		p.Subscribe("e", c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := p.PublishOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if c.calls.Load() != events || count(t, pool, "published_at IS NOT NULL") != events {
		t.Fatalf("teslim=%d, beklenen %d", c.calls.Load(), events)
	}
}

// Worker olayı kiralayıp çökerse, kiralama süresi dolunca olay başka worker'a geçer.
func TestPublisher_ExpiredLeaseIsReclaimed(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	write(t, pool, "e", true)
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET lease_until = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	c := &fakeConsumer{name: "c"}
	p := NewPublisher(pool, discard, 10, 10)
	p.Subscribe("e", c)
	if n, _ := p.PublishOnce(ctx); n != 0 {
		t.Fatal("başkası kiraladıysa alınmamalı")
	}
	_, _ = pool.Exec(ctx, `UPDATE outbox_events SET lease_until = now() - interval '1 second'`)
	if n, _ := p.PublishOnce(ctx); n != 1 || c.calls.Load() != 1 {
		t.Fatal("süresi dolan kiralama devralınmalı")
	}
}

func TestOnce_RunsEffectExactlyOnce(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	write(t, pool, "e", true)
	var id int64
	_ = pool.QueryRow(ctx, `SELECT id FROM outbox_events`).Scan(&id)
	if _, err := pool.Exec(ctx, `CREATE TABLE effects (n int)`); err != nil {
		t.Fatal(err)
	}
	effect := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO effects VALUES (1)`)
		return err
	}

	// Hata: etki ve işlendi kaydı birlikte geri alınır; sonraki deneme çalışır.
	if _, err := Once(ctx, pool, "c", id, func(tx pgx.Tx) error { _ = effect(tx); return errors.New("x") }); err == nil {
		t.Fatal("hata dönmeli")
	}
	// Aynı olay eşzamanlı 10 kez gelse de etki bir kez.
	var wg sync.WaitGroup
	var ran atomic.Int64
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := Once(ctx, pool, "c", id, effect); err == nil && ok {
				ran.Add(1)
			}
		}()
	}
	wg.Wait()
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM effects`).Scan(&n)
	if ran.Load() != 1 || n != 1 {
		t.Fatalf("etki bir kez olmalı: ran=%d effects=%d", ran.Load(), n)
	}
	// Başka tüketici aynı olayı ayrıca işleyebilir.
	if ok, _ := Once(ctx, pool, "baska", id, effect); !ok {
		t.Fatal("tüketiciler bağımsız olmalı")
	}
}

func TestBackoff(t *testing.T) {
	want := map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 50: 5 * time.Minute}
	for n, d := range want {
		if got := Backoff(n); got != d {
			t.Errorf("Backoff(%d)=%v, beklenen %v", n, got, d)
		}
	}
}

func TestRun_StopsWithContext(t *testing.T) {
	pool := dbtest.New(t)
	c := &fakeConsumer{name: "c"}
	p := NewPublisher(pool, discard, 10, 10)
	p.Subscribe("e", c)
	write(t, pool, "e", true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p.Run(ctx, 20*time.Millisecond)
	if c.calls.Load() != 1 || len(p.Collectors()) != 3 {
		t.Fatal("Run olayı iletmeli")
	}
}

// Aynı kümenin olayları sırayla iletilir: önceki olay başarısızsa sonraki beklemede kalır.
func TestPublisher_KeepsOrderWithinAggregate(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	first := &fakeConsumer{name: "ilk"}
	first.fail.Store(1)
	second := &fakeConsumer{name: "ikinci"}
	p := NewPublisher(pool, discard, 10, 10)
	p.Subscribe("order.placed", first)
	p.Subscribe("order.cancelled", second)
	writeFor(t, pool, "order.placed", "sira", true)
	writeFor(t, pool, "order.cancelled", "sira", true)

	_, _ = p.PublishOnce(ctx)
	_, _ = p.PublishOnce(ctx)
	if second.calls.Load() != 0 {
		t.Fatal("önceki olay yayımlanmadan sonraki iletilmemeli")
	}
	_, _ = pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now()`)
	_, _ = p.PublishOnce(ctx)
	_, _ = p.PublishOnce(ctx)
	if first.calls.Load() != 2 || second.calls.Load() != 1 {
		t.Fatalf("sıra korunarak ikisi de iletilmeli: ilk=%d ikinci=%d", first.calls.Load(), second.calls.Load())
	}
}

type slowConsumer struct{ d time.Duration }

func (slowConsumer) Name() string { return "yavas" }

func (s slowConsumer) Handle(ctx context.Context, e Event) error {
	var p struct{ Yavas bool }
	if json.Unmarshal(e.Payload, &p) == nil && p.Yavas {
		time.Sleep(s.d)
		return errors.New("zaman aşımı")
	}
	return nil
}

// Yavaş bir bağımlılık (her deneme zaman aşımına kadar sürer) diğer kümeleri tıkamaz.
func TestPublisher_SlowAggregateDoesNotBlockOthers(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	p := NewPublisher(pool, discard, 50, 10)
	p.Subscribe("e", slowConsumer{d: 300 * time.Millisecond})
	for i := range 20 {
		tx, _ := pool.Begin(ctx)
		payload := map[string]bool{"yavas": i < 10}
		if err := Write(ctx, tx, "e", "order", fmt.Sprintf("k%d", i), payload); err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit(ctx)
	}
	start := time.Now()
	if _, err := p.PublishOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("10 yavaş olay sırayla işlenmiş gibi: %v", took)
	}
	if n := count(t, pool, "published_at IS NOT NULL"); n != 10 {
		t.Fatalf("hızlı 10 olay yayımlanmalı: %d", n)
	}
}
