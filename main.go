// ShopeePay Partner API Gateway — Go port of server.js
//
// Ported from the obfuscated server.js (javascript-obfuscator output) at
// commit 4fbcbd0. Behaviour is a 1:1 port of the Node/Express original,
// including the in-memory 24h transaction dedup and the GET / root route.
//
// Single file, standard library only.

package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const wib = 7 * time.Hour

const (
	shopeeListURL   = "https://shopeepay.shopee.co.id/merchant/v1/partner-web/get-transaction-list"
	shopeeDetailURL = "https://shopeepay.shopee.co.id/merchant/v1/partner-web/get-transaction-detail"
	qrImageAPI      = "https://api.qrserver.com/v1/create-qr-code/?size=300x300&data="
)

// ---------------------------------------------------------------- state

type qrisEntry struct {
	data      string
	expiresAt time.Time
}

type logEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

type server struct {
	apiKey           string
	telegramBotToken string
	telegramChatID   string
	qrisStatic       string
	port             string

	client *http.Client

	// endpoint overridable so tests can point at a fake Shopee
	listURL   string
	detailURL string

	mu         sync.RWMutex
	shopeeTok  string
	tokenValid bool
	tokenNotif bool

	qrisMu   sync.Mutex
	qrisSt   map[string]qrisEntry
	dedupMu  sync.Mutex
	dedup    map[string]int64
	logsMu   sync.Mutex
	logLines []logEntry
}

func newServer() *server {
	return &server{
		apiKey:           env("API_KEY", ""),
		telegramBotToken: env("TELEGRAM_BOT_TOKEN", ""),
		telegramChatID:   env("TELEGRAM_CHAT_ID", ""),
		qrisStatic:       env("QRIS_STATIC", ""),
		port:             env("PORT", "4000"),
		client:           &http.Client{Timeout: 20 * time.Second},
		listURL:          shopeeListURL,
		detailURL:        shopeeDetailURL,
		shopeeTok:        env("SHOPEE_TOKEN", ""),
		tokenValid:       true,
		qrisSt:           make(map[string]qrisEntry),
		dedup:            make(map[string]int64),
	}
}

func (s *server) getToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shopeeTok
}

func (s *server) setToken(t string) {
	s.mu.Lock()
	s.shopeeTok = t
	s.mu.Unlock()
}

// ---------------------------------------------------------------- logging

func (s *server) logEvent(level, message string) {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	fmt.Printf("[%s] [%s] %s\n", ts, level, message)
	s.logsMu.Lock()
	s.logLines = append(s.logLines, logEntry{ts, level, message})
	if len(s.logLines) > 100 {
		s.logLines = s.logLines[len(s.logLines)-100:]
	}
	s.logsMu.Unlock()
}

func (s *server) snapshotLogs() []logEntry {
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	out := make([]logEntry, len(s.logLines))
	copy(out, s.logLines)
	return out
}

// ---------------------------------------------------------------- .env

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// ---------------------------------------------------------------- JSON helpers
//
// Everything decoded with UseNumber() so transactionId (an 18-digit value)
// survives round-trip without float64 precision loss.

func decodeUseNumber(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

func mget(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func mmap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func mlist(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].([]any); ok {
		return v
	}
	return nil
}

// numToString mirrors JavaScript String(x).
func numToString(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case json.Number:
		return t.String()
	case string:
		return t
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e21 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return "undefined"
}

func numOf(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case float64:
		return int64(t)
	case string:
		return parseIntPrefix(t)
	}
	return 0
}

func floatOf(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// parseIntPrefix mirrors parseInt(s, 10) — leading digits only, NaN → 0.
func parseIntPrefix(s string) int64 {
	s = strings.TrimSpace(s)
	i := 0
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var n int64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int64(s[i]-'0')
		i++
	}
	if i == start {
		return 0
	}
	if neg {
		return -n
	}
	return n
}

