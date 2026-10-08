package traceid

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// 这一道的验收只能靠**日志里真的出现了/真的没出现那条记录**来判。
// 「跑完没报错」不说明闸门生效了 —— 它默认就是关着的，忘了打开照样全绿。
//
// 所以每个用例都：装一个把输出抓进内存的 handler → 打一个请求 → 数那条记录。
// 另有两条专门对着「曾经真的会红」的情形：
//   - TestAccessLogDropsWhenSilent 证明默认关着的时候真的没记录（漏挂闸门会红）
//   - TestAccessLogRateOutOfRange证明传错的值不会把开关变成全量

// capture 抓 slog 的输出。
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) install() {
	c.mu.Lock()
	c.buf.Reset()
	c.mu.Unlock()
	slog.SetDefault(slog.New(slog.NewTextHandler(&c.buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
}

// lines 抓到的日志行。拷贝出来再用（持有锁返回会和写入并发）。
func (c *capture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Split(strings.TrimSpace(c.buf.String()), "\n")
}

// countLines 数匹配 substr 的行数。
func (c *capture) countLines(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range strings.Split(c.buf.String(), "\n") {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func newTestRouter() *gin.HandlerFunc {
	gin.SetMode(gin.TestMode)
	inner := AccessLog()
	return &inner
}

// do 打一个请求，返回状态码。handler 由调用方给。
func do(r *gin.Engine, method, path string, body string, hdr map[string]string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func engine(h gin.HandlerFunc, terminal gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware()) // 号先挂上去，与 app.go 同序
	r.Use(h)
	r.GET("/ok", terminal)
	r.POST("/echo", terminal)
	r.GET("/slow", terminal)
	r.GET("/healthz", terminal)
	r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	return r
}

// ---------------------------------------------------------------------------
// 闸门本身
// ---------------------------------------------------------------------------

// TestShouldLogFourRules 把 shouldLog 的四条判据逐条钉住。
// 每条给「刚好不满足」与「刚好满足」两个输入 —— 只测满足那一侧的话，
// 一个恒真�� shouldLog 也能全绿。
func TestShouldLogFourRules(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })

	const id = "0123456789abcdef0123456789abcdef"
	fast := 10 * time.Millisecond
	slow := slowMillis * time.Millisecond

	cases := []struct {
		name string
		path string
		st   int
		dur  time.Duration
		id   string
		rate int64
		want bool
	}{
		{"正常的快请求、默认关着 → 不记", "/ok", 200, fast, id, 0, false},
		{"5xx 无条件记", "/ok", 500, fast, id, 0, true},
		{"404 无条件记", "/ok", 404, fast, id, 0, true},
		{"409 也记（幂等在飞）", "/ok", 409, fast, id, 0, true},
		{"慢请求无条件记", "/slow", 200, slow, id, 0, true},
		{"慢一丁点不算慢（边界下侧）", "/ok", 200, slow - time.Millisecond, id, 0, false},
		{"健康检查 200 也不记", "/healthz", 200, fast, id, 0, false},
		{"健康检查 500 也不记（闸在最前）", "/healthz", 500, fast, id, 0, false},
		{"采样率 100 → 记", "/ok", 200, fast, id, 100, true},
		{"采样率 1 且 id 不在白名单 → 走随机，这里只验不越界", "/ok", 200, fast, "", 1, false},
		{"没有号也不影响前四条", "/ok", 500, fast, "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetForTest()
			SetSampleRate(tc.rate)
			if got := shouldLog(tc.path, tc.st, tc.dur, tc.id); got != tc.want {
				t.Fatalf("shouldLog = %v, 想要 %v", got, tc.want)
			}
		})
	}
}

// TestShouldLogForcedBeatsSilentRate 白名单是唯一「不看采样率」的判据。
func TestShouldLogForcedBeatsSilentRate(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	resetForTest()
	SetSampleRate(0)

	const id = "0123456789abcdef0123456789abcdef"
	if shouldLog("/ok", 200, time.Millisecond, id) {
		t.Fatal("还没加白名单就记了")
	}
	if !ForceOn(id) {
		t.Fatal("ForceOn 拒绝了合法号")
	}
	if !shouldLog("/ok", 200, time.Millisecond, id) {
		t.Fatal("采样率 0，白名单里的号却没有被记 —— 排障姿势就是靠这一条")
	}
	if shouldLog("/ok", 200, time.Millisecond, "ffffffffffffffffffffffffffffffff") {
		t.Fatal("白名单之外的号被记了")
	}
}

// TestSamplingRateIsHonoured 采样率 100 必须真的全量记，而不是「碰巧」。
// 20 次请求全过 —— 单次能过可能是随机，20 次恒过不是。
func TestSamplingRateIsHonoured(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	resetForTest()
	SetSampleRate(100)

	for i := 0; i < 20; i++ {
		if !shouldLog("/ok", 200, time.Millisecond, "") {
			t.Fatalf("采样率 100，第 %d 次仍没记", i)
		}
	}
}

// TestSamplingRateOutOfRangeCloses 传错的值只能变成 0，不能变成全量。
// 这一条对着「运维手滑传了 2500 于是日志炸了」那个情形。
func TestSamplingRateOutOfRangeCloses(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	for _, v := range []int64{-1, 101, 2500} {
		resetForTest()
		SetSampleRate(v)
		if got := SampleRate(); got != 0 {
			t.Fatalf("SetSampleRate(%d) 之后采样率是 %d，应当被夹回 0", v, got)
		}
		if shouldLog("/ok", 200, time.Millisecond, "") {
			t.Fatalf("SetSampleRate(%d) 之后闸门是开的", v)
		}
	}
}

// TestForceOnRejectsBadShape 不合法的号一律拒。
// 白名单是用来查特定请求的，存进去一个查不到的东西等于给运维一个假象。
func TestForceOnRejectsBadShape(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	resetForTest()

	bad := []string{"", "  ", "abc", strings.Repeat("z", 32),
		strings.Repeat("a", 31), strings.Repeat("a", 33),
		strings.Repeat("a", 31) + "\n", "0123456789abcdef0123456789abcde-"}
	for _, s := range bad {
		if ForceOn(s) {
			t.Fatalf("ForceOn 接受了不法的号 %q", s)
		}
	}
	if ForceOff(strings.Repeat("a", 32)) {
		t.Fatal("ForceOff 报告成功，但那个号从没被加进去")
	}

	const id = "0123456789abcdef0123456789abcdef"
	if !ForceOn(id) {
		t.Fatal("ForceOn 拒绝了合法号")
	}
	if !Forced(id) {
		t.Fatal("Forced 说不在，刚加的号不见了")
	}
	if !ForceOff(id) {
		t.Fatal("ForceOff 报告失败，刚加的号删不掉")
	}
	if Forced(id) {
		t.Fatal("ForceOff 之后还在白名单里")
	}
}

// TestForcedListNeverLeaks 把白名单列出来，确认里面全是合法形状的号。
func TestForcedListNeverLeaks(t *testing.T) {
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	resetForTest()
	ForceOn("0123456789abcdef0123456789abcdef")
	ForceOn("fedcba9876543210fedcba9876543210")

	got := ForcedList()
	if len(got) != 2 {
		t.Fatalf("白名单列出了 %d 个，想要 2：%v", len(got), got)
	}
	for _, s := range got {
		if Normalize(s) == "" {
			t.Fatalf("白名单里有个不法的号 %q", s)
		}
	}
}

// ---------------------------------------------------------------------------
// 中间件：日志里真的出现/真的不出现
// ---------------------------------------------------------------------------

// TestAccessLogDropsWhenSilent 默认关着的时候，一个 200 请求不能留下记录。
// 这是对着「闸门忘了挂上」的红 —— 全绿的测试最需要的就是这种反向判据。
func TestAccessLogDropsWhenSilent(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusOK) })

	if code := do(r, "GET", "/ok", "", nil); code != 200 {
		t.Fatalf("状态码 %d", code)
	}
	if n := cap.countLines("method=GET"); n != 0 {
		t.Fatalf("采样率 0 的时候记下了 %d 条 200 请求的日志，闸门没关上", n)
	}
}

