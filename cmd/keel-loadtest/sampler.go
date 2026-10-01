package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sampler 在压测期间轮询 docker stats 与 pg_stat_activity。两者都是可选的外部命令：
// 没有 docker 或没权限时只是采不到，不影响压测本身。
type sampler struct {
	wg  sync.WaitGroup
	mu  sync.Mutex
	cpu map[string]*sampleStat
	mem map[string]*sampleStat
	pg  map[string]*sampleStat
}

func startSampler(ctx context.Context, containers, pgTargets []string) *sampler {
	s := &sampler{cpu: map[string]*sampleStat{}, mem: map[string]*sampleStat{}, pg: map[string]*sampleStat{}}
	if len(containers) > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for ctx.Err() == nil {
				args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, containers...)
				out, err := exec.CommandContext(ctx, "docker", args...).Output()
				if err != nil || ctx.Err() != nil {
					continue
				}
				s.mu.Lock()
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					f := strings.Split(line, "\t")
					if len(f) < 3 {
						continue
					}
					cpu, err := strconv.ParseFloat(strings.TrimSuffix(f[1], "%"), 64)
					if err != nil {
						continue
					}
					name := shortName(f[0])
					get(s.cpu, name).add(cpu)
					get(s.mem, name).add(parseMiB(strings.TrimSpace(strings.Split(f[2], "/")[0])))
				}
				s.mu.Unlock()
			}
		}()
	}
	for _, t := range pgTargets {
		c, db, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			q := `SELECT coalesce(state, '?'), count(*), count(*) FILTER (WHERE wait_event_type = 'Lock')
			        FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()
			       GROUP BY 1`
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for ctx.Err() == nil {
				out, err := exec.CommandContext(ctx, "docker", "exec", c, "psql", "-U", "keel", "-d", db, "-XAt", "-F", "\t", "-c", q).Output()
				if err == nil && ctx.Err() == nil {
					counts := map[string]float64{"total": 0, "active": 0, "idle": 0, "idle in transaction": 0, "lockwait": 0}
					for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
						f := strings.Split(line, "\t")
						if len(f) < 3 {
							continue
						}
						n, _ := strconv.ParseFloat(f[1], 64)
						l, _ := strconv.ParseFloat(f[2], 64)
						counts[f[0]] += n
						counts["total"] += n
						counts["lockwait"] += l
					}
					s.mu.Lock()
					for k, v := range counts {
						get(s.pg, shortName(c)+"."+k).add(v)
					}
					s.mu.Unlock()
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
				}
			}
		}()
	}
	return s
}

func (s *sampler) wait() (cpu, mem, pg map[string]*sampleStat) {
	s.wg.Wait()
	return s.cpu, s.mem, s.pg
}

func get(m map[string]*sampleStat, k string) *sampleStat {
	if m[k] == nil {
		m[k] = &sampleStat{}
	}
	return m[k]
}

// shortName 去掉 compose 项目前缀与序号：keelchaos-postgres-1 → postgres。
func shortName(n string) string {
	if i := strings.Index(n, "-"); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSuffix(n, "-1")
}

func parseMiB(s string) float64 {
	units := []struct {
		suf string
		mul float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"B", 1.0 / 1024 / 1024}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suf), 64)
			if err == nil {
				return v * u.mul
			}
		}
	}
	return 0
}
