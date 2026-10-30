# ÇarşıHub — Çok Satıcılı Pazaryeri Sipariş ve Ödeme Sistemi

**Geliştiren:** Zehre İnce · DDO Yazılım Ekibi alım projesi

Satıcıların ürün sattığı bir pazaryerinin sepet, stok, çok satıcılı sipariş bölme, ödeme, komisyon ve iade süreçlerini yöneten sistem.

> **Durum:** F-1, F-2, F-3, F-4, F-6 ve zorlu görevlerden **T-1**, **T-2**, **T-3** tamam ve `make verify` ile kanıtlı (29/29). **T-4** kısmen: çift taraflı defter, zaman geçerlilikli komisyon ve teslimde hakediş tahakkuku çalışıyor ve `make verify` ile kanıtlı (30/30); iade düzeltmesinin deftere bağlanması, dönem kapanışı ve fark mutabakatı yapılmadı. **F-5** kısmen (satıcı hakediş kalemlerini ve alacak bakiyesini görüyor). **T-5**, **F-7**'nin iade işlemi ve arayüzün katalog/sepet ekranları yapılmadı. Ayrıntı ve nedenleri: [Yapılmayanlar](#yapılmayanlar-ve-bilinen-eksikler).

## Kurulum

Gereksinim: Docker + Docker Compose.

```bash
git clone https://github.com/zehreince/ddo-case-zehre-ince.git && cd ddo-case-zehre-ince
cp .env.example .env
docker compose up --build
```

| Servis | Adres |
|---|---|
| Frontend | http://localhost:3000 |
| API | http://localhost:8080 |
| Sağlık | http://localhost:8080/health |
| Metrikler | http://localhost:8080/metrics |

Veritabanı host'a açılmaz, yalnızca compose ağı içinden erişilir.

### Demo hesapları

Seed her açılışta otomatik yüklenir. Hepsinin parolası **`Demo123!`**; giriş ekranındaki "Demo hesapları" düğmeleri formu doldurur.

| Rol | E-posta |
|---|---|
| Müşteri | `musteri@carsihub.test` |
| Satıcı | `satici@carsihub.test` |
| Pazaryeri operasyon | `operasyon@carsihub.test` |
| Finans | `finans@carsihub.test` |
| Kargo entegrasyonu | `kargo@carsihub.test` |
| Yönetici | `yonetici@carsihub.test` |

### Sık kullanılan komutlar

| Komut | Ne yapar |
|---|---|
| `make setup` | Git hook'larını açar: commit'te gofmt/vet, push'ta tüm testler |
| `make up` | Derler, ayağa kaldırır, sağlıklı olana kadar bekler |
| `make verify` | Tüm kontrolleri uçtan uca çalıştırır, PASS/FAIL tablosu basar |
| `make chaos` | Ödeme sağlayıcıyı ve worker'ı durdurup sipariş akışının toparlandığını gösterir (T-2) |
| `make reset-demo` | Veritabanını silip ilk seed haline döndürür |
| `make test` | Backend + frontend birim testleri |
| `make lint` | go vet, golangci-lint, TypeScript kontrolü |

## Mimari

```
                                         ┌──────────── callback (HMAC) ─────────────┐
                                         ▼                                          │
frontend (React :3000) ──► api (Go :8080) ──► PostgreSQL 16 ◄── worker (Go :8081)   │
                                                 ▲   outbox      │ ödeme iste/iade, │
                         migrate (bir kez:       │   olayları    │ kargo oluştur    │
                         şema + demo seed) ──────┘               ▼                  │
                                                         simulator (Go :8090) ──────┘
                                                         ödeme sağlayıcı + kargo firması
```