// TestAccessLogRecordsErrorAndCarriesTraceID 报错请求必须被记，且那条日志
// 里的 trace_id 要等于调用方带来的那个 —— 这一列是整道中间件存在的理由。
func TestAccessLogRecordsErrorAndCarriesTraceID(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusOK) })

	const id = "0123456789abcdef0123456789abcdef"
	if code := do(r, "GET", "/boom", "", map[string]string{Header: id}); code != 500 {
		t.Fatalf("状态码 %d", code)
	}

	lines := cap.lines()
	var hit string
	for _, l := range lines {
		if strings.Contains(l, "/boom") {
			hit = l
			break
		}
	}
	if hit == "" {
		t.Fatalf("500 请求没有留下日志，抓到的是：%v", lines)
	}
	if !strings.Contains(hit, "trace_id="+id) {
		t.Fatalf("那条日志里的 trace_id 不是调用方带来的那个 %s：%s", id, hit)
	}
	if !strings.Contains(hit, "status=500") {
		t.Fatalf("那条日志里没有 status=500：%s", hit)
	}
}

// TestAccessLogRecordsWhitelistedTraceOnly 白名单命中就记，且**只**记它。
// 「只记它」这一侧是对着「白名单写成了全局开关」那个情形。
func TestAccessLogRecordsWhitelistedTraceOnly(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusOK) })

	const want = "0123456789abcdef0123456789abcdef"
	const other = "fedcba9876543210fedcba9876543210"
	ForceOn(want)

	do(r, "GET", "/ok", "", map[string]string{Header: other})
	if n := cap.countLines("method=GET"); n != 0 {
		t.Fatalf("非白名单的号也被记了 %d 条，白名单起不到隔离作用", n)
	}

	do(r, "GET", "/ok", "", map[string]string{Header: want})
	if n := cap.countLines("method=GET"); n != 1 {
		t.Fatalf("白名单里的号记了 %d 条，想要正好 1 条", n)
	}
	if n := cap.countLines("trace_id=" + other); n != 0 {
		t.Fatalf("别人的号被记了 %d 条", n)
	}

	// 移出白名单之后立刻不再记 —— 这就是「运行时开关」的可逆性。
	ForceOff(want)
	do(r, "GET", "/ok", "", map[string]string{Header: want})
	if n := cap.countLines("method=GET"); n != 1 {
		t.Fatalf("ForceOff 之后又记了一条，累计 %d 条", n)
	}
}

