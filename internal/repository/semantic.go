package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository/internal/db"
)

// 派生数据入库（M3 Task 3）在 repository 边界上的那一面。
//
// # 这一层守的是「写进去的向量能不能用」，而那件事没有任何别的地方会报错
//
// 语义检索层 §2.3 把话说死了：配合 vector_cosine_ops，**写入未归一化的向量
// 不会报错，只会悄悄拉低召回质量**。也就是说一个范数是 7.3 的向量：
// INSERT 成功、HNSW 索引照建、检索照常返回结果、每一条形状断言照绿 ——
// 只有排序变得没有意义，而没有人会在几个月后把「搜索好像不太准」这件事
// 追回到某一次入库。
//
// internal/inference 的客户端已经核过一遍范数了（那个包的 validate）。
// 这里再核一遍不是重复：
//
//	· 客户端核的是**引擎的输出**。这里核的是**真的要写进那一列的东西** ——
//	  两者之间隔着调用方，而调用方完全可以从别处（缓存、影子表回填、
//	  将来某个自己算向量的 processor）拿到一个向量再交给这里。
//	· 「引擎将来换实现」是 §10 明说要允许的事（接口稳定、引擎可独立演进）。
//	  换掉之后 internal/inference 那一道还在不在、还严不严，不由这一层决定。
//	· 这是最后一道。它下面就是 SQL，SQL 下面是一列不会抱怨的 vector(1024)。
//
// 维度同理：pgvector 的 vector(1024) 列**会**拒绝别的维度（数据模型 §8 原话：
// 「768 维的向量插不进 vector(1024) 列，是插入直接报错」），所以维度这一条
// 有数据库兜底。仍然在这里查一遍，是为了让错误信息里有商品 id 和两个维度值，
// 而不是一句 `expected 1024 dimensions, not 768`。

// ErrVectorNotNormalized：向量的 L2 范数不是 1。入库与检索两侧共用一个 sentinel：
// 两边坏的是同一件事（余弦距离失真），而调用方对它的处置也一样 —— 重试没有用。
//
// 做成 sentinel 而不是一句普通错误：调用方对它的处置与「数据库连不上」完全不同 ——
// 重试没有用，这是一个坏值，要么是引擎坏了，要么是调用方自己算的。
var ErrVectorNotNormalized = errors.New("向量没有 L2 归一化，拒绝使用")

// ErrVectorWrongDim：维度不是 inference.Dim。
var ErrVectorWrongDim = errors.New("向量维度与 product_text_vectors.embedding 对不上")

// ErrProductGoneDuringIndex：写回时这件商品已经不在了（删了 / 软删了）。
// 正常路径，跳过即可。
var ErrProductGoneDuringIndex = errors.New("这件商品在索引过程中消失了")

// IndexCandidate 是一件**待判定**的商品：派生数据的全部输入，加上它上一次
// 被加工成什么样。判定（要不要重算）发生在 service 层，不在这里 ——
// 这一层不认得指纹怎么算，它只负责把原料端上来。
type IndexCandidate struct {
	ProductID    int64
	Title        string
	Subtitle     string
	CategoryName string

	// UpdatedAt 是读到这一行时 products.updated_at 的值。写回前要拿它与
	// 当时的值比对，见 LockProductForIndex。
	UpdatedAt time.Time

	// SearchText 是 products.search_text 的当前值。nil = 从没写过。
	SearchText *string

	// InputHashes 是 product_understanding.input_hashes 解出来的那张表。
	// nil（或者某个键不在）= 那个 processor 从没算过。
	InputHashes map[string]string

	// VectorModelName / VectorModelVersion 是 product_text_vectors 上记的
	// 「这条向量是谁算的」。nil = 还没有向量行。
	//
	// 它们是「要不要重算」的另一半依据（换模型 ⇒ 全量重算，语义检索层 §2.2）。
	VectorModelName    *string
	VectorModelVersion *string
}

// TextVector 是要写进 product_text_vectors 的一行。
//
// 没有 MerchantID 字段，也不可能有：那一列由 DEFAULT current_merchant() 填
// （00016 第二节），而写入语句里根本没有它。「拿 A 店的上下文往 B 店的商品
// 名下写一条向量」在这个签名下连编译都编不出来。
type TextVector struct {
	ProductID    int64
	Content      string
	Embedding    []float32
	ModelName    string
	ModelVersion string
}

