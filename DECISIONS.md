# Mimari Karar Kayıtları

Her kayıt: bağlam, karar, düşünülen alternatifler, sonuçları.

---

## ADR-001: Modüler monolit, mikroservis değil

**Bağlam.** Sipariş → stok → ödeme → kargo akışı tutarlı kalmalı. Ekip tek kişi, süre sınırlı.

**Karar.** Tek Go servisi (api) + arka plan işçisi; alanlar `internal/` altında ayrı paketler. Paketler arası çağrı yalnızca dışa açık service fonksiyonları üzerinden.

**Alternatifler.**
- *Servis başına ayrı süreç (order-service, payment-service …):* Dağıtık transaction, ağ hataları ve ayrı veritabanları getirir. Bu projede saga'yı göstermek için zaten simüle edilmiş dış bağımlılıklar (ödeme sağlayıcı, kargo) var; kendi servislerimizi de bölmek değer katmadan karmaşıklık ekler.

**Sonuç.** Sipariş + stok + outbox tek veritabanı transaction'ında yazılabiliyor. Bölünme gerekirse paket sınırları zaten hazır.

---

## ADR-002: Katmanlara göre değil, alana göre paketleme

**Bağlam.** C# projelerinde alışılmış `Api / Application / Domain / Infrastructure` ayrımı Go'da çok sayıda boş ara katman ve döngüsel import riski doğuruyor.

**Karar.** Her alan bir paket (`order`, `inventory`, `ledger` …). Paket içinde dosya düzeyinde katman: `domain.go` (saf kurallar), `service.go` (use-case, transaction sınırı), `store.go` (SQL), `handler.go` (HTTP). Fiyat ve komisyon hesabı gibi saf mantık veritabanı ve HTTP import etmez.

**Alternatifler.** Klasik katmanlı yapı (`handlers/`, `services/`, `repositories/`): bir özelliği değiştirmek için dört klasörü gezmek gerekiyor ve iş kuralları kolayca handler'a sızıyor.

**Sonuç.** Saf paketler birim testle yüksek kapsama ulaşır; veritabanı gerektiren kısımlar gerçek PostgreSQL ile entegrasyon testine bırakılır.

---

## ADR-003: pgx + elle yazılmış SQL, ORM yok

**Bağlam.** Stok için koşullu `UPDATE`, `FOR UPDATE SKIP LOCKED`, full-text arama ve `EXPLAIN` çıktısı isteniyor.

**Karar.** `pgx/v5` havuzu ve açık SQL.

**Alternatifler.** GORM vb.: kilitleme ve izolasyon seviyesi gibi kritik ayrıntıları gizliyor, üretilen sorguyu `EXPLAIN` için ayrıca yakalamak gerekiyor.

**Sonuç.** Daha fazla SQL yazılıyor ama her kritik sorgu okunabilir ve ölçülebilir.

---

## ADR-004: goose ile gömülü migrasyonlar, ayrı migrate servisi

**Bağlam.** Migrasyon ve seed elle adım gerektirmemeli.

**Karar.** Migrasyonlar `go:embed` ile ikili dosyaya gömülü. Compose'da `migrate` servisi bir kez çalışıp çıkar; `api` ona `service_completed_successfully` ile bağlı.

**Alternatifler.** API açılışında migrasyon çalıştırmak: birden fazla API kopyası aynı anda migrasyon denemesi yapar, yavaş bir migrasyon healthcheck'i düşürür.

**Sonuç.** Migrasyon hatası API'yi hiç başlatmaz; sorun compose çıktısında net görünür.

---

## ADR-005: Tek imaj, distroless, root olmayan kullanıcı

**Bağlam.** Çok aşamalı derleme, küçük imaj ve root olmayan kullanıcı isteniyor.

**Karar.** Tüm Go komutları tek imaja derlenir (`gcr.io/distroless/static-debian12:nonroot`). Kabuk olmadığı için sağlık kontrolü `api healthcheck` alt komutu ile yapılır.

**Alternatifler.** Komut başına ayrı imaj: aynı bağımlılıklar birkaç kez derlenir, soğuk makinede açılış süresi uzar. Alpine + curl: daha büyük imaj, gereksiz saldırı yüzeyi.

**Sonuç.** Tek derleme, küçük imaj; yeni bir komut eklemek Dockerfile değişikliği gerektirmez.

---

## ADR-006: Kısa ömürlü JWT + tek kullanımlık, aile bazlı refresh token

**Bağlam.** Her uçta rol kontrolü gerekiyor; oturum hem tarayıcıdan hem `verify`/`requests.http` gibi API istemcilerinden açılıyor. Çalınan bir token'ın etkisi sınırlı olmalı.

**Karar.**
- **Access token:** HS256 JWT, 15 dk. Durumsuz; her istekte veritabanına gitmeden rol okunur. Yalnızca HS256 kabul edilir (`alg=none` ve algoritma değiştirme saldırıları reddedilir).
- **Refresh token:** 32 bayt rastgele, opak. Veritabanında yalnızca SHA-256 özeti tutulur. Her kullanımda yenisiyle değiştirilir (rotation). Aynı oturumdan türeyen token'lar bir `family_id` paylaşır; **iptal edilmiş bir token tekrar gelirse aile tamamen iptal edilir** ve olay audit'e yazılır.
- **Yarış:** Aynı refresh token'la eşzamanlı istekler `SELECT … FOR UPDATE` ile sıraya girer; yalnızca biri döndürme yapar. Testle kanıtlı (10 eşzamanlı istek → tam 1 başarı).
- **Tarayıcı:** Access token yalnızca bellekte. Refresh token `httpOnly; SameSite=Strict; Path=/api/v1/auth` çerezde, JavaScript okuyamaz. Sayfa yenilenince oturum çerezle sessizce geri gelir. API istemcileri aynı token'ı gövdeden gönderebilir.
- **Kullanıcı sayımı:** Bilinmeyen e-posta, yanlış parola ve kapalı hesap aynı 401'i döner; bilinmeyen e-postada da bcrypt karşılaştırması yapılır ki yanıt süresi bir şey sızdırmasın.

**Alternatifler.**
- *Sunucu taraflı oturum (session tablosu):* Her istekte veritabanı okuması gerekir; anında iptal kolaylaşır. Kampanya anı yükünde (Bölüm 8) her isteğe bir sorgu eklememek için reddedildi.
- *Uzun ömürlü tek JWT:* İptal edilemez; çalınırsa süresi dolana kadar geçerli.
- *Refresh token'ı localStorage'da tutmak:* XSS ile okunabilir.

**Bilinen sınırlama.** Rol değişikliği ya da hesap kapatma, access token'ın süresi dolana kadar (en fazla 15 dk) yansımaz; refresh anında yansır. Giriş denemelerine hız sınırı yok; üretimde IP + hesap bazlı sınırlama eklenmeli.
