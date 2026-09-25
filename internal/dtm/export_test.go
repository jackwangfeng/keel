package dtm

// 只在测试里可见的观察点。
//
// 信号量那条断言守的是「真的有上限」，不是「代码里有个 channel」。要断言前者
// 就得看见「同时进到 cgo 里的调用数」的峰值，而那个数在正常 API 面上没有意义 ——
// 导出它只会让调用方去读一个它不该关心的指标。
func (t *TC) peakInflight() int64 { return t.peak.Load() }

func (t *TC) semCap() int { return cap(t.sem) }
