package num

import (
	"encoding/json"
	"testing"
)

func TestParseAndFormat(t *testing.T) {
	for in, want := range map[string]string{
		"0.00055000": "0.00055", "80556.00000000": "80556", "-12.5": "-12.5", ".5": "0.5", "7": "7", "0.00000001": "0.00000001", "0": "0",
	} {
		d, err := Parse(in)
		if err != nil || d.String() != want {
			t.Fatalf("%s: got %q %v, want %q", in, d.String(), err, want)
		}
	}
	for _, bad := range []string{"", "1e5", "0.000000001", "abc", "--1", "1.2.3", "99999999999", "."} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestArithmeticIsExact(t *testing.T) {
	qty, price := MustParse("0.00055"), MustParse("80556.01")
	if got := qty.Mul(price).String(); got != "44.3058055" {
		t.Fatalf("0.00055 × 80556.01 = %s", got)
	}
	if got := MustParse("45").Div(price).Floor(MustParse("0.00001")).String(); got != "0.00055" {
		t.Fatalf("$45 of BTC at 80556.01 in 0.00001 steps = %s", got)
	}
	if got := MustParse("0.1").Add(MustParse("0.2")).String(); got != "0.3" {
		t.Fatalf("0.1 + 0.2 = %s", got)
	}
	if got := MustParse("-0.000015").Floor(MustParse("0.00001")).String(); got != "-0.00002" {
		t.Fatalf("floor of a negative value = %s", got)
	}
	if got := MustParse("45.129").Fixed(2); got != "45.12" {
		t.Fatalf("Fixed(2) = %s", got)
	}
	if got := MustParse("45").Fixed(2); got != "45.00" {
		t.Fatalf("Fixed(2) = %s", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var v struct{ A, B Dec }
	if err := json.Unmarshal([]byte(`{"A":"0.12345678","B":3.5}`), &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	if string(out) != `{"A":"0.12345678","B":"3.5"}` {
		t.Fatalf("got %s", out)
	}
}

func TestOverflowPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an out-of-range product must panic, never wrap")
		}
	}()
	big := MustParse("9999999999")
	big.Mul(big)
}
