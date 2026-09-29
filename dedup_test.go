package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The dedup store is what stops two customers paying the same amount at the
// same time from both claiming the same receipt. Verify the claim/GC semantics
// match the JS: claim on first paid match, skip on every later poll, and evict
// entries whose createTime is older than 24h.
func TestDedupClaimAndGC(t *testing.T) {
	s := newServer()

	if s.dedupSeen("tx-1") {
		t.Fatal("fresh id must not be seen as used")
	}

	now := time.Now().Unix()
	s.dedupClaim("tx-1", now)
	if !s.dedupSeen("tx-1") {
		t.Fatal("id must be marked used after claim")
	}

	// recent entry survives the 24h sweep
	s.dedupGC(now - 24*3600)
	if !s.dedupSeen("tx-1") {
		t.Fatal("entry younger than 24h must survive GC")
	}

	// entry created 25h ago is evicted
	s.dedupClaim("tx-old", now-25*3600)
	s.dedupGC(now - 24*3600)
	if s.dedupSeen("tx-old") {
		t.Fatal("entry older than 24h must be evicted")
	}
	if !s.dedupSeen("tx-1") {
		t.Fatal("GC must not evict the fresh entry")
	}
}

func TestDedupIsConcurrencySafe(t *testing.T) {
	s := newServer()
	done := make(chan bool, 64)
	for i := 0; i < 64; i++ {
		go func() {
			if s.dedupSeen("race") {
				done <- false
				return
			}
			s.dedupClaim("race", time.Now().Unix())
			done <- true
		}()
	}
	won := 0
	for i := 0; i < 64; i++ {
		if <-done {
			won++
		}
	}
	if won != 1 {
		t.Errorf("exactly one caller should have won the claim, got %d", won)
	}
}

// amountToFloat gate: JS rejects !amount || amount <= 0
func TestAmountGate(t *testing.T) {
	reject := []any{nil, "", "0", jsonNumber(0), float64(0), float64(-1), "abc"}
	for _, v := range reject {
		f, ok := floatOf(v)
		if ok && f != 0 && f > 0 {
			t.Errorf("expected %v (%T) to be rejected", v, v)
		}
	}
	accept := []any{jsonNumber(1), jsonNumber(1008), "15000", float64(1008)}
	for _, v := range accept {
		f, ok := floatOf(v)
		if !ok || f <= 0 {
			t.Errorf("expected %v (%T) to be accepted", v, v)
		}
	}
}

func TestQRISStoreExpiry(t *testing.T) {
	s := newServer()
	s.qrisSt["live"] = qrisEntry{data: "abc", expiresAt: time.Now().Add(time.Minute)}
	s.qrisSt["dead"] = qrisEntry{data: "abc", expiresAt: time.Now().Add(-time.Minute)}

	// /qr/live must redirect
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/qr/live", nil)
	req.SetPathValue("id", "live")
	s.handleQR(rec, req)
	if rec.Code != 302 {
		t.Errorf("live QR = %d, want 302", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "data=abc") {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}

	// /qr/dead must be 410 and evict the entry
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/qr/dead", nil)
	req.SetPathValue("id", "dead")
	s.handleQR(rec, req)
	if rec.Code != 410 {
		t.Errorf("expired QR = %d, want 410", rec.Code)
	}
	s.qrisMu.Lock()
	_, still := s.qrisSt["dead"]
	s.qrisMu.Unlock()
	if still {
		t.Error("expired entry must be deleted on access")
	}

	// unknown id is 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/qr/nope", nil)
	req.SetPathValue("id", "nope")
	s.handleQR(rec, req)
	if rec.Code != 404 {
		t.Errorf("unknown QR = %d, want 404", rec.Code)
	}
}

func TestLogRingBufferKeepsLast100(t *testing.T) {
	s := newServer()
	for i := 0; i < 150; i++ {
		s.logEvent("INFO", "line")
	}
	got := s.snapshotLogs()
	if len(got) != 100 {
		t.Fatalf("ring buffer len = %d, want 100", len(got))
	}
}
