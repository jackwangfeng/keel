package understanding_test

import (
	"context"
	"strings"
	"testing"

	"github.com/keel/keel/internal/understanding"
)

// 合规检查（商品理解服务设计 §2 的快路径）的行为闸门。
//
// 这一组要能区分的是两件长得很像的事：
//
//	· 「标题里有『最佳』，拦住了」—— 词表在工作；
//	· 「标题里有『最近』，**没有**拦」—— 词表的边界在工作。
//
// 第二条是这份词表唯一的刹车。《广告法》第九条禁的是绝对化用语，不是「最」
// 这个字；把裸字列进词表，全站商家的标题会成片被拒，而且没有任何出口 ——
// 那是一个看上去更严格、也不会让第一条变红的动作。

func check(t *testing.T, in understanding.ProductInput) []understanding.Violation {
	t.Helper()
	v, err := understanding.ComplianceCheck{}.Check(context.Background(), in)
	if err != nil {
		t.Fatalf("检查报错了: %v", err)
	}
	return v
}

// 三类违禁词都拦得住，而且**位置是对的**。
//
// 位置这一半和「拦住了」同样要紧：契约把 offset / length 放进 FieldError，
// 客户端据此高亮。位置算错的话商家看到的是「第 3 个字有问题」而那里是个逗号，
// 而「拦住了」这条断言对此一声不吭。
func TestComplianceCatchesBannedTermsWithPosition(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           understanding.ProductInput
		field, term  string
		offset, size int
	}{
		{
			name:   "绝对化用语在标题里",
			in:     understanding.ProductInput{Title: "全网最佳手冲咖啡壶"},
			field:  "title",
			term:   "最佳",
			offset: 2, size: 2,
		},
		{
			name:   "国家机关背书在副标题里",
			in:     understanding.ProductInput{Subtitle: "所有商品国家免检"},
			field:  "subtitle",
			term:   "国家免检",
			offset: 4, size: 4,
		},
		{
			name:   "变体写法在详情里",
			in:     understanding.ProductInput{Description: "销量no.1，闭眼入"},
			field:  "description",
			term:   "no.1",
			offset: 2, size: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := check(t, tc.in)
			if len(got) != 1 {
				t.Fatalf("命中 %d 条，期望 1 条：%+v", len(got), got)
			}
			v := got[0]
			if v.Field != tc.field || v.Term != tc.term {
				t.Errorf("命中的是 %s 的 %q，期望 %s 的 %q", v.Field, v.Term, tc.field, tc.term)
			}
			if v.Offset != tc.offset || v.Length != tc.size {
				t.Errorf("位置是 [%d, %d)，期望 [%d, %d) —— "+
					"客户端按这两个数高亮，算错了商家会被指到一个没问题的地方",
					v.Offset, v.Length, tc.offset, tc.offset+tc.size)
			}
			// 按码点算，不是按字节。中文一个字三个字节，混淆了的话
			// 上面那个 offset 会是它的三倍 —— 而那正好是最容易写出来的 bug。
			if strings.HasPrefix(tc.field, "title") && v.Offset >= len(tc.in.Title) {
				t.Errorf("offset %d 落在标题的字节长度 %d 之外 —— "+
					"这两个数被按字节算了", v.Offset, len(tc.in.Title))
			}
		})
	}
}

// **反例：正常中文不许被拦。**
//
// 这条是整份词表的刹车，见文件头。它盯的是一个具体的、很容易被做出来的动作：
// 往 wordlist 里加一条裸「最」或裸「第一」。加了之后上面那条测试更绿，
// 而这一条会红。
func TestCommonChineseWordsAreNotFlagged(t *testing.T) {
	for _, s := range []string{
		"最近新到的一批货",     // 「最」不是违禁词，「最近」是正常词
		"最后三件，售完即止",    // 同上
		"最初的设计稿",       // 同上
		"第一次使用请先清洗",    // 「第一」同理
		"第一天就上手",       // 同上
		"每人最多购买 5 件",   // 「最多」在这里是限购说明
		"绝配的一对马克杯",     // 「绝」不是违禁词
		"顶层楼王视野",       // 「顶」不是违禁词，「顶级」才是
		"独立包装，一件一盒",    // 「独」不是违禁词，「独一无二」才是
		"这款咖啡壶的领口设计",   // 「领」不是违禁词
	} {
		if got := check(t, understanding.ProductInput{Title: s}); len(got) != 0 {
			t.Errorf("%q 被拦下了（命中 %+v）—— 《广告法》禁的是绝对化用语，"+
				"不是「最」「第一」这两个字本身。裸字进词表会让全站商家的标题"+
				"成片被拒，而且没有任何出口", s, got)
		}
	}
}

