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

---

## ADR-007: Stok rezervasyonu — koşullu UPDATE, ayrı rezervasyon tablosu, global kilit sırası

**Bağlam.** Aynı varyanta 1.000 eşzamanlı sipariş stoktan fazla satılamamalı (T-1). Sepet stok tutmalı (TTL), ödeme başarısızsa stok dönmeli, çok satıcılı sepette kısmi satış kuralı açık olmalı.

**Karar.**
- **Stok modeli:** `offers` satırında `stock_on_hand` ve `stock_reserved`. Satılabilir = fark. Her tutma `stock_reservations` tablosunda ayrı bir satır (`active → committed | released | expired`); `stock_reserved` her zaman aktif rezervasyonların toplamıdır (testlerde ayrıca kontrol ediliyor).
- **Oversell önleme:** Tek koşullu `UPDATE offers SET stock_reserved = stock_reserved + q WHERE id = $1 AND stock_on_hand - stock_reserved >= q`. PostgreSQL bekleyen işlemin `WHERE` koşulunu kilidi aldıktan sonra güncel satırda yeniden değerlendirir; READ COMMITTED yeterli. Son savunma hattı olarak `CHECK (stock_reserved <= stock_on_hand)`.
- **Kilit sırası:** Sistem genelinde `orders → offers (id sırasıyla) → stock_reservations`. Birden çok teklife dokunan her işlem önce `LockOffers` ile teklifleri sıralı kilitler. Testle kanıtlı: aynı iki ürünü ters sırada isteyen 100 eşzamanlı işlem deadlock'a girmiyor.
- **TTL:** Sepet rezervasyonu `RESERVATION_TTL`, siparişinki `ORDER_PAYMENT_TTL`. Ayrı `worker` servisi süresi dolanları partiler halinde bırakır. Ödenmemiş siparişler `FOR UPDATE SKIP LOCKED` ile seçilir (o anda ödeme sonucu işlenen sipariş beklenmez).
- **Sepetten ödemeye:** Sepet rezervasyonları bırakılır ve aynı transaction içinde sipariş için yeniden alınır; teklif kilitleri baştan alındığı için arada stok kaybedilmez. Hata olursa her şey geri alınır, sepet olduğu gibi kalır.
- **Kısmi satış (`PARTIAL_FULFILLMENT`):** `allow` (varsayılan) → stoğu olan kalemler satılır, olmayanlar `unavailable_items` olarak döner ve sepette kalır; hiçbiri yoksa sipariş oluşmaz. `reject` → tek kalem bile yetmezse hiçbir şey rezerve edilmez. Her kalem kendi savepoint'inde denenir; böylece tüm başarısız kalemler tek seferde raporlanır.
- **Idempotency:** Sipariş, sepete ekleme ve ödeme adımı `Idempotency-Key` destekler; anahtar kullanıcı başına, istek özeti (yöntem + yol + gövde) ile saklanır.

**Alternatifler.**
- *`SELECT … FOR UPDATE` + uygulamada kontrol + `UPDATE`:* Doğru ama iki gidiş-dönüş; koşullu `UPDATE` aynı garantiyi tek ifadede verir.
- *`SERIALIZABLE` + yeniden deneme:* Sıcak üründe (kampanya anı) çakışma oranı çok yüksek olur, yeniden denemeler yükü katlar.
- *Redis'te sayaç (DECR):* Hızlı ama stok ile sipariş iki ayrı sistemde olur; tutarlılık için ayrıca telafi gerekir. Ek bağımlılık getirmeye değmedi.
- *Stoğu doğrudan düşmek (rezervasyon tablosu olmadan):* TTL, iade ve "hangi sipariş neyi tutuyor" sorusu cevapsız kalır.
- *Süpürücüde rezervasyonları önce `FOR UPDATE SKIP LOCKED` ile kilitlemek:* İlk tasarım buydu; sepet işlemleriyle ters sırada kilit almak demekti (rezervasyon → teklif vs. teklif → rezervasyon) ve deadlock'a açıktı. Adaylar kilitsiz seçilip global sırayla kilitleniyor, durum kilit altında yeniden kontrol ediliyor.