// IndexTx 是派生数据入库在一次租户事务里能做的事。
type IndexTx interface {
	// ListStaleProductsForIndex 按触发点捞一批待判定的商品（增量）。
	ListStaleProductsForIndex(ctx context.Context, limit int32) ([]IndexCandidate, error)

	// ListProductsForIndex 按 id 游标翻页捞一批在架商品（全量）。
	ListProductsForIndex(ctx context.Context, afterID int64, limit int32) ([]IndexCandidate, error)

	// LockProductForIndex 锁住这一行并返回它当前的 updated_at。
	// 商品已消失时返回 ErrProductGoneDuringIndex。
	LockProductForIndex(ctx context.Context, productID int64) (time.Time, error)

	// UpsertProductTextVector 写文本向量。**向量在这里过维度与范数两道闸门**，
	// 不合格时返回 ErrVectorWrongDim / ErrVectorNotNormalized，一个字节也不写。
	UpsertProductTextVector(ctx context.Context, v TextVector) error

	// SetProductSearchText 写 bigram 串。search_vector 是生成列，跟着自动更新。
	SetProductSearchText(ctx context.Context, productID int64, text string) error

	// MarkProductIndexed 记下这一轮的判定结果：指纹（依据）与 updated_at（水位线）。
	// hashes 里只写这一轮动过的 processor，其余几格由 SQL 的 `||` 保留。
	MarkProductIndexed(ctx context.Context, m ProductIndexMark) error
}