- **Backend:** Go, tek modül. Alan bazlı paketler (`internal/catalog`, `internal/order` …); her paketin içinde `domain → service → store → handler` katmanları. Ayrıntı: [DECISIONS.md](DECISIONS.md).
- **Ortak altyapı:** `internal/platform` — config, JSON log, hata biçimi, middleware, veritabanı, metrikler.
- **Komutlar:** `cmd/api`, `cmd/migrate`, `cmd/worker`, `cmd/simulator`, `cmd/verify`. Hepsi tek imajda; compose hangisinin çalışacağını seçer.
- **Saga (T-2):** Durum değişikliği ve olayı aynı transaction'da outbox'a yazılır; worker en az bir kez iletir, tüketiciler idempotenttir. Ret, zaman aşımı, çift callback ve geç ödeme için telafiler var. Ayrıntı: [ADR-008](DECISIONS.md#adr-008-saga--outbox-ile-olay-güdümlü-orkestrasyon-durum-makinesiyle-telafi). Kanıt: `make chaos`.
- **Kupon (T-3):** Saf fiyat motoru (`internal/pricing`) sipariş, sepet ve iadede aynı. İndirim satıcılara en büyük kalan yöntemiyle dağıtılır, kuruş kayması yok. Kısmi iade yeniden fiyatlamayla, min. sepet altına düşünce kupon geri alınır. Ayrıntı: [ADR-009](DECISIONS.md#adr-009-kupon-ve-fiyat-motoru--saf-hesap-en-büyük-kalan-yöntemi-yeniden-fiyatlamayla-iade).
- **Stok (T-1):** Oversell tek bir koşullu `UPDATE` ile engellenir, sistem genelinde sabit kilit sırası deadlock'u önler. Ayrıntı ve elenen alternatifler: [ADR-007](DECISIONS.md#adr-007-stok-rezervasyonu--koşullu-update-ayrı-rezervasyon-tablosu-global-kilit-sırası).
- **Frontend:** React + Vite + TypeScript.

### Frontend seçimi ve gerekçesi

React + Vite + TypeScript. Rol bazlı birden fazla panel (müşteri, satıcı, finans, operasyon) olacağı için bileşen yapısı ve tip güvenliği işe yarıyor. Vite derlemesi hızlı, çıktı nginx ile statik olarak sunulabiliyor. HTMX daha sade olurdu ama sepet ve kupon hesabı gibi istemci tarafı durumu olan ekranlarda React daha rahat.

## Yapılmayanlar ve bilinen eksikler

Dürüst durum; bir şeyi yapılmış gibi göstermek yerine neyin eksik olduğunu yazıyorum.

| Kimlik | Durum | Not |
|---|---|---|
| T-4 hakediş defteri | **Kısmen** | **Çalışan:** çift taraflı defter (borç=alacak commit anında veritabanında zorlanıyor, append-only, idempotent); `order.paid` → ödeme kaydı; `shipment.delivered` → hakediş kalemi (net = brüt − komisyon − kargo, vade = teslim + `PAYOUT_DELAY_DAYS`) ve tahakkuk kaydı; kategori bazlı, zaman geçerlilikli komisyon (sipariş anındaki oran; çakışan aralık `btree_gist` ile engelli); `GET /finance/ledger/balances` (mizan), `GET /seller/settlements`. **Eksik:** iade düzeltmesinin (kurucu ve şema hazır) olaylara bağlanması, dönem kapanışı/ödeme ve fark mutabakatı uçları. Tasarım: [ADR-010](DECISIONS.md#adr-010-çift-taraflı-defter--veritabanında-zorlanan-denge-append-only-idempotent-kayıt). |
| F-5 satıcı hakedişi | Kısmen | Satıcı hakediş kalemlerini (brüt, komisyon, kargo, net, vade) ve defterdeki alacak bakiyesini görüyor; ödeme (dönem kapanışı) yok. |
| F-7 iade | Kısmen | İade **tutarı** hesaplanıyor (`POST /orders/{id}/refund-quote`, kupon geri hesabıyla, T-3). İadenin kendisi (para, stok, defter düzeltmesi) ve raporlar yok. Ödeme öncesi iptal ve geç ödemede otomatik iade var. |
| T-5 arama | Yapılmadı | `pg_trgm` ve `unaccent` eklentileri kurulu; 1M ürün üretimi, arama ucu, `EXPLAIN` ve p95 ölçümü yok. |
| Yük testi (`make load`) | Yapılmadı | Eşzamanlılık kanıtları (1.000 eşzamanlı sipariş) var ama k6 p50/p95/p99 tablosu yok. |
| Arayüz | Kısmen | Giriş, sessiz oturum yenileme, rol bazlı menü, yönetici kullanıcı listesi, sistem durumu. Katalog, sepet, sipariş takibi ve satıcı/operasyon ekranları yok; bu akışlar API (`requests.http`) ile denenebiliyor. |

**Önceliklendirme:** Süre kısıtında zorlu görevlerin doğruluğunu (T-1, T-2, T-3) yüzeysel ama geniş kapsamın önüne koydum. Her biri gerçek PostgreSQL üzerinde eşzamanlılık testleriyle, çalışan sistemde `make verify` ile ve T-2'de `make chaos` ile kanıtlı.

## Doğrulama Soruları

### 1. En zorlandığın görev hangisiydi? Hangi alternatifleri düşündün, neden eledin?

**T-2 (saga + outbox).** Zorluk tek bir algoritmada değil, hataların birbiriyle yarışmasındaydı: callback ile mutabakat aynı anda gelebiliyor, müşteri ödemesi sağlayıcıdayken iptal edebiliyor, worker olayı kiralamışken ölebiliyor.

- **Broker (Kafka/RabbitMQ) yerine PostgreSQL outbox:** Broker'a yazmak yine "DB'ye yazdım ama mesaj gitmedi" sorununu doğuruyordu, yani yine outbox gerekiyordu. Ek altyapı değer katmıyordu.
- **Koreografi yerine tek kapı (`ApplyResult`):** Telafi kararları (ret → stok iadesi, kapalı siparişe ödeme → iade) tek bir fonksiyonda, sipariş → ödeme kilidi altında. Callback, mutabakat ve tekrarlar aynı yoldan geçtiği için 10 eşzamanlı callback tek etki üretiyor.
- **En çok öğreten an:** İlk sürümde yayımlayıcı olayları sırayla işliyordu. `make chaos` ile sağlayıcıyı durdurunca her deneme 3–5 sn sürdü ve bütün kuyruk tıkandı (satıcı bildirimi ve kargo da bekledi). Olayları paralel işleyecek şekilde değiştirdim; aynı siparişin olayları ise kendi içinde sıralı kalıyor (`claim` sorgusunda `NOT EXISTS`). Bu sorunu bir birim testi değil, kaos testi buldu.

### 2. Çözümünün kırıldığı ya da yetersiz kaldığı bir senaryo

**Tek bir çok satan ürün (sıcak satır).** Oversell, tek bir `offers` satırındaki koşullu `UPDATE` ile önleniyor. Bu doğru ama aynı ürüne gelen bütün siparişler o satırın kilidinde sıraya giriyor: 1.000 eşzamanlı sipariş ~2,5 sn sürüyor. Kampanya anında tek bir ürüne binlerce istek gelirse gecikme doğrusal artar. Çözüm, stoğu satıcı deposu ya da sanal parça bazında birden çok satıra bölmek olurdu. Bunu yapmadım, çünkü doğruluk testleri tek satırla daha açık.

İkincisi: **Idempotency yanıtı işlemle aynı transaction'da değil.** İşlem commit olup yanıt kaydedilemeden süreç çökerse, aynı anahtarla gelen tekrar istek işlemi yeniden yapar. Pencere milisaniyeler ama sıfır değil (ADR-007).

### 3. İki dizin: hangi sorgu için, ölçüm farkı

- **`offers_variant_idx` / `offers (id)` birincil anahtarı:** Stok rezervasyonundaki `UPDATE … WHERE id = $1` birincil anahtarla tek satır bulur; kilit süresi kısa kalır.
- **`orders_pending_deadline_idx` (kısmi: `WHERE status = 'pending_payment'`):** Süpürücünün "ödeme süresi dolan siparişler" sorgusu yalnızca bekleyen siparişleri tarar. Ödenmiş/kapanmış milyonlarca sipariş dizine girmez.
- **`outbox_pending_idx` (kısmi: yayımlanmamış ve dead olmayan):** Outbox tablosu sürekli büyür ama yayımlayıcı yalnızca bekleyenleri tarar.

**Dürüst not:** Bu dizinler için dizinli/dizinsiz `EXPLAIN ANALYZE` ölçümü yapmadım. Ölçüm, 1M ürünlük veriyle T-5'in parçası olarak planlanmıştı ve T-5 yapılmadı. Yukarıdakiler tasarım gerekçesi, ölçülmüş sonuç değil.

### 4. Yapay zekâ kullanımı: reddettiğim ya da düzelttiğim bir çıktı

Kodun büyük kısmını Claude (Claude Code) ile birlikte yazdım. Düzelttiğim örneklerden biri, **süresi dolan sepet rezervasyonlarını bırakan süpürücü:**

**Önce** (ilk öneri):
```sql
SELECT id FROM stock_reservations
WHERE status = 'active' AND order_id IS NULL AND expires_at <= $1
LIMIT $2 FOR UPDATE SKIP LOCKED   -- önce rezervasyon kilitleniyor
-- ardından Release → offers satırları kilitleniyor
```
**Neden yanlış:** Sepetten ürün çıkaran ya da ödeme yapan istek önce `offers`, sonra `stock_reservations` kilitliyor. Süpürücü tersini yapıyordu. Aynı anda çalışırlarsa birbirini sonsuza kadar bekler (deadlock).

**Sonra:**
```sql
SELECT id FROM stock_reservations
WHERE status = 'active' AND order_id IS NULL AND expires_at <= $1
LIMIT $2                          -- kilitsiz aday seçimi
-- Release: önce offers (id sırasıyla), sonra rezervasyon, durum kilit altında yeniden kontrol
```
Sistem genelinde tek kilit sırası tanımladım: `orders → offers → stock_reservations`. Ters sırada kilit isteyen 100 eşzamanlı işlemin deadlock'a girmediği testle kanıtlı (`TestReserveBatch_OppositeOrderDoesNotDeadlock`).

Diğer düzelttiklerim:
- Alt sorgulu `RETURNING (SELECT …)` kalıbını açık `SELECT … FOR UPDATE` ile değiştirdim (hangi anın değerini okuduğu belirsizdi).
- Bir iade testinde hiçbir şey kontrol etmeyen bir `if` bloğunu sildim.
- Saga eklendikten sonra yarışa giren bir `verify` kontrolünü (T-1a) deterministik hâle getirdim.

### 5. "Stok rezervasyonu ve oversell önleme" çözümünün doğru çalıştığını nasıl kanıtladın?

Üç katmanda, hepsi gerçek PostgreSQL 16 üzerinde (mock yok):

**a) Stok seviyesinde** — aynı teklife (stok 50) 1.000 goroutine aynı anda rezervasyon ister:

```bash
cd backend && go test ./internal/inventory -run ThousandConcurrent -v
```
```
inventory_test.go:67: istek=1000 başarılı=50 stok-yetersiz=950 | on_hand=50 reserved=50
--- PASS: TestReserve_ThousandConcurrentRequestsNeverOversell (5.72s)
```
Test ayrıca `offers.stock_reserved` ile aktif rezervasyonların toplamının eşit olduğunu doğrular.

**b) Sipariş seviyesinde** — stok 100, 1.000 eşzamanlı sipariş:

```bash
cd backend && go test ./internal/order -run ThousandConcurrentOrders -v
```
```
service_test.go:147: sipariş isteği=1000 oluşan=100 reddedilen=900 | sipariş tablosu=100 kalem toplamı=100 reserved=100
--- PASS: TestPlace_ThousandConcurrentOrdersNeverOversell (8.36s)
```

**c) Çalışan sistemde HTTP üzerinden** — `make verify` içindeki T-1a, stok 50 olan teklife 1.000 paralel `POST /api/v1/orders` atar, ardından siparişleri iptal edip stoğun tamamen döndüğünü kontrol eder:

```
  T-1a: 201=50 409=950 diğer=0 reserved=50/50 süre=2.57s
T-1a     1.000 eşzamanlı sipariş: stoktan fazlası satılmaz                      PASS   3.556s
```

Bunlara ek olarak: ters sırada kilit isteyen 100 eşzamanlı işlemin deadlock'a girmediği, doğrudan SQL ile eldekinden fazla rezervasyon yazılamadığı (`CHECK` kısıtı), TTL dolunca ve ödeme başarısız olunca stoğun iade edildiği ayrı testlerle gösterildi. Komutların tamamı: [TESTING.md → Kanıt komutları](TESTING.md#kanıt-komutları).