**Bilinen sınırlamalar.**
- Tek bir çok satan ürün tek bir satıra yazıldığı için o satır seri hale gelir (ölçüm: 1.000 eşzamanlı sipariş ~2,5 sn). Gerekirse stok satıcı deposu başına parçalara bölünebilir.
- Idempotency yanıtı işlem transaction'ından ayrı kaydedilir. İşlem commit olup yanıt kaydedilemeden süreç çökerse aynı anahtarla tekrar istek işlemi yeniden yapar. Pencere çok küçük; tam çözüm yanıtın işlemle aynı transaction'da yazılmasıdır.

---

## ADR-008: Saga — outbox ile olay güdümlü orkestrasyon, durum makinesiyle telafi

**Bağlam.** Sipariş → stok → ödeme → satıcı bildirimi → kargo adımlarının her biri başarısız olabilir: sağlayıcı kapalı, ödeme zaman aşımı, çift callback, geç gelen sonuç, worker çökmesi. Hiçbir adım kaybolmamalı ya da iki kez etki etmemeli.

**Karar.**
- **Transactional outbox:** Her durum değişikliği, olayını aynı transaction'da `outbox_events`'e yazar. Broker yok; worker olayları veritabanından kiralar (`FOR UPDATE SKIP LOCKED` + `lease_until`) ve abone tüketicilere iletir. Başarısızlıkta üstel bekleme (1 sn → 5 dk), `OUTBOX_MAX_ATTEMPTS` sonra `dead` (operasyon `POST /ops/outbox/{id}/retry` ile geri alır).
- **En az bir kez + idempotent tüketici:** Yerel etkili tüketiciler `outbox.Once` ile `(tüketici, olay)` kaydını kendi değişikliğiyle aynı transaction'da atar. Dış çağrı yapan tüketiciler (ödeme iste, iade, kargo oluştur) önce dış çağrıyı **idempotency anahtarıyla** yapar (ödeme kimliği, alt sipariş kimliği), sonra yerel durumu koşullu günceller (`WHERE status = 'pending'`).
- **Orkestrasyon yeri:** Ayrı bir saga tablosu yerine sipariş ve ödeme satırlarının durum makineleri saga durumudur. Her adım, bir önceki adımın olayına abonedir (`order.placed → payment.request`, `order.paid → notification + shipping`, `payment.refund_requested → payment.refund`).
- **Ödeme sonucu tek kapıdan:** Callback, mutabakat ve tekrarlar hepsi `ApplyResult`'tan geçer; sipariş → ödeme kilitleri altında son durum kontrol edilir. Çift callback ya da callback + mutabakat yarışı tek etki üretir (10 eşzamanlı callback testi).
- **Telafiler:**
  - ödeme reddi → sipariş iptal, stok iadesi (`FailPaymentTx`, aynı transaction)
  - ödeme süresi doldu (`ORDER_PAYMENT_TTL`) → sipariş `expired`, stok iadesi
  - kapanmış siparişe başarılı ödeme → `refund_requested` → sağlayıcıdan iade
  - ödeme istenmeden sipariş kapandı → sağlayıcıya hiç gidilmez (`abandoned`)
- **Ödeme zaman aşımı:** Sonucu `PAYMENT_RECONCILE_AFTER`'dan uzun süre gelmeyen ödemeler sağlayıcıya sorulur: sonuç varsa uygulanır; sağlayıcı ödemeyi tanımıyorsa yeniden istenir; sipariş kapanmışsa terk edilir.
- **Küme içi sıra, kümeler arası paralellik:** Aynı siparişin olayları sırayla, farklı siparişler paralel (16) iletilir.
- **Webhook güvenliği:** Callback'ler JWT değil HMAC-SHA256 + zaman damgası ile doğrulanır (5 dk tolerans, replay koruması).

**Alternatifler.**
- *Mesaj kuyruğu (Kafka/RabbitMQ):* Ek altyapı ve "DB'ye yazdım ama mesaj gitmedi" ikilisi yine outbox gerektirir. Bu ölçekte PostgreSQL kuyruk olarak yeterli; geçiş gerekirse yayımlayıcı olayları broker'a basacak şekilde değişir, yazma tarafı aynı kalır.
- *Koreografi (her servis kendi olayına tepki verir, merkez yok):* Tek süreçte çalışan modüllerde akışı takip etmeyi zorlaştırıyor; telafi kararlarının (`ApplyResult`) tek yerde olması tercih edildi.
- *Dış çağrıyı transaction içinde yapmak:* Yavaş sağlayıcı kilitleri ve bağlantıları tutar; çağrı sonrası commit başarısız olursa yine tutarsızlık. Dış çağrılar transaction dışında, idempotency anahtarıyla.
- *2PC / dağıtık transaction:* Ödeme sağlayıcıları desteklemez.