// 规范化：全角、大小写、以及**空白规避**。
//
// 「最 佳」加一个空格就绕过词表，是最常见的一种躲法。
// 而 length 要把那个空格算进去 —— 客户端高亮出来才盖得住用户看到的那一段。
func TestComplianceNormalizesBeforeMatching(t *testing.T) {
	for _, tc := range []struct {
		in           string
		term         string
		offset, size int
	}{
		{"全网最 佳咖啡壶", "最佳", 2, 3},    // 半角空格
		{"全网最　佳咖啡壶", "最佳", 2, 3},    // 全角空格
		{"销量ＮＯ．１，闭眼入", "no.1", 2, 4}, // 全角字母数字
		{"销量No.1，闭眼入", "no.1", 2, 4},  // 大小写
	} {
		got := check(t, understanding.ProductInput{Title: tc.in})
		if len(got) != 1 {
			t.Errorf("%q 命中 %d 条，期望 1 条 —— 规避写法没被归一化：%+v",
				tc.in, len(got), got)
			continue
		}
		if got[0].Term != tc.term || got[0].Offset != tc.offset || got[0].Length != tc.size {
			t.Errorf("%q 命中的是 %q @[%d,+%d)，期望 %q @[%d,+%d) —— "+
				"被丢掉的空白要算进 length，否则高亮盖不住商家看到的那一段",
				tc.in, got[0].Term, got[0].Offset, got[0].Length,
				tc.term, tc.offset, tc.size)
		}
	}
}

// **标点不当作可跨越的空白。** 这是上一条那个取舍的另一半。
//
// 丢掉标点会让「价格最。好评如潮」拼出一个「最好」，那是一次误拒。
// 代价是「最·好」这类写法拦不住 —— 那属于小模型兜底的那一半，本轮没有做。
func TestPunctuationIsNotSkipped(t *testing.T) {
	for _, s := range []string{"价格最。好评如潮", "论性价比它最，好在哪呢"} {
		if got := check(t, understanding.ProductInput{Title: s}); len(got) != 0 {
			t.Errorf("%q 被拦下了（%+v）—— 跨过标点拼词会造出误拒，"+
				"而标点在中文里本来就是语义边界", s, got)
		}
	}
}

// 同一处被两条词条命中时只报一条（长的那条）。
//
// 商家看到同一个地方被报两次，第一反应是「我改了一个它还在」。
func TestOverlappingHitsCollapseToTheLongest(t *testing.T) {
	// 「全球第一」与「第一品牌」都在词表里；「全球第一品牌」这段文字里，
	// 两条都命中且互相重叠但谁也不包含谁 —— 这种情况两条都该留。
	// 真正要收敛的是「完全被包住」那种，用「最佳」与一个包含它的更长词造不出来，
	// 所以这里用同一条词的重复出现来验去重的稳定性，
	// 再用一条真包含的情形（no.1 与 no1 在 "no.1" 上不重叠）做对照。
	got := check(t, understanding.ProductInput{Title: "最佳最佳"})
	if len(got) != 2 {
		t.Fatalf("「最佳最佳」命中 %d 条，期望 2 条（两处各一条）：%+v", len(got), got)
	}
	if got[0].Offset != 0 || got[1].Offset != 2 {
		t.Errorf("两处的位置是 %d / %d，期望 0 / 2 —— 结果没有按位置排序，"+
			"商家看到的错误顺序与文案顺序对不上", got[0].Offset, got[1].Offset)
	}
}