func (t tenantTx) ListStaleProductsForIndex(ctx context.Context, limit int32) ([]IndexCandidate, error) {
	rows, err := t.q.ListStaleProductsForIndex(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]IndexCandidate, 0, len(rows))
	for _, r := range rows {
		c, err := candidateFrom(r.ID, r.Title, r.Subtitle, r.CategoryName, r.UpdatedAt,
			r.SearchText, r.InputHashes, r.VectorModelName, r.VectorModelVersion)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (t tenantTx) ListProductsForIndex(ctx context.Context, afterID int64, limit int32) ([]IndexCandidate, error) {
	rows, err := t.q.ListProductsForIndex(ctx, db.ListProductsForIndexParams{
		AfterID: afterID, RowLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]IndexCandidate, 0, len(rows))
	for _, r := range rows {
		c, err := candidateFrom(r.ID, r.Title, r.Subtitle, r.CategoryName, r.UpdatedAt,
			r.SearchText, r.InputHashes, r.VectorModelName, r.VectorModelVersion)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// candidateFrom 是两条查询共用的那段转换。
//
// 两条查询的 Row 是两个不同的生成类型（字段完全一样），所以这里按位置收参数
// 而不是收结构体。这也是一道缝：哪天两条查询的 SELECT 列表分了岔，
// 这里的调用点会编译不过，而不是安静地少读一列。
func candidateFrom(id int64, title string, subtitle *string, categoryName string,
	updatedAt pgtype.Timestamptz, searchText *string, inputHashes []byte,
	modelName, modelVersion *string) (IndexCandidate, error) {

	c := IndexCandidate{
		ProductID:          id,
		Title:              title,
		CategoryName:       categoryName,
		UpdatedAt:          updatedAt.Time,
		SearchText:         searchText,
		VectorModelName:    modelName,
		VectorModelVersion: modelVersion,
	}
	if subtitle != nil {
		c.Subtitle = *subtitle
	}
	if len(inputHashes) > 0 {
		// 解析失败要出声，不能当成「从没算过」。当成「从没算过」的话，
		// 一列坏掉的 JSONB 会让这件商品每一轮都被重算一遍，钱一直花，
		// 而没有任何东西报错。
		if err := json.Unmarshal(inputHashes, &c.InputHashes); err != nil {
			return IndexCandidate{}, fmt.Errorf(
				"product %d 的 input_hashes 不是一张字符串表（%s）: %w",
				id, string(inputHashes), err)
		}
	}
	return c, nil
}

func (t tenantTx) LockProductForIndex(ctx context.Context, productID int64) (time.Time, error) {
	ts, err := t.q.LockProductForIndex(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("product %d: %w", productID, ErrProductGoneDuringIndex)
	}
	if err != nil {
		return time.Time{}, err
	}
	return ts.Time, nil
}

func (t tenantTx) UpsertProductTextVector(ctx context.Context, v TextVector) error {
	lit, err := vectorLiteral(fmt.Sprintf("product %d", v.ProductID), v.Embedding)
	if err != nil {
		return err
	}
	if v.ModelName == "" || v.ModelVersion == "" {
		// 这两列是「这批向量要不要重算」的另一半依据（换模型 ⇒ 全量重算）。
		// 空字符串能写进 NOT NULL 列，而它让那个判断永远答不上来。
		return fmt.Errorf("product %d: model_name/model_version 不能为空 —— "+
			"它们是「这批向量是哪个模型算的、要不要重算」的唯一记录（语义检索层 §10）",
			v.ProductID)
	}
	return t.q.UpsertProductTextVector(ctx, db.UpsertProductTextVectorParams{
		ProductID:    v.ProductID,
		Content:      v.Content,
		Embedding:    lit,
		ModelName:    v.ModelName,
		ModelVersion: v.ModelVersion,
	})
}

// vectorLiteral 把一个向量变成 pgvector 的文本字面量，**并在此之前过两道闸门**。
//
// 顺序是先查后拼，不是先拼后查：拼完再查的话，一个坏值已经变成了一个
// 长得完全正常的字符串，而字符串上看不出范数。
//
// subject 是错误信息里的主语（"product 12" / "查询向量"）。它是参数而不是写死的
// 「product %d」，因为这道闸门有两个调用方：入库那一条，以及检索那一条
// （search.go 的 SearchProductsByVector）。**查询向量同样必须归一化** ——
// `<=>` 算的是余弦距离，它对查询侧的模长同样敏感；一个没归一化的查询向量
// 不会报错，只会让距离排序失真，与 §2.3 说的入库侧是同一件事的另一半。
func vectorLiteral(subject string, v []float32) (string, error) {
	if len(v) != inference.Dim {
		return "", fmt.Errorf("%s: %w（%d 维 vs %d 维）",
			subject, ErrVectorWrongDim, len(v), inference.Dim)
	}
	var sum float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			// NaN 能写进 vector 列，而它让这一行与任何查询向量的距离都是 NaN。
			// 排序里 NaN 被当成最大值，于是这件商品从检索结果里彻底消失 ——
			// 一个不报错的、单向的静默失踪。
			return "", fmt.Errorf("%s: 向量里有 NaN 或 Inf，拒绝使用", subject)
		}
		sum += f * f
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1) > inference.NormTolerance {
		return "", fmt.Errorf("%s: %w —— L2 范数是 %.6f，不是 1（容差 %g）。"+
			"配合 vector_cosine_ops 写入未归一化的向量会让距离失真，"+
			"而它入库之后不会报错、不会变慢、检索照常返回结果，"+
			"只会悄悄拉低召回质量（语义检索层 §2.3）",
			subject, ErrVectorNotNormalized, norm, inference.NormTolerance)
	}

	var b strings.Builder
	b.Grow(len(v)*12 + 2)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		// 按 float32 的精度输出：向量列就是 float4[]，多打的位数进不了库，
		// 只会让每一行的字面量白白长出几 KB。
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

func (t tenantTx) SetProductSearchText(ctx context.Context, productID int64, text string) error {
	n, err := t.q.SetProductSearchText(ctx, db.SetProductSearchTextParams{
		ProductID: productID, SearchText: &text,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		// 商品在同一个事务里刚被 LockProductForIndex 锁住，这里匹配不到只可能是
		// 它被别的东西删了（那时锁会先报 ErrProductGoneDuringIndex），
		// 或者 RLS 把这一行挡在外面 —— 后者意味着租户上下文串了。
		return fmt.Errorf("product %d: 写 search_text 匹配到 0 行 —— "+
			"这一行在本租户不可见，租户上下文可能串了", productID)
	}
	return nil
}

// ProductIndexMark 是一次判定留下的痕迹。
//
// Status 是 product_understanding.status（0待处理 1部分完成 2完成 3失败）。
// PipelineVersion 由 service 给：这一层不认得文本模板的版本号，
// 那是 internal/search 的事，而数据访问层不该 import 它。
type ProductIndexMark struct {
	ProductID       int64
	Status          int16
	Hashes          map[string]string
	PipelineVersion string
}

func (t tenantTx) MarkProductIndexed(ctx context.Context, m ProductIndexMark) error {
	if len(m.Hashes) == 0 {
		return fmt.Errorf("product %d: 没有任何指纹要记 —— "+
			"这条写入的全部意义就是记指纹与水位线", m.ProductID)
	}
	if m.PipelineVersion == "" {
		// pipeline_version 是 NOT NULL，空字符串写得进去。它回答的是
		// 「这一行是哪套流水线加工的」——空着的话，将来换流水线时
		// 「哪些行还是老版本算的」这个问题没有答案。
		return fmt.Errorf("product %d: pipeline_version 不能为空", m.ProductID)
	}
	raw, err := json.Marshal(m.Hashes)
	if err != nil {
		return err
	}
	return t.q.MarkProductIndexed(ctx, db.MarkProductIndexedParams{
		ProductID:       m.ProductID,
		Status:          m.Status,
		InputHashes:     raw,
		PipelineVersion: m.PipelineVersion,
	})
}