**Ölçülen bir sorun ve düzeltmesi.** İlk sürümde yayımlayıcı olayları sırayla işliyordu. Kaos testinde sağlayıcı kapalıyken her deneme DNS/zaman aşımı yüzünden 3–5 sn sürdü ve 5 olayın bir turu ~16 sn aldı: yavaş bir bağımlılık tüm kuyruğu (satıcı bildirimi, kargo) tıkıyordu. Olaylar kümeye göre gruplanıp paralel iletilecek şekilde değiştirildi; küme içi sıra `claim` sorgusundaki `NOT EXISTS` koşuluyla korunuyor (testli: 10 yavaş + 10 hızlı olay 0,44 sn).

**Bilinen sınırlamalar.**
- Devre kesici (circuit breaker) yok: sağlayıcı uzun süre kapalıysa her olay kendi denemesini yapar (üstel bekleme yükü sınırlar ama tek tek).
- Kargo oluşturma kalıcı olarak başarısız olursa olay `dead` olur ve operasyon müdahalesi beklenir; otomatik kısmi iade yapılmaz (iş kararı gerektirir).
- Simülatörün durumu bellektedir; yeniden başlarsa ödemeleri unutur. Bu bilerek bırakıldı: mutabakatın "sağlayıcı tanımıyor → yeniden iste" yolunu gerçekçi biçimde sınar.

---

## ADR-009: Kupon ve fiyat motoru — saf hesap, en büyük kalan yöntemi, yeniden fiyatlamayla iade

**Bağlam.** Pazaryeri ve satıcı kuponları birlikte, minimum sepet, kategori sınırı ve birlikte kullanım kurallarıyla çalışmalı. İndirim satıcılar arasında kuruş kayması olmadan dağılmalı (hakediş buna dayanır). Kısmi iadede indirim geri hesaplanmalı.

**Karar.**
- **Saf paket (`internal/pricing`):** Veritabanı ve saat bilmez; aynı girdi aynı sonucu verir. Sipariş, sepet önizlemesi ve iade aynı fonksiyonu kullanır; bu yüzden üçü asla birbirinden farklı tutar hesaplamaz.
- **Kurallar:**
  - Satıcı kuponunu satıcı, pazaryeri kuponunu pazaryeri karşılar; kırılım satır ve alt sipariş düzeyinde saklanır (`marketplace_discount_kurus`, `seller_discount_kurus`).
  - Önce satıcı kuponları, sonra pazaryeri kuponu kalan tutara uygulanır.
  - Minimum sepet, kuponun uygulanabildiği satırların indirimsiz toplamına bakar.
  - Kapsam başına bir kupon (bir pazaryeri, satıcı başına bir). Birleştirilemeyen kupon varsa "tek başına" ile "birleştirilebilenlerin hepsi" karşılaştırılır, müşteriye daha avantajlı olan seçilir.
  - Yüzde indirim aşağı yuvarlanır.
- **Kuruş kayması yok:** İndirim, uygun satırlara kalan tutarlarıyla orantılı olarak en büyük kalan (Hamilton) yöntemiyle dağıtılır. Kalan eşitse büyük ağırlık, sonra satır kimliği kazanır, yani sonuç deterministiktir. `amount × weight` çarpımı 128-bit (`math/bits`) yapılır; büyük tutarlarda taşma olmaz.
- **Kısmi iade = yeniden fiyatlama:** Siparişte uygulanan kuponların sipariş anındaki tanımları (`order_coupons.snapshot`) kalan satırlara yeniden uygulanır. İade tutarı "iadeden önce ödenmesi gereken" ile "sonra ödenmesi gereken" arasındaki farktır. Böylece:
  - İade edilen kalemin indirim payı geri alınır.
  - Sabit kupon hâlâ geçerliyse indirim kalan kalemlere kayar.
  - Kalan sepet minimum tutarın altına düşerse kupon geri alınır ve kayıp iadeden kesilir (clawback).
  - Kademeli iadelerde toplam iade ödenen tutara kuruşu kuruşuna eşittir (testli).
