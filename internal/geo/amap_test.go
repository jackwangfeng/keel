package geo

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 高德客户端对一个假的高德：进出坐标换算、「没有值给 []」、status 0 的错误、没坐标的提示被丢掉、缓存。
func TestAmap(t *testing.T) {
	calls := 0
	var gotLoc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("key") != "k" {
			w.Write([]byte(`{"status":"0","info":"INVALID_USER_KEY"}`))
			return
		}
		gotLoc = r.URL.Query().Get("location")
		switch r.URL.Path {
		case "/v3/geocode/regeo":
			w.Write([]byte(`{"status":"1","regeocode":{"formatted_address":"北京市东城区东华门街道天安门",
				"addressComponent":{"province":"北京市","city":[],"district":"东城区","adcode":"110101","township":"东华门街道",
				"streetNumber":{"street":[],"number":[]},"neighborhood":{"name":"天安门","type":"风景名胜"}}}}`))
		case "/v3/assistant/inputtips":
			w.Write([]byte(`{"status":"1","tips":[
				{"name":"望京SOHO","district":"北京市朝阳区","adcode":"110105","address":"阜通东大街","location":"116.480983,39.996539"},
				{"name":"望京-西直门","district":[],"adcode":[],"address":[],"location":[]}]}`))
		}
	}))
	defer srv.Close()
	a := &Amap{Key: "k", Base: srv.URL, Client: srv.Client()}
	p := NewCached(a)

	pl, err := p.Reverse(context.Background(), 39.908692, 116.397477)
	if err != nil {
		t.Fatal(err)
	}
	if pl.City != "北京市" || pl.District != "东城区" || pl.Adcode != "110101" || pl.Name != "天安门" || pl.Street != "" {
		t.Fatalf("逆地理编码：%+v", pl)
	}
	// 出去的坐标是 GCJ-02（lng,lat）：比 WGS-84 偏约 +0.0062 / +0.0014。
	parts := strings.Split(gotLoc, ",")
	lng, _ := strconv.ParseFloat(parts[0], 64)
	if math.Abs(lng-116.397477-0.00624) > 0.0003 {
		t.Fatalf("发给高德的坐标没换成 GCJ-02：%s", gotLoc)
	}
	// 缓存：同一坐标第二次不打出去。
	before := calls
	if _, err := p.Reverse(context.Background(), 39.908692, 116.397477); err != nil || calls != before {
		t.Fatalf("同一坐标第二次打了高德（calls %d → %d）", before, calls)
	}

	tips, err := p.Suggest(context.Background(), "望京", 0, 0, "北京")
	if err != nil {
		t.Fatal(err)
	}
	if len(tips) != 1 || tips[0].Name != "望京SOHO" || tips[0].Adcode != "110105" {
		t.Fatalf("输入提示：%+v", tips)
	}
	// 回来的坐标换回 WGS-84：比高德给的 GCJ-02 小约 0.0062 / 0.0014。
	if math.Abs(116.480983-tips[0].Lng-0.0062) > 0.0005 || math.Abs(39.996539-tips[0].Lat-0.0014) > 0.0005 {
		t.Fatalf("输入提示的坐标没换回 WGS-84：%v,%v", tips[0].Lat, tips[0].Lng)
	}

	bad := &Amap{Key: "wrong", Base: srv.URL, Client: srv.Client()}
	if _, err := bad.Reverse(context.Background(), 39.9, 116.4); !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "INVALID_USER_KEY") {
		t.Fatalf("key 错应是 ErrUpstream 并带 info：%v", err)
	}
}

func TestFromEnv(t *testing.T) {
	if p, err := FromEnv("", ""); p != nil || err != nil {
		t.Fatal("没配应返回 nil, nil")
	}
	if _, err := FromEnv("baidu", "k"); err == nil {
		t.Fatal("不认识的服务商应报错")
	}
	if p, err := FromEnv("AMAP", "k"); err != nil || p.Name() != "amap" {
		t.Fatalf("amap：%v %v", p, err)
	}
}
