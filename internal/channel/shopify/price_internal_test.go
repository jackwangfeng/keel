package shopify

import "testing"

func TestParsePriceCents(t *testing.T) {
	ok := map[string]int64{"949.95": 94995, "10": 1000, "0.5": 50, "10.00": 1000, "1.500": 150, ".25": 25, "0": 0}
	for in, want := range ok {
		got, err := parsePriceCents(in)
		if err != nil || got != want {
			t.Errorf("parsePriceCents(%q) = %d, %v；期望 %d", in, got, err, want)
		}
	}
	for _, in := range []string{"1.005", "-1", "", "abc", "1.2.3", "+5"} {
		if _, err := parsePriceCents(in); err == nil {
			t.Errorf("parsePriceCents(%q) 应当报错", in)
		}
	}
	if formatCents(1234) != "12.34" || formatCents(5) != "0.05" || formatCents(100) != "1.00" {
		t.Fatal("formatCents 不对")
	}
}

func TestIdemKeyStableAndMembershipSensitive(t *testing.T) {
	a := idemKey([]string{"1:2:3:4", "1:2:5:1"})
	if a != idemKey([]string{"1:2:5:1", "1:2:3:4"}) {
		t.Fatal("同一组键顺序不同得到了不同的幂等键")
	}
	if a == idemKey([]string{"1:2:3:4"}) {
		t.Fatal("组成员不同得到了相同的幂等键")
	}
}
