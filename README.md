# ShopeePay Partner API Gateway — Go Edition

> **Port resmi dari `server.js` (Express) ke Go.** Perilaku 1:1 dengan versi Node, termasuk
> dedup transaksi in-memory dan route `GET /`. Tanpa dependency eksternal — standard library saja.

[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Dependencies](https://img.shields.io/badge/dependencies-0-success)](https://pkg.go.dev)
[![License](https://img.shields.io/badge/license-Proprietary-red.svg?style=flat-square)](#lisensi)

API Gateway ringan untuk verifikasi mutasi transaksi ShopeePay, validasi token, penarikan
riwayat bulanan, dan pembuatan **dynamic QRIS EMVCo** secara instan. 100% stateless —
tanpa database, tanpa background polling per-transaksi.

---

> [!IMPORTANT]
> ### ⚠️ Disclaimer: Unofficial Gateway
>
> Proyek ini adalah **API Gateway tidak resmi** yang **TIDAK berafiliasi, TIDAK didukung, dan
> TIDAK disetujui** oleh PT. Shopee International Indonesia atau Sea Group dalam kapasitas
> apapun.
>
> Cara kerjanya: membaca data dari **ShopeePay Partner Portal** menggunakan sesi akun merchant
> Anda sendiri (token internal), mirip cara kerja extension browser atau skrip otomasi pihak
> ketiga. Credential **tidak pernah dikirim ke pihak lain** selain server resmi ShopeePay.
> Gateway bersifat **read-only** — tidak mengubah saldo, menarik dana, atau memodifikasi transaksi.
>
> Anda bertanggung jawab penuh atas kepatuhan terhadap ketentuan layanan ShopeePay dan
> peraturan yang berlaku di wilayah Anda.

---

## Fitur

| Fitur | Keterangan |
|---|---|
| **Verifikasi pembayaran** | Cocokkan nominal + rentang waktu, dedup otomatis agar satu struk tidak bisa diklaim dua kali |
| **Dynamic QRIS EMVCo** | Sisipkan nominal ke static QRIS Anda, hitung ulang CRC16-CCITT, expires 15 menit |
| **Riwayat transaksi** | Harian (berpaginasi) dan bulanan (otomatis ambil semua halaman) |
| **Validasi token** | Token dicek otomatis tiap 5 menit, notifikasi Telegram saat expired |
| **Multi-token** | Header `X-Shopee-Token` untuk cek pakai token lain tanpa mengubah token global |
| **Log in-memory** | 100 baris terakhir, tersedia via API |
| **Tanpa dependency** | Cuma standard library. Binary ~9.7 MB, tanpa CGO |

---

## Requirements

- **Go 1.22+** (memakai pola route `net/http` versi baru + `math/rand/v2`)
- Akun **ShopeePay Partner** yang sudah aktif
- Static QRIS merchant (opsional, hanya untuk `/create-qris`)

---

## Instalasi

```bash
git clone https://github.com/ahmadzakiyox/shoppepay-api-gateway.git
cd shoppepay-api-gateway
cp .env.example .env
```

Edit `.env`, lalu:

```bash
go build -o shoppepay-gateway .
./shoppepay-gateway
```

Output startup:

```
Endpoints:
  POST /update-token       - Update ShopeeToken
  GET  /token-status       - Check ShopeeToken Validity status
  POST /create-qris        - Generate Dynamic QRIS from static template
  GET  /qr/:id             - Fetch Dynamic QRIS Image Redirect
  GET  /transactions       - Fetch transactions list
  GET  /transactions/all   - Fetch all transactions of the month
```

---

## Konfigurasi

Semua konfigurasi lewat environment variable (baca dari `.env` saat boot, atau dari
environment sistem). Nilai bertanda kutip akan otomatis dilepas.

| Variable | Wajib | Default | Keterangan |
|---|:---:|---|---|
| `SHOPEE_TOKEN` | ya | — | Token internal dari ShopeePay Partner Portal |
| `API_KEY` | ya | — | Kunci untuk endpoint yang butuh autentikasi |
| `PORT` | tidak | `4000` | Port HTTP |
| `QRIS_STATIC` | tidak | — | Static QRIS merchant. Kosongkan kalau tidak pakai `/create-qris` |
| `TELEGRAM_BOT_TOKEN` | tidak | — | Untuk notifikasi token expired |
| `TELEGRAM_CHAT_ID` | tidak | — | Chat tujuan notifikasi |

> [!TIP]
> `SHOPEE_TOKEN` **bisa diganti saat runtime** lewat `POST /update-token` — tidak perlu restart.

> [!WARNING]
> `SHOPEE_TOKEN` itu berumur pendek dan **expire rutin**. Jalankan token checker (otomatis aktif
> kalau `SHOPEE_TOKEN` + `API_KEY` terisi) dan monitored `GET /token-status`.

---

## Autentikasi

Semua endpoint kecuali `GET /`, `GET /api/health`, dan `GET /qr/{id}` memerlukan API key.
Bisa lewat dua cara:

```bash
# Header (disarankan)
curl -H "X-API-Key: $API_KEY" http://localhost:4000/token-status

# Query string (untuk browser / webhook)
curl "http://localhost:4000/token-status?api_key=$API_KEY"
```

Kunci salah / tidak ada → `401` dengan body:

```json
{ "success": false, "error": "Invalid or missing API key" }
```

---

## API Reference

### `GET /` — Health check ringan

Tanpa autentikasi. Dipakai buat uptime monitor.

```bash
curl http://localhost:4000/
```

```
Shoppe API Running
```

---

### `GET /api/health` — Health check

Tanpa autentikasi.

```json
{
  "success": true,
  "message": "ShopeePay API Service is running",
  "timestamp": "2026-09-29T02:30:41.335Z"
}
```

---

### `POST /update-token` — Ganti Shopee token

```bash
curl -X POST http://localhost:4000/update-token \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"token":"B:EcBKwXTy0..."}'
```

```json
{ "success": true, "data": { "message": "Token updated" } }
```

| Error | Status | Pesan |
|---|:---:|---|
| Body tanpa `token` | `400` | `Provide token in body` |

---

### `GET /token-status` — Status validitas token

Dicek otomatis tiap 5 menit, plus sekali saat boot.

```json
{
  "success": true,
  "data": {
    "token_status": "valid",
    "message": "Token is working"
  }
}
```

Kalau token expired:

```json
{
  "success": false,
  "data": {
    "token_status": "invalid",
    "message": "Token expired/invalid. Please update via POST /update-token"
  }
}
```

> Notifikasi Telegram dikirim **sekali** per transisi tidak-valid, lalu di-reset saat token
> kembali valid — supaya tidak spam.

---

### `POST /create-qris` — Generate dynamic QRIS

Sisipkan nominal ke `QRIS_STATIC`, hitung ulang CRC, simpan 15 menit.

```bash
curl -X POST http://localhost:4000/create-qris \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"amount":15000}'
```

```json
{
  "success": true,
  "data": {
    "qris_url": "http://localhost:4000/qr/54a4c16d",
    "amount": 15000,
    "expires_at": "2026-09-29 09:46:32",
    "expires_in": "15 menit"
  }
}
```

| Error | Status | Pesan |
|---|:---:|---|
| `amount` ≤ 0 / bukan angka | `400` | `Provide valid amount (positive integer)` |
| `QRIS_STATIC` kosong | `500` | `QRIS_STATIC belum diset di .env` |

Nominal dalam **satuan penuh** (bukan ribuan): `15000` = Rp 15.000.

---

### `GET /qr/{id}` — Ambil gambar QRIS

Tanpa autentikasi — aman dipakai langsung di `<img src>`. Meng-issue **redirect 302** ke
QR generator dengan payload QRIS ter-URL-encode.

```bash
curl -i http://localhost:4000/qr/54a4c16d
```

```
HTTP/1.1 302 Found
Location: https://api.qrserver.com/v1/create-qr-code/?size=300x300&data=0002010102...
```

| Status | Body | Kapan |
|:---:|---|---|
| `302` | — | QR valid |
| `404` | `QR not found` | ID tidak dikenal |
| `410` | `QR expired` | Sudah lewat 15 menit (entry dihapus) |

> Integrasi langsung: `<img src="http://localhost:4000/qr/54a4c16d" />`

---

### `GET /transactions` — Daftar transaksi (berpaginasi)

**Query parameter** (semua opsional):

| Parameter | Default | Keterangan |
|---|---|---|
| `startTime` | `now - 3 hari` | Unix timestamp |
| `endTime` | `now` | Unix timestamp |
| `pageSize` | `10` | Jumlah per halaman |
| `next_position` | — | Kursor halaman berikutnya dari respons sebelumnya |

```bash
curl -H "X-API-Key: $API_KEY" "http://localhost:4000/transactions?pageSize=20"
```

```json
{
  "success": true,
  "total_amount": "409.662",
  "data": {
    "transactions": [
      {
        "amount": 65000,
        "status": "success",
        "time": "2026-09-07 14:24:34",
        "issuer": "Seabank"
      }
    ]
  }
}
```

`issuer` hanya muncul kalau lookup detail berhasil — dan itu satu panggilan ke API detail **per transaksi**,
jadi `pageSize` besar = banyak request. `time` selalu WIB (UTC+7).

---

### `GET /transactions/all` — Semua transaksi bulan ini

Otomatis menarik semua halaman (mulai **awal bulan kalender WIB**) sampai habis. Dipaginasi
otomatis dengan jeda 500 ms antar halaman.

```bash
curl -H "X-API-Key: $API_KEY" http://localhost:4000/transactions/all
```

```json
{
  "success": true,
  "total_amount": "1",
  "data": {
    "period": "2026-09-01 s/d 2026-09-29",
    "total_count": 1,
    "transactions": [
      { "amount": 65000, "status": "success", "time": "2026-09-07 14:24:34", "issuer": "Seabank" }
    ]
  }
}
```

> Endpoint ini bisa lambat kalau transaksi banyak — satu request = banyak panggilan ke ShopeePay.

---

### `POST /check-payment` — Verifikasi pembayaran

Endpoint utama. Cocokkan transaksi yang **sukses**, nominal sama, dan `createTime` dalam
rentang pencarian.

```bash
curl -X POST http://localhost:4000/check-payment \
  -H "X-API-Key: $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"amount":65000,"startTime":1788762274}'
```

**Body:**

| Field | Wajib | Default | Keterangan |
|---|:---:|---|---|
| `amount` | ya | — | Nominal penuh, harus > 0 |
| `startTime` | tidak | `now - 30 menit` | Unix timestamp batas bawah |

Belum dibayar:

```json
{ "success": true, "paid": false }
```

Sudah dibayar:

```json
{
  "success": true,
  "paid": true,
  "transaction": {
    "transactionId": "239706725649854541",
    "amount": 65000,
    "status": "success",
    "time": "2026-09-07 14:24:34",
    "issuer": "Seabank"
  }
}
```

Kunci polling: **`/check-payment` bersifat destruktif terhadap struk** — begitu kecocokan
ditemukan, struk itu ditandai terpakai dan tidak akan dikembalikan lagi. Lihat
[Peringatan Dedup](#⚠️-peringatan-dedup).

| Error | Status |
|---|:---:|
| `amount` ≤ 0 / bukan angka | `400` |
| Error dari ShopeePay (`code` ≠ 0) | `400` |
| Exception / timeout (20 dtk) | `500` |

---

### `GET /api/logs` — Log terbaru

100 baris terakhir, lebih baru di depan.

```bash
curl -H "X-API-Key: $API_KEY" http://localhost:4000/api/logs
```

```json
{
  "success": true,
  "data": {
    "logs": [
      { "timestamp": "2026-09-29T02:30:41.594Z", "level": "INFO", "message": "Token valid" }
    ]
  }
}
```

---

## Multi-token (opsional)

Kirim header `X-Shopee-Token` untuk mengecek transaksi milik merchant lain **tanpa mengubah**
token global server:

```bash
curl -X POST http://localhost:4000/check-payment \
  -H "X-API-Key: $API_KEY" \
  -H "X-Shopee-Token: B:TokenMerchantLain..." \
  -H "Content-Type: application/json" \
  -d '{"amount":50000}'
```

---

## Status Code

| Code | Arti |
|:---:|---|
| `200` | Sukses |
| `204` | CORS preflight (`OPTIONS`) |
| `302` | Redirect gambar QRIS |
| `400` | Input tidak valid / ditolak ShopeePay |
| `401` | API key salah atau tidak ada |
| `404` | Route atau QRIS tidak ditemukan |
| `405` | Method tidak diizinkan untuk route tersebut |
| `410` | QRIS kedaluwarsa |
| `500` | Error internal / upstream |

Semua error konsisten berbentuk:

```json
{ "success": false, "error": "pesan" }
```

---

## CORS

Header CORS diaktifkan secara global (origin `*`) supaya bisa dipanggil langsung dari halaman kasir
di browser tanpa proxy. Preflight `OPTIONS` di-answer `204` dan dikembalikan otomatis.

> Kalau di-host di domain publik, pertimbangkan batasi origin — `Access-Control-Allow-Origin: *`
> plus `Access-Control-Allow-Credentials: true` itu kombinasi yang longgar.

---

## Testing

```bash
go test ./...          # 26 test
go test -v ./...       # verbose
go test -run Dedup -v  # filter
```

Suite-nya mencakup:

- **Golden test QRIS** — output dibandingkan byte-per-byte dengan implementasi JavaScript
  asli (dijalankan di Bun) untuk 7 nominal, jadi regresi EMVCo/CRC langsung ketahuan
- **Paritas `parseTLV`** — 7 kasus batas (length non-numerik, TLV terpotong, buffer overrun)
  diverifikasi terhadap output JS
- **`/check-payment` end-to-end** — dijalankan melawan fake Shopee server: dedup, error API,
  propagasi `500`, dan preservasi `transactionId` 18 digit (inti dari pembulatan `float64`)
- **Race pada dedup** — 64 goroutine bersaing, harus tepat satu yang menang klaim
- **Helper parsing** — `cleanAmount` (termasuk format `"9.600"`), `parseInt`, status mapping,
  format waktu WIB

Test tidak butuh jaringan maupun credential asli.

---

## Deployment

### systemd (Linux)

```bash
go build -o shoppepay-gateway .

sudo useradd -r -s /usr/sbin/nologin gateway
sudo install -m755 shoppepay-gateway /usr/local/bin/
sudo mkdir -p /opt/shoppepay-gateway
sudo cp .env /opt/shoppepay-gateway/
sudo chown -R gateway:gateway /opt/shoppepay-gateway
```

`/etc/systemd/system/shoppepay-gateway.service`:

```ini
[Unit]
Description=ShopeePay Partner API Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=gateway
Group=gateway
WorkingDirectory=/opt/shoppepay-gateway
EnvironmentFile=/opt/shoppepay-gateway/.env
ExecStart=/usr/local/bin/shoppepay-gateway
Restart=always
RestartSec=5

# Pengetatan
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadOnlyPaths=/opt/shoppepay-gateway

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now shoppepay-gateway
sudo journalctl -u shoppepay-gateway -f
```

### Reverse proxy

Contoh nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:4000;
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-Proto $scheme;   # dipakai untuk qris_url
    proxy_set_header X-Real-IP         $remote_addr;
}
```

> `X-Forwarded-Proto` ikut dibaca untuk menentukan skema pada `qris_url` — tanpa header ini
> `qris_url` akan selalu `http://` walau diakses via HTTPS.

### Cross-compile

Karena tanpa CGO, build silang sesuka hati:

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/gateway-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o dist/gateway-linux-arm64 .
GOOS=windows GOARCH=amd64 go build -o dist/gateway-windows.exe .
GOOS=darwin  GOARCH=arm64 go build -o dist/gateway-macos-arm64 .
```

---

## ⚠️ Peringatan Dedup

`/check-payment` memakai store in-memory (`usedTransactionIds`) supaya satu struk tidak bisa
diklaim oleh dua pembeli sekaligus — kalau nominal kembar, hanya polling pertama yang dapat
`paid: true`.

**Jendela dedup di-anchor ke `createTime` transaksi, bukan ke waktu klaim.** Artinya resip yang
sudah **lebih tua dari 24 jam akan langsung di-evict** pada pengecekan berikutnya dan bisa
diklaim ulang. Ini perilaku yang sama persis dengan versi Node/Express, dan sengaja
dipertahankan agar port ini benar-benar 1:1.

Kalau proteksi yang lo maksud adalah *"struk ini tidak bisa dipakai lagi selama 24 jam sejak
diklaim"*, ubah nilai yang disimpan dari `createTime` transaksi ke `time.Now().Unix()` di
`dedupClaim` (`main.go`), lalu sesuaikan tesnya.

**Konsekuensi lain:** store ini in-memory, jadi **restart = semua dedup hilang**. Untuk
lingkungan multi-instance (load balancer), dedup tidak konsisten antar instance — butuh store
eksternal seperti Redis kalau itu relevan.

---

## Catatan Teknis

- **`transactionId` dibaca dengan `json.Decoder.UseNumber()`** — ID transaksi ShopeePay bisa
  18 digit, yang melebihi presisi `float64` (2^53). Pakai `float64` akan membuat digit
  terakhir berubah. Ada test khusus untuk mengunci ini.
- **WIB = offset tetap +7 jam**, bukan `Asia/Jakarta` dari tzdata, supaya identik dengan
  perhitungan offset manual versi JS dan tidak bergantung zona waktu server.
- **Hanya standard library.** Nol `go get`, nol `vendor/`, nol CVEs dari transitif dependency.
  Parser `.env` (~20 baris) dan parser `application/x-www-form-urlencoded` ditulis sendiri.
- **Body dibatasi 1 MB**, CORS dan batas request diurus di satu middleware.
- **Mutex terpisah** untuk token, store QRIS, dedup, dan log — `/check-payment` bisa paralel
  tanpa race (diverifikasi `TestDedupIsConcurrencySafe`).

---

## Project Layout

```
.
├── main.go                  # aplikasi (≈1085 baris)
├── go.mod
├── .env.example
├── qris_test.go             # golden values + paritas dengan JS
├── checkpayment_test.go     # end-to-end vs fake Shopee
├── dedup_test.go            # dedup, QRIS store, log ring
└── helpers_test.go
```

---

## Lisensi

**Proprietary (Komersial).** Hak cipta dilindungi undang-undang.

Dilarang keras menyebarkan ulang, melakukan reverse engineering, dekompilasi, atau
memperjualbelikan kembali kode sumber/aplikasi ini tanpa izin tertulis dari pemilik hak cipta resmi.

> Proyek ini mem-porting ulang implementasi JavaScript yang ada di repositori ini sendiri.
> Ikuti lisensi asli — jika Anda bukan pemilik hak cipta, pastikan Anda memiliki izin.

---

## Credits

- Versi JavaScript/Express asli: `server.js` (sudah di-remove dalam edisi ini; tersedia di
  riwayat git)
- Protokol QRIS mengikuti standar **EMVCo** (TLV + CRC-16/CCITT)
