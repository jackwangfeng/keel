package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// doReq 发一个请求，读完响应体。body 为空串表示不带体。
func doReq(c *http.Client, method, url, body string, hdr map[string]string) (int, []byte, string, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header.Get("Content-Type"), err
}

// worker 是一个闭环并发单元。每个 worker 固定扮演一个买家（按序号轮到），自带统计，结束时合并。
type worker struct {
	id    int
	cfg   *config
	rng   *rand.Rand
	user  *userFix
	stats map[string]*labelStats
	// 统计窗口：预热期间的请求照发但不记。
	recording bool
	// 场景自己的状态（购物车条目、计数器等）。
	n     int
	state map[string]any
}

type labelStats struct {
	lat      []time.Duration
	codes    map[int]int
	problems map[string]int
	netErrs  int
}

// call 发请求并记账。path 以 / 开头，相对 /api/v1；auth 为 "user" / "admin" / ""。
func (w *worker) call(label, method, path, body, auth string, extra map[string]string) (int, []byte) {
	hdr := map[string]string{}
	switch auth {
	case "user":
		hdr["Authorization"] = "Bearer " + w.user.Token
	case "admin":
		hdr["Authorization"] = "Bearer " + w.cfg.adminToken
	}
	for k, v := range extra {
		hdr[k] = v
	}
	url := w.cfg.base + "/api/v1" + path
	if strings.HasPrefix(path, "/api/") {
		url = w.cfg.base + path
	}
	t0 := time.Now()
	st, b, ctype, err := doReq(w.cfg.client, method, url, body, hdr)
	el := time.Since(t0)
	if !w.recording {
		return st, b
	}
	s := w.stats[label]
	if s == nil {
		s = &labelStats{codes: map[int]int{}, problems: map[string]int{}}
		w.stats[label] = s
	}
	if err != nil {
		s.netErrs++
		s.lat = append(s.lat, el)
		return 0, nil
	}
	s.lat = append(s.lat, el)
	s.codes[st]++
	if st >= 400 {
		pt := "-"
		if strings.Contains(ctype, "json") {
			var p struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(b, &p) == nil && p.Type != "" {
				pt = strings.TrimPrefix(p.Type, "https://keel.dev/problems/")
			}
		}
		s.problems[fmt.Sprintf("%d %s", st, pt)]++
	}
	return st, b
}

type sampleStat struct {
	Avg, Max float64
	n        int
}

func (s *sampleStat) add(v float64) {
	s.Avg = (s.Avg*float64(s.n) + v) / float64(s.n+1)
	s.n++
	s.Max = max(s.Max, v)
}

type labelResult struct {
	Label    string         `json:"label"`
	Count    int            `json:"count"`
	RPS      float64        `json:"rps"`
	P50      float64        `json:"p50_ms"`
	P95      float64        `json:"p95_ms"`
	P99      float64        `json:"p99_ms"`
	Max      float64        `json:"max_ms"`
	ErrRate  float64        `json:"err_rate"`
	Codes    map[int]int    `json:"codes"`
	Problems map[string]int `json:"problems,omitempty"`
	NetErrs  int            `json:"net_errs,omitempty"`
}

type levelResult struct {
	Scenario string                 `json:"scenario"`
	Conc     int                    `json:"concurrency"`
	Seconds  float64                `json:"seconds"`
	Start    time.Time              `json:"start"`
	Labels   []labelResult          `json:"labels"`
	Total    labelResult            `json:"total"`
	CPU      map[string]*sampleStat `json:"cpu_pct,omitempty"`
	Mem      map[string]*sampleStat `json:"mem_mib,omitempty"`
	PG       map[string]*sampleStat `json:"pg,omitempty"`
	Load     [2]string              `json:"loadavg"`
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		return strings.Join(f[:3], " ")
	}
	return ""
}