// TestAccessLogHonoursRateAtRuntime 采样率在运行中改，立刻生效，不用重建。
func TestAccessLogHonoursRateAtRuntime(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusOK) })

	do(r, "GET", "/ok", "", nil)
	if n := cap.countLines("method=GET"); n != 0 {
		t.Fatalf("关着的时候记了 %d 条", n)
	}

	SetSampleRate(100)
	do(r, "GET", "/ok", "", nil)
	if n := cap.countLines("method=GET"); n != 1 {
		t.Fatalf("开了之后累计只记了 %d 条", n)
	}

	SetSampleRate(0)
	do(r, "GET", "/ok", "", nil)
	if n := cap.countLines("method=GET"); n != 1 {
		t.Fatalf("关回去之后累计 %d 条 —— 采样率没在运行时生效", n)
	}
}

// TestAccessLogNeverRecordsBody 报文不进日志。登录的报文里有口令，
// webhook 的报文里有金额与流水号 —— 这条对着「顺手把 body 也打进去」。
func TestAccessLogNeverRecordsBody(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusInternalServerError) })

	const secret = "s3cr3t-token-value"
	do(r, "POST", "/echo", `{"password":"`+secret+`"}`, nil)
	if n := cap.countLines(secret); n != 0 {
		t.Fatalf("请求体里的秘密进了日志 %d 处", n)
	}
	if n := cap.countLines("password"); n != 0 {
		t.Fatalf("日志里出现了 password 这个键 %d 次", n)
	}
}

// TestAccessLogCountsStats 计数器是给观察用的：记下的与丢掉的必须对得上总数。
func TestAccessLogCountsStats(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	h := newTestRouter()
	r := engine(*h, func(c *gin.Context) { c.Status(http.StatusOK) })

	do(r, "GET", "/ok", "", nil)          // 正常快 → 丢
	do(r, "GET", "/boom", "", nil)        // 500 → 记
	do(r, "GET", "/healthz", "", nil)     // 健康检查 → 丢（闸在最前）
	do(r, "GET", "/ok", "", map[string]string{Header: "0123456789abcdef0123456789abcdef"}) // 200 快 → 丢

	s := GetStats()
	if s.Kept != 1 {
		t.Fatalf("Kept = %d，想要 1", s.Kept)
	}
	if s.Dropped != 3 {
		t.Fatalf("Dropped = %d，想要 3", s.Dropped)
	}
	if s.SlowMillis != slowMillis {
		t.Fatalf("SlowMillis = %d，想要 %d", s.SlowMillis, slowMillis)
	}

	// Stats 能 JSON 出去 —— 后台接口直接返回它。
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("Stats 序列化失败：%v", err)
	}
}

// TestAccessLogLogsSlowRequest 慢请求在采样率 0 时也被记。
// 这一条对着「阈值写成了 2 秒，于是所有该查的慢请求都过去了」。
func TestAccessLogLogsSlowRequest(t *testing.T) {
	resetForTest()
	t.Cleanup(func() { resetForTest(); SetSampleRate(0) })
	SetSampleRate(0)

	cap := &capture{}
	cap.install()
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.Use(AccessLog())
	r.GET("/slow", func(c *gin.Context) {
		time.Sleep(slowMillis * time.Millisecond * 2)
		c.Status(http.StatusOK)
	})

	do(r, "GET", "/slow", "", nil)
	if n := cap.countLines("/slow"); n != 1 {
		t.Fatalf("慢请求记了 %d 条，想要 1", n)
	}
}