- **Kullanım limiti:** Toplam limit koşullu `UPDATE coupons SET redeemed_count = redeemed_count + 1 WHERE redeemed_count < max_redemptions` ile korunur; ek olarak `CHECK` kısıtı vardır (T-1 ile aynı desen). Bu `UPDATE` kupon satırını kilitlediği için kullanıcı başına sayım da aynı kupon için sıraya girer. Limit doluysa kupon reddedilir ve sepet onsuz yeniden hesaplanır; her deneme kendi savepoint'indedir. İptal, süre dolumu ve ödeme hatasında kullanım bırakılır.
- **Kısmi satışla etkileşim:** Kuponlar rezervasyondan sonra hesaplanır. Satılamayan kalem düşünce minimum sepet bozulursa kupon nedeniyle reddedilir ve yanıtta gösterilir.
- **%100 indirim:** Tahsil edilecek tutar 0 ise ödeme kaydı açılmaz; sipariş doğrudan ödenmiş sayılır ve saga bildirim/kargo adımından devam eder.

**Alternatifler.**
- *Satır başına ayrı yuvarlama (her satırın payını kendi başına yuvarlamak):* Payların toplamı indirimden 1-2 kuruş sapar. Satıcı hakedişinde bu fark birikir.
- *İadede satırın sipariş anındaki ödenmiş payını aynen geri vermek:* Basit, ama minimum sepet şartı iade sonrası bozulduğunda kuponun haksız kazancı müşteride kalır. Ayrıca kademeli iadelerde yuvarlama farkı birikir.
- *Kuponları sırayla deneyip ilk uyanı almak (birleştirilemeyende):* Müşteri kuponları hangi sırayla girdiğine göre farklı fiyat görür; karşılaştırma daha adil ve açıklanabilir.

**Bilinen sınırlamalar.**
- İadede müşteriden ek ödeme istenmez: kupon kaybı iade edilen kalemden pahalıysa iade 0 olur ve satıcı bazındaki farklar negatif çıkabilir. Hakediş defteri (T-4) bu durumu bir düzeltme kaydı olarak işlemeli.
- Sepetteki kupon önizlemesi kullanım limitini kontrol etmez; limit ödeme adımında ayrılır, o anda dolmuşsa kupon orada reddedilir.
- Kategori sınırı düz listedir; alt kategoriler otomatik dahil edilmez.

---

## ADR-010: Çift taraflı defter — veritabanında zorlanan denge, append-only, idempotent kayıt

**Durum:** Kısmen uygulandı (çekirdek ve şema; olaylara bağlanması, dönem kapanışı ve mutabakat yapılmadı).

**Karar.**
- **Hesap planı:** `platform:cash`, `platform:customer_advances`, `seller:<id>:payable`, `platform:commission_revenue`, `platform:shipping_revenue`, `platform:marketplace_discount`.
- **Gelir tanıma teslimde:** Ödemede para "müşteri avansı"na yazılır. Teslimde avans satıcı alacağına (net = brüt − komisyon − kargo), komisyon ve kargo gelirine dağılır. Pazaryerinin karşıladığı kupon indirimi gider yazılır, satıcının hakedişini azaltmaz.
- **Denge üç katmanda korunur:** Uygulamada `Validate`; veritabanında ertelenmiş kısıt tetikleyicisi (bir işlemin kayıtları birer birer eklenebilir ama dengesiz işlem commit edilemez); append-only tetikleyici (düzeltme ancak ters kayıtla).
- **Idempotency:** Her işlemin iş olayından türeyen bir anahtarı vardır (`accrual:<alt_sipariş>`, `return:<iade>` …). Outbox olayı tekrar gelse de deftere bir kez yazılır.
- **Kurucular işaretli tutarla çalışır:** Negatif tutar kaydı karşı tarafa yazar. Kargo ücreti satıştan büyükse satıcı hesabı borçlanır ve sonraki ödemeden mahsup edilir; iade sıfıra kırpılınca aradaki fark pazaryerine yazılır.
- **Komisyon zaman geçerlilikli:** Aynı kategoride çakışan geçerlilik aralıkları `btree_gist` exclusion constraint ile engellenir; sipariş anındaki oran uygulanır.

**Alternatifler.** *Tek bakiye kolonu (satıcı tablosunda `balance`):* Basit ama geçmiş ve mutabakat kaybolur; neden o tutarın orada olduğu açıklanamaz. *Defteri uygulama kodunda dengeli tutmak:* Bir hata ya da elle yapılan bir SQL ile bozulabilir; veritabanı kısıtı son savunma.