// cleanAmount mirrors String(t.amount || "0").replace(/\./g,"").replace(/,/g,"")
// followed by parseInt(..., 10) || 0.
func cleanAmount(v any) int64 {
	var s string
	switch t := v.(type) {
	case nil:
		s = "0"
	case json.Number:
		if t.String() == "" || t.String() == "0" {
			s = "0"
		} else {
			s = t.String()
		}
	case float64:
		if t == 0 {
			s = "0"
		} else {
			s = numToString(t)
		}
	case string:
		if t == "" {
			s = "0"
		} else {
			s = t
		}
	default:
		s = "0"
	}
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, ",", "")
	return parseIntPrefix(s)
}

func statusName(v any) string {
	switch numToString(v) {
	case "1":
		return "pending"
	case "2":
		return "failed"
	case "3":
		return "success"
	case "4":
		return "refunded"
	case "5":
		return "expired"
	}
	return "unknown_" + numToString(v)
}

// txnID mirrors tx.transactionId || tx.displayTransactionId
func txnID(tx map[string]any) string {
	if s := numToString(mget(tx, "transactionId")); s != "undefined" && s != "" && s != "0" {
		return s
	}
	return numToString(mget(tx, "displayTransactionId"))
}

// formatWIB renders "YYYY-MM-DD HH:MM:SS" in UTC+7, matching the original's
// manual offset-shifted getUTC*() calls.
func formatWIB(t time.Time) string {
	return t.UTC().Add(wib).Format("2006-01-02 15:04:05")
}

func formatWIBDate(t time.Time) string {
	return t.UTC().Add(wib).Format("2006-01-02")
}

type txnOut struct {
	Amount int64   `json:"amount"`
	Status string  `json:"status"`
	Time   string  `json:"time"`
	Issuer *string `json:"issuer,omitempty"`
}

func formatTransaction(tx map[string]any) txnOut {
	return txnOut{
		Amount: cleanAmount(mget(tx, "amount")),
		Status: statusName(mget(tx, "status")),
		Time:   formatWIB(time.Unix(numOf(mget(tx, "createTime")), 0)),
	}
}

// ---------------------------------------------------------------- HTTP utils

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "error": msg})
}

