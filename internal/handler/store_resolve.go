package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// GET /stores/resolve 单独一个文件：它读 {lat, lng, size}，而 GET /stores
// 读 {page, page_size}，参数对账按文件做（store.go 的文件头写了推理）。

// Resolve 实现 GET /api/v1/stores/resolve。
//
// ===========================================================================
// 三种结果全走 200，包括「不在服务范围」
// ===========================================================================
//
// match_type = none 不是错误，是一个正常的查询结果。用 404 表达它会让客户端的
// 错误分支同时装着「网络失败」「鉴权失败」和「这个地方我们不送」，
// 而第三种要渲染的是一个完全不同的页面。
//
// 唯一的 4xx 是 422：lat 与 lng 只给了一个，或取值超范围。**拒绝授权定位
// 不在这一支里** —— 那是「一个都没给」，与「坐标落在围栏外」走同一条回落路径
// （产品第 3 条答复）。两者在进 service 之前就已经合流成 coord == nil。
func (h *StoreHandler) Resolve(c *gin.Context) {
	coord, ok := parseCoord(c)
	if !ok {
		return
	}
	// size 解析不出就传 0，由 service 钳到默认值 10（契约的 default）。
	// 钳制规则在 service：它是业务规则，不是解析细节。
	size, _ := strconv.Atoi(c.Query("size"))

	res, err := h.svc.Resolve(c.Request.Context(), coord, size)
	if err != nil {
		writeStoreQueryError(c, err)
		return
	}
	stores := make([]api.StoreMatch, 0, len(res.Stores))
	for _, m := range res.Stores {
		stores = append(stores, apiStoreMatch(m))
	}
	c.JSON(http.StatusOK, api.StoreResolveResult{
		MatchType: api.StoreMatchType(res.MatchType),
		// Stores 在 none 那一支是**空数组，不是 null**（契约明写）。
		// make(..., 0, n) 保证了这一点 —— 一个 nil slice 会序列化成 null，
		// 而客户端对 null 与 [] 的处置不同（前者常常是一次 crash）。
		Stores: stores,
	})
}

// parseCoord 把 lat / lng 两个 query 参数收成一个 *service.Coord。
//
// 用一个结构体的指针而不是两个 *float64：后者让「只给了 lat」成为一个
// 写得出来的状态，而那是一次 422，不该由每个调用方各自记得去判。
//
// 解析失败（`?lat=abc`）与**没传**在这里刻意是两件事：没传是合法输入
// （走回落），传了个解不开的值是 422。把前者当成后者会让一次拼错参数名的
// 请求静默拿到兜底门店，而客户端以为自己按坐标查过了。
func parseCoord(c *gin.Context) (*service.Coord, bool) {
	rawLat, rawLng := c.Query("lat"), c.Query("lng")
	if rawLat == "" && rawLng == "" {
		return nil, true
	}
	if rawLat == "" || rawLng == "" {
		writeStoreQueryError(c, service.ErrInvalidCoord)
		return nil, false
	}
	lat, errLat := strconv.ParseFloat(rawLat, 64)
	lng, errLng := strconv.ParseFloat(rawLng, 64)
	if errLat != nil || errLng != nil {
		writeStoreQueryError(c, service.ErrInvalidCoord)
		return nil, false
	}
	// 取值范围由 service 判（契约的 minimum / maximum 是业务约束）。
	return &service.Coord{Lat: lat, Lng: lng}, true
}

// apiStoreMatch 把一次命中装成契约的 StoreMatch。
//
// 与 apiStore 的唯一差别是 distance_m，而它在契约里是**必填且可空**
// （json:"distance_m" 没有 omitempty）：为 null 当且仅当算不出距离。
// 两个类型因此不能合一 —— 合了的话 GET /stores 的每一行都会多一个
// 恒为 null 的 distance_m，而客户端会以为那是「距离未知」，
// 不是「这条接口不算距离」。
func apiStoreMatch(m repository.StoreMatch) api.StoreMatch {
	return api.StoreMatch{
		Id:        m.ID,
		Name:      m.Name,
		Phone:     optStr(m.Phone),
		Address:   optStr(m.Address),
		IsDefault: m.IsDefault,
		Lat:       optCoord(m.Lat),
		Lng:       optCoord(m.Lng),
		DistanceM: optCoord(m.DistanceM),
	}
}
