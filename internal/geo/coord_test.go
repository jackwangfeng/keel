package geo

import (
	"math"
	"testing"
)

// 天安门：WGS-84 (39.908692, 116.397477) 在 GCJ-02 下约偏 (+0.0014, +0.0062)（各家公开样例一致）。
func TestGCJRoundTrip(t *testing.T) {
	lat, lng := 39.908692, 116.397477
	gLat, gLng := WGS84ToGCJ02(lat, lng)
	if math.Abs(gLat-lat-0.00140) > 0.0002 || math.Abs(gLng-lng-0.00624) > 0.0002 {
		t.Fatalf("WGS→GCJ 偏移不对：(%v, %v)", gLat-lat, gLng-lng)
	}
	bLat, bLng := GCJ02ToWGS84(gLat, gLng)
	if math.Abs(bLat-lat) > 1e-7 || math.Abs(bLng-lng) > 1e-7 {
		t.Fatalf("往返误差过大：(%v, %v)", bLat-lat, bLng-lng)
	}
	// 国外不偏移。
	if a, b := WGS84ToGCJ02(51.5, -0.12); a != 51.5 || b != -0.12 {
		t.Fatal("伦敦不该被偏移")
	}
}