// decodeBody handles express.json() and express.urlencoded({extended:true}).
func decodeBody(w http.ResponseWriter, r *http.Request) map[string]any {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err == nil {
			out := map[string]any{}
			for k, v := range r.PostForm {
				if len(v) > 0 {
					out[k] = v[0]
				}
			}
			return out
		}
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return map[string]any{}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := decodeUseNumber(b, &out); err == nil && out != nil {
		return out
	}
	out = map[string]any{}
	if vals, err := url.ParseQuery(string(b)); err == nil {
		for k, v := range vals {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- middleware

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// cors({ origin: "*", credentials: true })
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE")
		reqHeaders := r.Header.Get("Access-Control-Request-Headers")
		if reqHeaders == "" {
			reqHeaders = "Content-Type, X-API-Key, X-Shopee-Token"
		}
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// express.json({ limit: "1mb" })
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}
		if key == "" || key != s.apiKey {
			errJSON(w, http.StatusUnauthorized, "Invalid or missing API key")
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------- shopee client

func randomUA() string {
	uas := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0",
		"Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:127.0) Gecko/20100101 Firefox/127.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.0.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	}
	return uas[mathrand.IntN(len(uas))]
}

// postShopee mirrors axios.post(); non-2xx is an error, and the timeout message
// is kept identical to axios so downstream error strings match.
func (s *server) postShopee(endpoint string, payload map[string]any, timeout time.Duration) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://partner.shopee.co.id")
	req.Header.Set("Referer", "https://partner.shopee.co.id/")
	req.Header.Set("User-Agent", randomUA())
	req.Header.Set("X-Timestamp-Ms", strconv.FormatInt(time.Now().UnixMilli(), 10))

	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, fmt.Errorf("timeout of %dms exceeded", timeout.Milliseconds())
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Request failed with status code %d", resp.StatusCode)
	}
	var out map[string]any
	if err := decodeUseNumber(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *server) callShopeeList(startTime, endTime, pageSize int64, nextPos, token string) (map[string]any, error) {
	np := nextPos
	if np == "" {
		np = ""
	}
	payload := map[string]any{
		"data": map[string]any{
			"metadata": map[string]any{
				"token":    token,
				"language": "id",
				"timezone": "Asia/Jakarta",
			},
			"pageSize":      pageSize,
			"filter":        map[string]any{"startTime": startTime, "endTime": endTime, "serviceList": []int{1, 3}},
			"sorter":        map[string]any{"field": "createTime", "order": "descend"},
			"next_position": np,
		},
	}
	return s.postShopee(s.listURL, payload, 20*time.Second)
}

func (s *server) callShopeeDetail(orderSN, token string) (map[string]any, error) {
	payload := map[string]any{
		"data": map[string]any{
			"metadata": map[string]any{
				"token":    token,
				"language": "id",
				"timezone": "Asia/Jakarta",
			},
			"order_sn": orderSN,
		},
	}
	return s.postShopee(s.detailURL, payload, 20*time.Second)
}

// ---------------------------------------------------------------- telegram

func (s *server) sendTelegramNotif(message string) {
	if s.telegramBotToken == "" || s.telegramChatID == "" {
		fmt.Println("[TELEGRAM] Bot token atau chat ID belum diset")
		return
	}
	payload := map[string]any{
		"chat_id":    s.telegramChatID,
		"text":       message,
		"parse_mode": "HTML",
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post("https://api.telegram.org/bot"+s.telegramBotToken+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("[TELEGRAM] Send failed:", err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	fmt.Println("[TELEGRAM] Notif sent")
}

// ---------------------------------------------------------------- token checker

func (s *server) checkToken() {
	now := time.Now().Unix()
	start, end := now-3600, now

	result, err := s.callShopeeList(start, end, 1, "", s.getToken())
	if err != nil {
		s.logEvent("ERROR", "Token check failed: "+err.Error())
		s.setTokenState(false)
		s.notifyOnce("⚠️ <b>Shopee API</b>\n\nToken error: " + err.Error() +
			"\n\nUpdate token via POST /update-token")
		return
	}
	if result == nil || numOf(mget(result, "code")) != 0 {
		msg := "Invalid response format"
		if result != nil {
			if m, ok := mget(result, "msg").(string); ok {
				msg = m
			} else if mget(result, "msg") != nil {
				msg = numToString(mget(result, "msg"))
			}
		}
		s.logEvent("ERROR", "Token invalid: "+msg)
		s.setTokenState(false)
		s.notifyOnce("⚠️ <b>Shopee API</b>\n\nToken invalid: " + msg +
			"\n\nUpdate token via POST /update-token")
		return
	}
	s.logEvent("INFO", "Token valid")
	s.setTokenState(true)
}

func (s *server) setTokenState(valid bool) {
	s.mu.Lock()
	s.tokenValid = valid
	s.mu.Unlock()
}

// notifyOnce mirrors the `!tokenNotifSent && (send, tokenNotifSent = true)`
// guard, which is reset to false whenever the token comes back valid.
func (s *server) notifyOnce(msg string) {
	s.mu.Lock()
	already := s.tokenNotif
	s.tokenNotif = true
	s.mu.Unlock()
	if already {
		return
	}
	go s.sendTelegramNotif(msg)
}

func (s *server) startTokenChecker() {
	go func() {
		s.checkToken()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.checkToken()
			s.mu.Lock()
			s.tokenNotif = false
			s.mu.Unlock()
		}
	}()
}

func (s *server) tokenStatus() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenValid
}

// ---------------------------------------------------------------- QRIS (EMVCo TLV)

type tlvField struct {
	tag   string
	value string
}

func parseTLV(data string) []tlvField {
	var out []tlvField
	i := 0
	for i < len(data) {
		if i+4 > len(data) {
			break
		}
		tag := data[i : i+2]
		length := parseIntPrefix(data[i+2 : i+4])
		if length == 0 && strings.Trim(data[i+2:i+4], "0") != "" {
			break // NaN length
		}
		i += 4
		if i+int(length) > len(data) {
			break
		}
		out = append(out, tlvField{tag, data[i : i+int(length)]})
		i += int(length)
	}
	return out
}

func buildTLV(fields []tlvField) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f.tag)
		b.WriteString(fmt.Sprintf("%02d", len(f.value)))
		b.WriteString(f.value)
	}
	return b.String()
}

