package main

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeShopee stands in for the ShopeePay partner API so /check-payment can be
// exercised end to end, including the in-memory dedup that decides whether a
// receipt can be claimed twice.
type fakeShopee struct {
	createTime  int64
	amount      string
	transaction string
	detailHits  int
}

func (f *fakeShopee) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/get-transaction-list", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "\"pageSize\"") {
			t.Errorf("list payload missing pageSize: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "",
			"data": map[string]any{
				"totalNetSales": "65000",
				"list": []any{map[string]any{
					"transactionId":        f.transaction,
					"displayTransactionId": f.transaction,
					"amount":               f.amount,
					"status":               3,
					"createTime":           f.createTime,
				}},
			},
		})
	})
	mux.HandleFunc("/get-transaction-detail", func(w http.ResponseWriter, r *http.Request) {
		f.detailHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"issuer": "Seabank"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeShopee) wire(s *server, srv *httptest.Server) {
	s.listURL = srv.URL + "/get-transaction-list"
	s.detailURL = srv.URL + "/get-transaction-detail"
}

func checkPayment(t *testing.T, s *server, amount int64, startTime int64) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"amount": amount, "startTime": startTime})
	req := httptest.NewRequest("POST", "/check-payment", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCheckPayment(rec, req)

	var out map[string]any
	_ = decodeUseNumber(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A transaction younger than 24h is claimed once; every later poll for the same
// amount must come back unpaid so two customers cannot both settle the invoice.
func TestCheckPaymentDedupsWithinWindow(t *testing.T) {
	now := time.Now().Unix()
	f := &fakeShopee{createTime: now - 60, amount: "65000", transaction: "239706725649854541"}
	srv := f.start(t)

	s := newServer()
	f.wire(s, srv)
	s.apiKey = "k"

	start := now - 3600

	code, out := checkPayment(t, s, 65000, start)
	if code != 200 {
		t.Fatalf("call 1 status = %d", code)
	}
	if out["paid"] != true {
		t.Fatalf("call 1 should be paid, got %v", out["paid"])
	}
	trx, _ := out["transaction"].(map[string]any)
	if trx["transactionId"] != f.transaction {
		t.Errorf("transactionId = %v, want %v", trx["transactionId"], f.transaction)
	}
	if trx["amount"].(json.Number).String() != "65000" {
		t.Errorf("amount = %v", trx["amount"])
	}
	if trx["status"] != "success" {
		t.Errorf("status = %v", trx["status"])
	}
	if trx["issuer"] != "Seabank" {
		t.Errorf("issuer = %v, want Seabank", trx["issuer"])
	}

	// second and third poll must be rejected by the dedup store
	for i := 2; i <= 3; i++ {
		code, out = checkPayment(t, s, 65000, start)
		if code != 200 {
			t.Fatalf("call %d status = %d", i, code)
		}
		if out["paid"] != false {
			t.Errorf("call %d should be paid:false (dedup), got %v", i, out["paid"])
		}
		if _, has := out["transaction"]; has {
			t.Errorf("call %d must not include a transaction block: %v", i, out)
		}
	}
}

// The dedup window is anchored to the transaction's createTime, matching the
// original: a receipt older than 24h is evicted on the very next check, so the
// same receipt can be claimed again.
func TestCheckPaymentDedupWindowAnchoredToCreateTime(t *testing.T) {
	now := time.Now().Unix()
	old := now - 25*3600
	f := &fakeShopee{createTime: old, amount: "65000", transaction: "111"}
	srv := f.start(t)

	s := newServer()
	f.wire(s, srv)

	start := old - 60
	for i := 1; i <= 2; i++ {
		code, out := checkPayment(t, s, 65000, start)
		if code != 200 {
			t.Fatalf("call %d status = %d", i, code)
		}
		if out["paid"] != true {
			t.Errorf("call %d: stale receipt (>24h) is not deduped, got paid=%v", i, out["paid"])
		}
	}
}

func TestCheckPaymentRejectsNonMatching(t *testing.T) {
	now := time.Now().Unix()
	f := &fakeShopee{createTime: now - 60, amount: "65000", transaction: "111"}
	srv := f.start(t)

	s := newServer()
	f.wire(s, srv)
	start := now - 3600

	// different amount -> unpaid
	if _, out := checkPayment(t, s, 999, start); out["paid"] != false {
		t.Errorf("amount mismatch should be unpaid, got %v", out)
	}
	// startTime after the transaction -> unpaid
	if _, out := checkPayment(t, s, 65000, now+60); out["paid"] != false {
		t.Errorf("startTime after createTime should be unpaid, got %v", out)
	}
	// invalid amount -> 400
	code, out := checkPayment(t, s, 0, start)
	if code != 400 || out["error"] != "Provide valid amount (positive integer)" {
		t.Errorf("amount 0 -> %d %v", code, out)
	}
}

// An 18-digit transactionId must survive the round trip; float64 would round it.
func TestCheckPaymentPreservesBigTransactionID(t *testing.T) {
	now := time.Now().Unix()
	const id = "264693445089687719"
	f := &fakeShopee{createTime: now - 60, amount: "1008", transaction: id}
	srv := f.start(t)

	s := newServer()
	f.wire(s, srv)

	_, out := checkPayment(t, s, 1008, now-3600)
	trx, _ := out["transaction"].(map[string]any)
	got, _ := trx["transactionId"].(string)
	if got != id {
		t.Errorf("transactionId = %q, want %q (precision lost?)", got, id)
	}
}

func TestCheckPaymentPropagatesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200020, "msg": "token invalid"})
	}))
	defer srv.Close()

	s := newServer()
	s.listURL = srv.URL + "/get-transaction-list"
	s.detailURL = srv.URL + "/get-transaction-detail"

	code, out := checkPayment(t, s, 1000, time.Now().Unix()-600)
	if code != 400 {
		t.Errorf("status = %d, want 400", code)
	}
	if out["error"] != "token invalid" {
		t.Errorf("error = %v, want %q", out["error"], "token invalid")
	}
}

func TestCheckPaymentHandlesUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newServer()
	s.listURL = srv.URL + "/get-transaction-list"
	s.detailURL = srv.URL + "/get-transaction-detail"

	code, out := checkPayment(t, s, 1000, time.Now().Unix()-600)
	if code != 500 {
		t.Errorf("status = %d, want 500", code)
	}
	if !strings.Contains(out["error"].(string), "Request failed with status code 500") {
		t.Errorf("error = %v", out["error"])
	}
}

// formatTransaction output shape, incl. the "9.600" style amount Shopee returns.
func TestFormatTransaction(t *testing.T) {
	got := formatTransaction(map[string]any{
		"createTime": json.Number("1784050000"),
		"amount":     "9.600",
		"status":     json.Number("3"),
	})
	if got.Amount != 9600 {
		t.Errorf("amount = %d, want 9600", got.Amount)
	}
	if got.Status != "success" {
		t.Errorf("status = %s", got.Status)
	}
	if got.Time != "2026-07-15 00:26:40" {
		t.Errorf("time = %s", got.Time)
	}
	if got.Issuer != nil {
		t.Errorf("issuer should be absent, got %v", *got.Issuer)
	}
}

func TestTotalSalesFalsyHandling(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{nil, `"0"`},
		{map[string]any{"totalNetSales": json.Number("0")}, `"0"`},
		{map[string]any{"totalNetSales": ""}, `"0"`},
		{map[string]any{"totalNetSales": json.Number("409.662")}, `409.662`},
		{map[string]any{"totalNetSales": "409.662"}, `"409.662"`},
	}
	for _, c := range cases {
		if got := string(totalSales(c.in)); got != c.want {
			t.Errorf("totalSales(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestParseTLVExposesRequiredTags(t *testing.T) {
	// the pinned merchant static QRIS must expose the currency (53) slot that
	// generateDynamicQRIS injects the amount (54) after
	fields := parseTLV(testQRISStatic)
	tags := map[string]bool{}
	for _, f := range fields {
		tags[f.tag] = true
	}
	for _, want := range []string{"00", "01", "26", "52", "53", "58", "59", "60", "61"} {
		if !tags[want] {
			t.Errorf("static QRIS missing tag %s (got %v)", want, tags)
		}
	}
	if !tags["63"] {
		t.Error("static QRIS should carry a CRC tag to be stripped and recomputed")
	}
}

// Whatever QRIS_STATIC is in .env must still yield a well-formed payload, even
// after the user swaps in a different merchant code or adds quotes.
func TestGenerateDynamicQRISWithLocalEnv(t *testing.T) {
	static := testStaticQRIS(t)
	if static == "" {
		t.Skip("no QRIS_STATIC in .env")
	}
	got, err := generateDynamicQRIS(static, 15000)
	if err != nil {
		t.Fatalf("local QRIS_STATIC rejected: %v", err)
	}
	if !strings.HasPrefix(got, "0002") {
		t.Errorf("payload must start with 0002, got %.4q", got)
	}
	// structural check: exactly one tag 54 carrying the requested amount
	var amountTags []string
	for _, f := range parseTLV(got) {
		if f.tag == "54" {
			amountTags = append(amountTags, f.value)
		}
	}
	if len(amountTags) != 1 || amountTags[0] != "15000" {
		t.Errorf("tag 54 values = %v, want [15000]", amountTags)
	}
	crc := got[len(got)-4:]
	for _, c := range crc {
		if !strings.ContainsRune("0123456789ABCDEF", c) {
			t.Errorf("CRC %q is not uppercase hex", crc)
			break
		}
	}
	// recomputing the CRC over the body must reproduce the trailer
	if body := got[:len(got)-4]; !strings.HasSuffix(got, "6304"+crc16CCITT(body)) {
		t.Errorf("CRC mismatch: body=%s trailer=%s", body, got[len(got)-8:])
	}
}

func TestHexIDLength(t *testing.T) {
	// /create-qris ids are crypto/randomBytes(4).toString("hex") == 8 chars
	var b [4]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	if n := len(hex.EncodeToString(b[:])); n != 8 {
		t.Errorf("id length = %d, want 8", n)
	}
}

// /transactions/all must window the current calendar month in WIB.
func TestStartOfMonthWIB(t *testing.T) {
	now := time.Now()
	year, month, _ := now.UTC().Add(wib).Date()
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Add(-wib)

	if got, want := formatWIBDate(start), fmt.Sprintf("%04d-%02d-01", year, int(month)); got != want {
		t.Errorf("start of month = %s, want %s", got, want)
	}
	// the window opens exactly at WIB midnight, i.e. 17:00 UTC the day before
	if h, m, sec := start.UTC().Clock(); h != 17 || m != 0 || sec != 0 {
		t.Errorf("start instant = %02d:%02d:%02d UTC, want 17:00:00", h, m, sec)
	}
	if start.Unix() >= now.Unix() {
		t.Error("start of month must be in the past")
	}
}
