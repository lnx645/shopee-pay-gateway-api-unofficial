package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

// static QRIS pinned so the golden values below stay stable regardless of
// what QRIS_STATIC in the local .env currently holds.
const testQRISStatic = "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI5204572253033605802ID5911NUXYS STORE6009TANGERANG61051511262070703A0163047034"

// Golden values produced by the original JS implementation
// (parseTLV/buildTLV/crc16CCITT/generateDynamicQRIS transcribed from the
// decoded server.js, run under Bun). Regenerate with qris-ref.js.
var qrisGolden = map[int64]string{
	1:         "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI520457225303360540115802ID5911NUXYS STORE6009TANGERANG61051511262070703A016304B9A3",
	99:        "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI5204572253033605402995802ID5911NUXYS STORE6009TANGERANG61051511262070703A0163044E6A",
	100:       "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI52045722530336054031005802ID5911NUXYS STORE6009TANGERANG61051511262070703A0163040018",
	1008:      "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI520457225303360540410085802ID5911NUXYS STORE6009TANGERANG61051511262070703A0163047D3F",
	15000:     "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI5204572253033605405150005802ID5911NUXYS STORE6009TANGERANG61051511262070703A01630424A6",
	1234567:   "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI520457225303360540712345675802ID5911NUXYS STORE6009TANGERANG61051511262070703A0163046E4C",
	100000000: "00020101021126610016ID.CO.SHOPEE.WWW01189360091800227615670208227615670303UMI51440014ID.CO.QRIS.WWW0215ID10264876014560303UMI52045722530336054091000000005802ID5911NUXYS STORE6009TANGERANG61051511262070703A016304BF6E",
}

// testStaticQRIS reads QRIS_STATIC from .env applying the same quote handling
// dotenv does, so it matches what the server loads at boot. "" when absent.
func testStaticQRIS(t *testing.T) string {
	t.Helper()
	f, err := os.Open(".env")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "QRIS_STATIC=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "QRIS_STATIC="))
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}

func TestGenerateDynamicQRISMatchesJS(t *testing.T) {
	for amount, want := range qrisGolden {
		got, err := generateDynamicQRIS(testQRISStatic, amount)
		if err != nil {
			t.Fatalf("amount %d: %v", amount, err)
		}
		if got != want {
			t.Errorf("amount %d mismatch\n got: %s\nwant: %s", amount, got, want)
		}
	}
}

func TestGenerateDynamicQRISErrors(t *testing.T) {
	if _, err := generateDynamicQRIS("", 1000); err == nil ||
		err.Error() != "QRIS_STATIC belum diset di .env" {
		t.Errorf("empty static: got %v", err)
	}
	if _, err := generateDynamicQRIS("garbage", 1000); err == nil ||
		err.Error() != "invalid QRIS format" {
		t.Errorf("garbage static: got %v", err)
	}
}

func TestParseTLVMatchesJS(t *testing.T) {
	// expectations captured from the original JS parseTLV (see qris-ref.js)
	cases := []struct {
		in   string
		want []tlvField
	}{
		{"520457225303", []tlvField{{"52", "5722"}}}, // trailing TLV cut short
		{"54ab1000", nil},                              // non-numeric length -> NaN -> stop
		{"540115802", []tlvField{{"54", "1"}}},         // value then truncated next TLV
		{"5303UMI", []tlvField{{"53", "UMI"}}},         // currency code
		{"5303UMI51440014", []tlvField{{"53", "UMI"}}}, // next length overruns buffer
		{"6304", nil}, // header + length with no value
		{"5401", nil}, // length overruns buffer
	}
	for _, c := range cases {
		got := parseTLV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseTLV(%q) = %d fields %+v, want %d %+v",
				c.in, len(got), got, len(c.want), c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseTLV(%q)[%d] = %+v, want %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestCRC16CCITT(t *testing.T) {
	if got := crc16CCITT("123456789"); got != "29B1" {
		t.Errorf("CRC-16/CCITT-FALSE check value = %s, want 29B1", got)
	}
}

func TestCleanAmount(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{nil, 0},
		{"9.600", 9600},
		{"10,500", 10500},
		{"1008", 1008},
		{float64(9600), 9600},
		{"abc", 0},
		{"", 0},
		{"9600xyz", 9600},
	}
	for _, c := range cases {
		if got := cleanAmount(c.in); got != c.want {
			t.Errorf("cleanAmount(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStatusName(t *testing.T) {
	cases := map[int64]string{1: "pending", 2: "failed", 3: "success", 4: "refunded", 5: "expired", 9: "unknown_9"}
	for in, want := range cases {
		if got := statusName(jsonNumber(in)); got != want {
			t.Errorf("statusName(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestParseIntPrefix(t *testing.T) {
	cases := map[string]int64{"": 0, "abc": 0, "12ab": 12, "-5": -5, " 42": 42, "0": 0, "007": 7}
	for in, want := range cases {
		if got := parseIntPrefix(in); got != want {
			t.Errorf("parseIntPrefix(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestFormatWIB(t *testing.T) {
	// 1784050000 == 2026-07-14 17:26:40 UTC == 2026-07-15 00:26:40 WIB
	ts, _ := strconv.ParseInt("1784050000", 10, 64)
	if got := formatWIB(secTime(ts)); got != "2026-07-15 00:26:40" {
		t.Errorf("formatWIB = %s", got)
	}
}