func crc16CCITT(data string) string {
	crc := 0xFFFF
	for i := 0; i < len(data); i++ {
		crc ^= int(data[i]) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = ((crc << 1) ^ 0x1021) & 0xFFFF
			} else {
				crc = (crc << 1) & 0xFFFF
			}
		}
	}
	return strings.ToUpper(fmt.Sprintf("%04X", crc))
}

func generateDynamicQRIS(staticQRIS string, amount int64) (string, error) {
	if staticQRIS == "" {
		return "", errors.New("QRIS_STATIC belum diset di .env")
	}
	fields := parseTLV(staticQRIS)
	if len(fields) == 0 {
		return "", errors.New("invalid QRIS format")
	}

	amt := strconv.FormatInt(amount, 10)
	var out []tlvField
	hasAmount := false
	for _, f := range fields {
		if f.tag == "63" {
			continue // drop CRC, recomputed below
		}
		if f.tag == "54" {
			out = append(out, tlvField{"54", amt})
			hasAmount = true
			continue
		}
		out = append(out, f)
	}
	if !hasAmount {
		withAmount := make([]tlvField, 0, len(out)+1)
		for _, f := range out {
			withAmount = append(withAmount, f)
			if f.tag == "53" {
				withAmount = append(withAmount, tlvField{"54", amt})
			}
		}
		out = withAmount
	}

	body := buildTLV(out) + "6304"
	return body + crc16CCITT(body), nil
}

// ---------------------------------------------------------------- handlers

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "Shoppe API Running")
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"message":   "ShopeePay API Service is running",
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

func (s *server) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(w, r)
	tok := mget(body, "token")
	if tok == nil || numToString(tok) == "" {
		errJSON(w, http.StatusBadRequest, "Provide token in body")
		return
	}
	s.setToken(numToString(tok))
	s.logEvent("INFO", "Token updated via API")
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"message": "Token updated"},
	})
}

func (s *server) handleTokenStatus(w http.ResponseWriter, r *http.Request) {
	valid := s.tokenStatus()
	status, msg := "valid", "Token is working"
	if !valid {
		status, msg = "invalid", "Token expired/invalid. Please update via POST /update-token"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": valid,
		"data":    map[string]any{"token_status": status, "message": msg},
	})
}

func (s *server) handleCreateQRIS(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(w, r)
	amountRaw := mget(body, "amount")
	amount, ok := floatOf(amountRaw)
	if !ok || amount == 0 || amount <= 0 {
		errJSON(w, http.StatusBadRequest, "Provide valid amount (positive integer)")
		return
	}

	qris, err := generateDynamicQRIS(s.qrisStatic, int64(amount))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	var b [4]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		s.logEvent("ERROR", "crypto/rand failed: "+err.Error())
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := hex.EncodeToString(b[:])
	expiresAt := time.Now().Add(15 * time.Minute)

	s.qrisMu.Lock()
	s.qrisSt[id] = qrisEntry{data: qris, expiresAt: expiresAt}
	s.qrisMu.Unlock()

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"qris_url":   scheme + "://" + r.Host + "/qr/" + id,
			"amount":     int64(amount),
			"expires_at": formatWIB(expiresAt),
			"expires_in": "15 menit",
		},
	})
}

