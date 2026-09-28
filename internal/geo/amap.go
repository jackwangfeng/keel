package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Amap 是高德 Web 服务 API（逆地理编码 v3/geocode/regeo、输入提示 v3/assistant/inputtips）。
// key 只在服务端（绑服务器 IP 白名单），不下发给任何客户端。高德坐标是 GCJ-02：进出都在这里换算。
type Amap struct {
	Key    string
	Base   string // 默认 https://restapi.amap.com，测试时指向 httptest
	Client *http.Client
}

func NewAmap(key string) *Amap {
	return &Amap{Key: key, Base: "https://restapi.amap.com", Client: &http.Client{Timeout: 5 * time.Second}}
}

func (a *Amap) Name() string { return "amap" }

// amapString 兼容高德「没有值时给空数组 []」的习惯（city、street 等字段）。
type amapString string

func (s *amapString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = amapString(v)
		return nil
	}
	*s = ""
	return nil
}

func (a *Amap) get(ctx context.Context, path string, q url.Values, out any) error {
	q.Set("key", a.Key)
	q.Set("output", "JSON")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrUpstream, resp.StatusCode)
	}
	var head struct {
		Status string `json:"status"`
		Info   string `json:"info"`
	}
	raw := json.NewDecoder(resp.Body)
	var body json.RawMessage
	if err := raw.Decode(&body); err != nil {
		return fmt.Errorf("%w: 响应不是 JSON：%v", ErrUpstream, err)
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Status != "1" {
		// status 0：key 不对、额度用完（DAILY_QUERY_OVER_LIMIT）、IP 不在白名单等 —— info 原样带出来便于排查。
		return fmt.Errorf("%w: 高德返回 %s", ErrUpstream, head.Info)
	}
	return json.Unmarshal(body, out)
}

func gcjParam(lat, lng float64) string {
	gLat, gLng := WGS84ToGCJ02(lat, lng)
	return strconv.FormatFloat(gLng, 'f', 6, 64) + "," + strconv.FormatFloat(gLat, 'f', 6, 64)
}

// parseLocation 解析高德的 "lng,lat"（GCJ-02），转成 WGS-84。
func parseLocation(s string) (float64, float64, bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lng, err1 := strconv.ParseFloat(parts[0], 64)
	lat, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	wLat, wLng := GCJ02ToWGS84(lat, lng)
	return wLat, wLng, true
}

func (a *Amap) Reverse(ctx context.Context, lat, lng float64) (Place, error) {
	var r struct {
		Regeocode struct {
			FormattedAddress amapString `json:"formatted_address"`
			AddressComponent struct {
				Province     amapString `json:"province"`
				City         amapString `json:"city"`
				District     amapString `json:"district"`
				Adcode       amapString `json:"adcode"`
				Township     amapString `json:"township"`
				StreetNumber struct {
					Street amapString `json:"street"`
					Number amapString `json:"number"`
				} `json:"streetNumber"`
				Neighborhood struct {
					Name amapString `json:"name"`
				} `json:"neighborhood"`
			} `json:"addressComponent"`
		} `json:"regeocode"`
	}
	if err := a.get(ctx, "/v3/geocode/regeo", url.Values{"location": {gcjParam(lat, lng)}, "extensions": {"base"}}, &r); err != nil {
		return Place{}, err
	}
	c := r.Regeocode.AddressComponent
	city := string(c.City)
	if city == "" {
		city = string(c.Province) // 直辖市高德给空城市
	}
	name := string(c.Neighborhood.Name)
	if name == "" {
		name = string(c.StreetNumber.Street) + string(c.StreetNumber.Number)
	}
	if name == "" {
		name = string(c.Township)
	}
	return Place{Name: name, Address: string(r.Regeocode.FormattedAddress), Province: string(c.Province), City: city,
		District: string(c.District), Adcode: string(c.Adcode), Street: string(c.StreetNumber.Street), Lat: lat, Lng: lng}, nil
}

func (a *Amap) Suggest(ctx context.Context, q string, lat, lng float64, city string) ([]Place, error) {
	params := url.Values{"keywords": {q}, "datatype": {"all"}}
	if lat != 0 || lng != 0 {
		params.Set("location", gcjParam(lat, lng))
	}
	if city != "" {
		params.Set("city", city)
	}
	var r struct {
		Tips []struct {
			Name     amapString `json:"name"`
			District amapString `json:"district"`
			Adcode   amapString `json:"adcode"`
			Address  amapString `json:"address"`
			Location amapString `json:"location"`
		} `json:"tips"`
	}
	if err := a.get(ctx, "/v3/assistant/inputtips", params, &r); err != nil {
		return nil, err
	}
	out := make([]Place, 0, len(r.Tips))
	for _, t := range r.Tips {
		wLat, wLng, ok := parseLocation(string(t.Location))
		if !ok {
			continue // 公交线路之类没有坐标的提示：选不了点，丢掉
		}
		out = append(out, Place{Name: string(t.Name), Address: string(t.District) + string(t.Address),
			District: string(t.District), Adcode: string(t.Adcode), Lat: wLat, Lng: wLng})
	}
	return out, nil
}