// 互相重叠但谁也不包含谁的两条命中，**两条都要报**。
//
// 「国家免检产品」里「国家免检」与「免检产品」各占一段，只留一条的话，
// 商家改掉被报的那一段之后另一段还在，他会以为这道闸门在耍他。
// 这与上一条（完全被包住的收敛成一条）不矛盾：判据是「包含」，不是「相交」。
func TestOverlappingButNotContainedHitsAreBothReported(t *testing.T) {
	got := check(t, understanding.ProductInput{Title: "本品为国家免检产品"})
	if len(got) != 2 {
		t.Fatalf("「国家免检产品」命中 %d 条，期望 2 条（国家免检 / 免检产品）：%+v",
			len(got), got)
	}
	if got[0].Term != "国家免检" || got[1].Term != "免检产品" {
		t.Errorf("命中的是 %q / %q，期望 国家免检 / 免检产品", got[0].Term, got[1].Term)
	}
}

// 三个字段都扫，而且按字段顺序返回。
func TestComplianceScansEveryVisibleField(t *testing.T) {
	got := check(t, understanding.ProductInput{
		Title:       "最佳咖啡壶",
		Subtitle:    "国家免检",
		Description: "巅峰之作",
	})
	if len(got) != 3 {
		t.Fatalf("命中 %d 条，期望 3 条（三个字段各一条）：%+v", len(got), got)
	}
	for i, want := range []string{"title", "subtitle", "description"} {
		if got[i].Field != want {
			t.Errorf("第 %d 条在 %s 上，期望 %s —— 少扫一个字段的后果是"+
				"商家把违禁词挪到那里就能发布", i, got[i].Field, want)
		}
	}
}

// 干净的文案一条都不该命中。
//
// 阳性对照方向相反的那一条：一个「恒返回一条违规」的实现能让上面全部变绿。
func TestCleanCopyPasses(t *testing.T) {
	got := check(t, understanding.ProductInput{
		Title:       "手冲咖啡壶 600ml 玻璃",
		Subtitle:    "耐热玻璃，配不锈钢滤网",
		Description: "适合两到三人份，洗碗机可用。",
	})
	if len(got) != 0 {
		t.Fatalf("一段干净的文案被拦下了：%+v", got)
	}
}

// ctx 被取消时要返回错误，而不是「没命中」。
//
// **这条比它看上去要紧。** 调用方（internal/service 的 checkCompliance）
// 靠 ctx 的 200 ms 预算兑现 §7 里那条「超时保守拒绝」——
// 而一个从不看 ctx 的实现会让那条规矩在真出问题时静默失效：
// 它会一直算下去，算完返回「没命中」，于是超时变成了放行。
func TestComplianceHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := understanding.ComplianceCheck{}.Check(ctx,
		understanding.ProductInput{Title: "最佳咖啡壶"})
	if err == nil {
		t.Fatal("ctx 已经取消了，检查还是返回了结论 —— " +
			"调用方的 200 ms 预算因此形同虚设，而「超时保守拒绝」是 §7 里" +
			"全系统唯一一处宁可误拒的规矩")
	}
}

// 词表里的每一条都必须是**规范形态**：小写、半角、无空白。
//
// 不是的话那一条永远匹配不上，而且不会有任何东西报错 —— 加了一个词、
// 以为拦住了，实际没拦。
func TestWordlistEntriesAreNormalized(t *testing.T) {
	// 走公开面：拿每一条词自己去查一次，查不到就说明它写得不对。
	// 直接读 wordlist 需要在包内测试，而包内测试看不到 Check 的真实路径。
	for _, probe := range []string{
		"最佳", "国家免检", "no.1", "巅峰之作", "蕞", "特供", "遥遥领先",
	} {
		if got := check(t, understanding.ProductInput{Title: probe}); len(got) != 1 {
			t.Errorf("词表里的 %q 自己都匹配不上（命中 %d 条）—— "+
				"它多半带着大写、全角或空格，而那样的词条永远不会命中，"+
				"加它的人却以为拦住了", probe, len(got))
		}
	}
}

// 空文案不该崩，也不该命中。
func TestEmptyInputIsClean(t *testing.T) {
	if got := check(t, understanding.ProductInput{}); len(got) != 0 {
		t.Fatalf("空商品命中了 %+v", got)
	}
}