func (s *server) handleQR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.qrisMu.Lock()
	entry, found := s.qrisSt[id]
	expired := found && time.Now().After(entry.expiresAt)
	if expired {
		delete(s.qrisSt, id)
	}
	s.qrisMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case !found:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "QR not found")
	case expired:
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, "QR expired")
	default:
		http.Redirect(w, r, qrImageAPI+url.QueryEscape(entry.data), http.StatusFound)
	}
}

func (s *server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	q := r.URL.Query()

	startTime := parseIntPrefix(q.Get("startTime"))
	endTime := parseIntPrefix(q.Get("endTime"))
	pageSize := parseIntPrefix(q.Get("pageSize"))
	nextPos := q.Get("next_position")

	if startTime == 0 {
		startTime = now - 3*24*3600
	}
	if endTime == 0 {
		endTime = now
	}
	if pageSize == 0 {
		pageSize = 10
	}

	token := s.reqToken(r)
	result, err := s.callShopeeList(startTime, endTime, pageSize, nextPos, token)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result == nil {
		errJSON(w, http.StatusInternalServerError, "Empty response from ShopeePay API")
		return
	}
	if numOf(mget(result, "code")) != 0 {
		msg := numToString(mget(result, "msg"))
		if msg == "undefined" || msg == "" {
			msg = "API error code " + numToString(mget(result, "code"))
		}
		errJSON(w, http.StatusBadRequest, msg)
		return
	}

	data := mmap(result, "data")
	formatted := make([]txnOut, 0)
	for _, item := range mlist(data, "list") {
		tx, ok := item.(map[string]any)
		if !ok {
			continue
		}
		trx := formatTransaction(tx)
		s.fillIssuer(&trx, tx, token, "ERROR")
		formatted = append(formatted, trx)
	}

	writeJSON(w, http.StatusOK, struct {
		Success     bool            `json:"success"`
		TotalAmount json.RawMessage `json:"total_amount"`
		Data        struct {
			Transactions []txnOut `json:"transactions"`
		} `json:"data"`
	}{true, totalSales(data), struct {
		Transactions []txnOut `json:"transactions"`
	}{formatted}})
}

// fillIssuer runs the best-effort issuer lookup, logging at warnLevel.
func (s *server) fillIssuer(t *txnOut, tx map[string]any, token, warnLevel string) {
	id := numToString(mget(tx, "displayTransactionId"))
	if id == "undefined" || id == "" {
		id = numToString(mget(tx, "transactionId"))
	}
	detail, err := s.callShopeeDetail(id, token)
	if err != nil {
		s.logEvent(warnLevel, "Gagal mengambil detail transaksi "+
			numToString(mget(tx, "transactionId"))+": "+err.Error())
		return
	}
	if detail == nil || numOf(mget(detail, "code")) != 0 {
		return
	}
	d := mmap(detail, "data")
	if d == nil {
		return
	}
	if iss, ok := d["issuer"]; ok {
		str := numToString(iss)
		t.Issuer = &str
	}
}

// totalSales mirrors (data && data.totalNetSales) || "0"
func totalSales(data map[string]any) json.RawMessage {
	v := mget(data, "totalNetSales")
	switch t := v.(type) {
	case json.Number:
		if t.String() == "0" {
			return json.RawMessage(`"0"`)
		}
		return json.RawMessage(t.String())
	case string:
		if t == "" || t == "0" {
			return json.RawMessage(`"0"`)
		}
		b, _ := json.Marshal(t)
		return b
	case float64:
		if t == 0 {
			return json.RawMessage(`"0"`)
		}
		return json.RawMessage(strconv.FormatFloat(t, 'f', -1, 64))
	}
	return json.RawMessage(`"0"`)
}