func runLevel(ctx context.Context, cfg *config, sc scenario, name string, conc int, dur, warm time.Duration,
	sampleNames, pgTargets []string) levelResult {
	res := levelResult{Scenario: name, Conc: conc}
	res.Load[0] = loadavg()
	workers := make([]*worker, conc)
	for i := range workers {
		w := &worker{id: i, cfg: cfg, rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(i))),
			stats: map[string]*labelStats{}, state: map[string]any{}}
		if len(cfg.fx.Users) > 0 {
			w.user = &cfg.fx.Users[i%len(cfg.fx.Users)]
		}
		workers[i] = w
	}
	lctx, cancel := context.WithTimeout(ctx, warm+dur)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lctx.Err() == nil {
				sc.step(w)
			}
		}()
	}
	// 预热结束后开始记账与采样。
	select {
	case <-time.After(warm):
	case <-lctx.Done():
	}
	for _, w := range workers {
		w.recording = true // 各 worker 下一个请求起生效；单写多读的布尔，竞态无害
	}
	start := time.Now()
	res.Start = start
	smp := startSampler(lctx, sampleNames, pgTargets)
	wg.Wait()
	res.Seconds = time.Since(start).Seconds()
	res.CPU, res.Mem, res.PG = smp.wait()
	res.Load[1] = loadavg()

	merged := map[string]*labelStats{}
	for _, w := range workers {
		for l, s := range w.stats {
			m := merged[l]
			if m == nil {
				m = &labelStats{codes: map[int]int{}, problems: map[string]int{}}
				merged[l] = m
			}
			m.lat = append(m.lat, s.lat...)
			for k, v := range s.codes {
				m.codes[k] += v
			}
			for k, v := range s.problems {
				m.problems[k] += v
			}
			m.netErrs += s.netErrs
		}
	}
	all := &labelStats{codes: map[int]int{}, problems: map[string]int{}}
	labels := make([]string, 0, len(merged))
	for l, s := range merged {
		labels = append(labels, l)
		all.lat = append(all.lat, s.lat...)
		for k, v := range s.codes {
			all.codes[k] += v
		}
		for k, v := range s.problems {
			all.problems[k] += v
		}
		all.netErrs += s.netErrs
	}
	sort.Strings(labels)
	for _, l := range labels {
		res.Labels = append(res.Labels, summarize(l, merged[l], res.Seconds))
	}
	res.Total = summarize("TOTAL", all, res.Seconds)
	return res
}

func summarize(label string, s *labelStats, secs float64) labelResult {
	r := labelResult{Label: label, Count: len(s.lat), Codes: s.codes, Problems: s.problems, NetErrs: s.netErrs}
	if len(s.lat) == 0 {
		return r
	}
	sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
	q := func(p float64) float64 {
		i := int(p * float64(len(s.lat)-1))
		return float64(s.lat[i].Microseconds()) / 1000
	}
	r.RPS = float64(len(s.lat)) / secs
	r.P50, r.P95, r.P99 = q(0.50), q(0.95), q(0.99)
	r.Max = float64(s.lat[len(s.lat)-1].Microseconds()) / 1000
	errs := s.netErrs
	for c, n := range s.codes {
		if c >= 500 {
			errs += n
		}
	}
	r.ErrRate = float64(errs) / float64(len(s.lat))
	return r
}

func (r levelResult) print(out io.Writer) {
	fmt.Fprintf(out, "\n=== %s  并发 %d  统计 %.0fs  load %s → %s\n", r.Scenario, r.Conc, r.Seconds, r.Load[0], r.Load[1])
	fmt.Fprintf(out, "%-22s %8s %9s %8s %8s %8s %8s %7s  %s\n", "接口", "请求数", "RPS", "p50ms", "p95ms", "p99ms", "maxms", "5xx%", "状态码 / problem")
	for _, l := range append(r.Labels, r.Total) {
		codes := make([]string, 0, len(l.Codes))
		for c, n := range l.Codes {
			codes = append(codes, fmt.Sprintf("%d×%d", c, n))
		}
		sort.Strings(codes)
		probs := make([]string, 0, len(l.Problems))
		for p, n := range l.Problems {
			probs = append(probs, fmt.Sprintf("[%s]×%d", p, n))
		}
		sort.Strings(probs)
		if l.NetErrs > 0 {
			probs = append(probs, fmt.Sprintf("[网络错误]×%d", l.NetErrs))
		}
		fmt.Fprintf(out, "%-22s %8d %9.1f %8.1f %8.1f %8.1f %8.1f %6.2f%%  %s %s\n", l.Label, l.Count, l.RPS, l.P50, l.P95, l.P99,
			l.Max, l.ErrRate*100, strings.Join(codes, " "), strings.Join(probs, " "))
	}
	if len(r.CPU) > 0 {
		names := sortedKeys(r.CPU)
		var parts []string
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s %.0f%%（峰 %.0f%%，%.0fMiB）", n, r.CPU[n].Avg, r.CPU[n].Max, r.Mem[n].Max))
		}
		fmt.Fprintf(out, "CPU（100%%=1 核）：%s\n", strings.Join(parts, "；"))
	}
	if len(r.PG) > 0 {
		names := sortedKeys(r.PG)
		var parts []string
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s 均 %.1f 峰 %.0f", n, r.PG[n].Avg, r.PG[n].Max))
		}
		fmt.Fprintf(out, "pg_stat_activity：%s\n", strings.Join(parts, "；"))
	}
}

func sortedKeys(m map[string]*sampleStat) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func (r levelResult) appendJSON(path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
}