func (s *server) handleTransactionsAll(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	wibNow := now.UTC().Add(wib)
	year, month, _ := wibNow.Date()
	startOfMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Add(-wib)
	startTime, endTime := startOfMonth.Unix(), now.Unix()

	token := s.reqToken(r)
	const pageSize = 100

	all := make([]txnOut, 0)
	nextPos := ""

	for {
		result, err := s.callShopeeList(startTime, endTime, pageSize, nextPos, token)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if result == nil {
			s.logEvent("ERROR", "Respons dari ShopeePay API kosong.")
			errJSON(w, http.StatusInternalServerError, "Empty response from ShopeePay API")
			return
		}
		if numOf(mget(result, "code")) != 0 {
			msg := numToString(mget(result, "msg"))
			if msg == "undefined" || msg == "" {
				msg = "API error code " + numToString(mget(result, "code"))
			}
			errJSON(w, http.StatusBadRequest, msg)
			return
		}

		data := mmap(result, "data")
		list := mlist(data, "list")
		for _, item := range list {
			tx, ok := item.(map[string]any)
			if !ok {
				continue
			}
			trx := formatTransaction(tx)
			s.fillIssuer(&trx, tx, token, "ERROR")
			all = append(all, trx)
		}

		np := numToString(mget(data, "next_position"))
		if data == nil || np == "undefined" || np == "" || len(list) < pageSize {
			break
		}
		nextPos = np
		time.Sleep(500 * time.Millisecond)
	}

	period := formatWIBDate(startOfMonth) + " s/d " + formatWIBDate(now)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"total_amount": strconv.Itoa(len(all)),
		"data": map[string]any{
			"period":       period,
			"total_count":  len(all),
			"transactions": all,
		},
	})
}

func (s *server) handleCheckPayment(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(w, r)
	amountRaw := mget(body, "amount")

	amount, ok := floatOf(amountRaw)
	if !ok || amount == 0 || amount <= 0 {
		errJSON(w, http.StatusBadRequest, "Provide valid amount (positive integer)")
		return
	}

	token := s.reqToken(r)
	nowUnix := time.Now().Unix()

	startUnix := parseIntPrefix(numToString(mget(body, "startTime")))
	if startUnix == 0 {
		startUnix = nowUnix - 30*60
	}

	s.logEvent("INFO", "Memulai pengecekan pembayaran stateless. Nominal: Rp "+
		numToString(amountRaw)+", Waktu Mulai: "+
		time.Unix(startUnix, 0).UTC().Format("2006-01-02T15:04:05.000Z"))

	result, err := s.callShopeeList(startUnix, nowUnix, 50, "", token)
	if err != nil {
		s.logEvent("ERROR", "Pengecekan gagal karena exception: "+err.Error())
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result == nil {
		s.logEvent("ERROR", "Respons dari ShopeePay API kosong.")
		errJSON(w, http.StatusInternalServerError, "Empty response from ShopeePay API")
		return
	}
	if numOf(mget(result, "code")) != 0 {
		code := numToString(mget(result, "code"))
		msg := numToString(mget(result, "msg"))
		s.logEvent("ERROR", "API ShopeePay mengembalikan kode "+code+": "+msg)
		if msg == "undefined" || msg == "" {
			msg = "API error code " + code
		}
		errJSON(w, http.StatusBadRequest, msg)
		return
	}

	var match map[string]any
	for _, item := range mlist(mmap(result, "data"), "list") {
		tx, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		if numOf(mget(tx, "status")) != 3 {
			continue
		}
		if float64(cleanAmount(mget(tx, "amount"))) != amount {
			continue
		}
		if numOf(mget(tx, "createTime")) < startUnix {
			continue
		}
		if s.dedupSeen(txnID(tx)) {
			continue
		}
		match = tx
		break
	}

	if match == nil {
		s.logEvent("INFO", "Pengecekkan selesai. Nominal Rp "+numToString(amountRaw)+" BELUM ditemukan.")
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "paid": false})
		return
	}

	// Claim the transaction before returning so a concurrent identical poll
	// cannot claim the same receipt twice.
	id := txnID(match)
	s.dedupClaim(id, numOf(mget(match, "createTime")))
	s.dedupGC(time.Now().Unix() - 24*3600)

	s.logEvent("INFO", "Pencocokan berhasil! Transaksi ditemukan: "+id+". Mengambil detail...")

	trx := formatTransaction(match)
	s.fillIssuer(&trx, match, token, "WARN")

	issuer := "QRIS / ShopeePay"
	if trx.Issuer != nil {
		issuer = *trx.Issuer
	}
	s.logEvent("INFO", "Pembayaran terverifikasi lunas via "+issuer+".")

	writeJSON(w, http.StatusOK, struct {
		Success     bool `json:"success"`
		Paid        bool `json:"paid"`
		Transaction *struct {
			TransactionID string `json:"transactionId"`
			Amount        int64  `json:"amount"`
			Status        string `json:"status"`
			Time          string `json:"time"`
			Issuer        string `json:"issuer"`
		} `json:"transaction,omitempty"`
	}{true, true, &struct {
		TransactionID string `json:"transactionId"`
		Amount        int64  `json:"amount"`
		Status        string `json:"status"`
		Time          string `json:"time"`
		Issuer        string `json:"issuer"`
	}{id, trx.Amount, trx.Status, trx.Time, issuer}})
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"logs": s.snapshotLogs()},
	})
}

func (s *server) reqToken(r *http.Request) string {
	if t := r.Header.Get("X-Shopee-Token"); t != "" {
		return t
	}
	return s.getToken()
}

// ---------------------------------------------------------------- dedup

func (s *server) dedupSeen(id string) bool {
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	_, ok := s.dedup[id]
	return ok
}

func (s *server) dedupClaim(id string, createTime int64) {
	s.dedupMu.Lock()
	s.dedup[id] = createTime
	s.dedupMu.Unlock()
}

func (s *server) dedupGC(cutoff int64) {
	s.dedupMu.Lock()
	for k, v := range s.dedup {
		if v < cutoff {
			delete(s.dedup, k)
		}
	}
	s.dedupMu.Unlock()
}

// ---------------------------------------------------------------- main

func main() {
	loadEnvFile(".env")
	s := newServer()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /update-token", s.auth(s.handleUpdateToken))
	mux.HandleFunc("GET /token-status", s.auth(s.handleTokenStatus))
	mux.HandleFunc("POST /create-qris", s.auth(s.handleCreateQRIS))
	mux.HandleFunc("GET /qr/{id}", s.handleQR)
	mux.HandleFunc("GET /transactions", s.auth(s.handleTransactions))
	mux.HandleFunc("GET /transactions/all", s.auth(s.handleTransactionsAll))
	mux.HandleFunc("POST /check-payment", s.auth(s.handleCheckPayment))
	mux.HandleFunc("GET /api/logs", s.auth(s.handleLogs))

	srv := &http.Server{
		Addr:              ":" + s.port,
		Handler:           s.middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("ShopeePay API (go) running at http://localhost:%s", s.port)
	fmt.Println("Endpoints:")
	fmt.Println("  POST /update-token       - Update ShopeeToken")
	fmt.Println("  GET  /token-status       - Check ShopeeToken Validity status")
	fmt.Println("  POST /create-qris        - Generate Dynamic QRIS from static template")
	fmt.Println("  GET  /qr/:id             - Fetch Dynamic QRIS Image Redirect")
	fmt.Println("  GET  /transactions       - Fetch transactions list")
	fmt.Println("  GET  /transactions/all   - Fetch all transactions of the month")

	if s.getToken() != "" && s.apiKey != "" {
		s.startTokenChecker()
	} else {
		fmt.Println("WARNING: SHOPEE_TOKEN and API_KEY must be set in .env to run checks.")
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
