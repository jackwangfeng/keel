package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// AdminCategoryTx 是商家写路径上类目这一面。
type AdminCategoryTx interface {
	// AdminListCategories 返回**扁平**的分类数组，按 path 升序，含停用、不含软删。
	AdminListCategories(ctx context.Context) ([]AdminCategory, error)

	// FindCategory 取一个未软删的分类。查不到返回 ErrCatalogNotFound。
	FindCategory(ctx context.Context, id int64) (AdminCategory, error)

	// CreateCategory 建一个分类。path 与 level 由服务端从 parent_id 算出，
	// 不是入参 —— 让客户端传 path 等于让前端去维护一个索引的内容。
	CreateCategory(ctx context.Context, n NewCategory) (AdminCategory, error)

	// UpdateCategory 改名 / 排序 / 启停。**不含 parent_id**，移动子树走 MoveCategory。
	UpdateCategory(ctx context.Context, id int64, p CategoryPatch) (AdminCategory, error)

	// MoveCategory 把一个分类连同**它全部后代**挪到新父节点下，
	// 在同一个事务里重写整棵子树的 path 与 level。
	// parentID 为 nil 表示移到根。成环返回 ErrCategoryCycle。
	MoveCategory(ctx context.Context, id int64, parentID *int64) (AdminCategory, error)

	// SoftDeleteCategory 置 deleted_at。两条闸门：ErrCategoryHasChildren 与
	// ErrCategoryHasProducts，都是契约的 409。
	SoftDeleteCategory(ctx context.Context, id int64) error
}

// NewCategory 是建分类的入参。没有 Path / Level —— 它们是派生的。
type NewCategory struct {
	ParentID  *int64
	Name      string
	SortOrder int32
}

// CategoryPatch 是改分类的入参，每个字段 nil 表示「不动」。
//
// ParentID 刻意不在这里：改父节点意味着整棵子树的 path 与 level 都要跟着改，
// 那是 MoveCategory 的事。混进这个结构体会让「只改了 parent_id、忘了重写 path」
// 成为一句写得出来的代码，而那种错不报警 —— path 是分类查询的主索引，
// 子树的 path 没跟着走，那些商品在新位置下查不出来，在旧位置下还查得出来。
type CategoryPatch struct {
	Name      *string
	SortOrder *int32
	Status    *int16
}

func (t tenantTx) AdminListCategories(ctx context.Context) ([]AdminCategory, error) {
	rows, err := t.q.AdminListCategories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AdminCategory, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminCategory{
			ID: r.ID, ParentID: r.ParentID, Name: r.Name, Path: r.Path,
			Level: r.Level, SortOrder: r.SortOrder, Status: r.Status,
			DeletedAt: optTime(r.DeletedAt),
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (t tenantTx) FindCategory(ctx context.Context, id int64) (AdminCategory, error) {
	r, err := t.q.GetCategory(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminCategory{}, fmt.Errorf("category %d: %w", id, ErrCatalogNotFound)
	}
	if err != nil {
		return AdminCategory{}, err
	}
	return AdminCategory{
		ID: r.ID, ParentID: r.ParentID, Name: r.Name, Path: r.Path,
		Level: r.Level, SortOrder: r.SortOrder, Status: r.Status,
		DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

// CreateCategory 建一个分类。
//
// 两条语句，不是一条：path 形如 /1/23/456/，**含自己的 id**，而 id 是
// GENERATED ALWAYS AS IDENTITY，插入之前不知道。压进一条语句的写法
// （WITH ins AS (INSERT ... RETURNING id) UPDATE ... WHERE id = (SELECT ...)）
// 在 PostgreSQL 里是**静默无效**的：同一条语句里的各个数据修改 CTE 共享同一个
// 快照，UPDATE 看不到 INSERT 刚插出来的行，于是它更新 0 行、不报任何错，
// 而 path 永远停在空串上 —— 那会让 idx_categories_path 上的前缀查询对这一支
// 整个失效，而且没有任何东西会红。
//
// 两条语句都在 WithTenant 的事务里，所以「半个分类」不会被提交出去。
func (t tenantTx) CreateCategory(ctx context.Context, n NewCategory) (AdminCategory, error) {
	prefix := "/"
	level := int16(1)
	if n.ParentID != nil {
		parent, err := t.FindCategory(ctx, *n.ParentID)
		if err != nil {
			// 父分类不属于当前租户（RLS 挡掉）或不存在 → 契约的 404。
			// 跨租户挂接在库里也是不可能的：categories.parent_id 走自引用的
			// 复合外键 (parent_id, merchant_id)（00004）。两道闸门。
			return AdminCategory{}, err
		}
		prefix = parent.Path
		level = parent.Level + 1
	}

	id, err := t.q.CreateCategoryRow(ctx, db.CreateCategoryRowParams{
		ParentID:  n.ParentID,
		Name:      n.Name,
		Level:     level,
		SortOrder: n.SortOrder,
	})
	if err != nil {
		if isForeignKeyViolation(err) {
			return AdminCategory{}, fmt.Errorf("parent category: %w", ErrCatalogNotFound)
		}
		return AdminCategory{}, err
	}

	r, err := t.q.SetCategoryPath(ctx, db.SetCategoryPathParams{
		ID:   id,
		Path: prefix + strconv.FormatInt(id, 10) + "/",
	})
	if err != nil {
		return AdminCategory{}, err
	}
	return AdminCategory{
		ID: r.ID, ParentID: r.ParentID, Name: r.Name, Path: r.Path,
		Level: r.Level, SortOrder: r.SortOrder, Status: r.Status,
		DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (t tenantTx) UpdateCategory(ctx context.Context, id int64, p CategoryPatch) (AdminCategory, error) {
	r, err := t.q.UpdateCategory(ctx, db.UpdateCategoryParams{
		ID:        id,
		Name:      p.Name,
		SortOrder: p.SortOrder,
		Status:    p.Status,
	})
	if err != nil {
		return AdminCategory{}, err
	}
	if r.VisibleRows == 0 {
		return AdminCategory{}, fmt.Errorf("category %d: %w", id, ErrCatalogNotFound)
	}
	if r.UpdatedRows == 0 || r.ID == nil {
		return AdminCategory{}, fmt.Errorf("category %d 可见却没改成——UpdateCategory 的 SQL 被改坏了", id)
	}
	return AdminCategory{
		ID: *r.ID, ParentID: r.ParentID, Name: *r.Name, Path: *r.Path,
		Level: *r.Level, SortOrder: *r.SortOrder, Status: *r.Status,
		DeletedAt: optTime(r.DeletedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

// MoveCategory 移动子树。
//
// 三步，必须在同一个事务里（WithTenant 保证了这一点）：
//
//	① 判环。判据就用 path：目标.path 以 自己.path 打头即成环
//	   （目标是自己，或是自己的后代）。没有这一条，一次误操作就能把一棵子树
//	   从树上摘下来变成一个独立的环，而第 ③ 步的 path 重写会在那个环上跑不完。
//	② 改自己的 parent_id。
//	③ 重写自己**以及全部后代**的 path 与 level。
//
// 少了第 ③ 步的后果不是「数据不太整齐」：path 是分类查询的主索引，
// 子树的 path 没跟着走，那些商品在新位置下就查不出来了，而在旧位置下还查得出来。
func (t tenantTx) MoveCategory(ctx context.Context, id int64, parentID *int64) (AdminCategory, error) {
	self, err := t.FindCategory(ctx, id)
	if err != nil {
		return AdminCategory{}, err
	}

	newPrefix := "/"
	newLevel := int16(1)
	if parentID != nil {
		if *parentID == id {
			return AdminCategory{}, fmt.Errorf("category %d 移到自己下面: %w", id, ErrCategoryCycle)
		}
		parent, err := t.FindCategory(ctx, *parentID)
		if err != nil {
			return AdminCategory{}, err
		}
		// 目标父节点是自己的后代 → 成环。用 path 判而不是递归往上爬：
		// 物化路径存在的全部意义就是让这类问题变成一次字符串比较。
		if strings.HasPrefix(parent.Path, self.Path) {
			return AdminCategory{}, fmt.Errorf(
				"category %d 的目标父节点 %d 是它自己的后代: %w", id, *parentID, ErrCategoryCycle)
		}
		newPrefix = parent.Path
		newLevel = parent.Level + 1
	}

	if _, err := t.q.SetCategoryParent(ctx, db.SetCategoryParentParams{
		ID:       id,
		ParentID: parentID,
	}); err != nil {
		if isForeignKeyViolation(err) {
			return AdminCategory{}, fmt.Errorf("parent category: %w", ErrCatalogNotFound)
		}
		return AdminCategory{}, err
	}

	if _, err := t.q.MoveCategorySubtree(ctx, db.MoveCategorySubtreeParams{
		NewPrefix:  newPrefix + strconv.FormatInt(id, 10) + "/",
		OldPrefix:  self.Path,
		LevelDelta: int32(newLevel) - int32(self.Level),
	}); err != nil {
		return AdminCategory{}, err
	}

	return t.FindCategory(ctx, id)
}

func (t tenantTx) SoftDeleteCategory(ctx context.Context, id int64) error {
	// 两条闸门在删之前查。它们**不能**靠数据库兜底：软删不删行，
	// 所以复合外键在数据库看来一直是完整的 —— 这两件事只有这里挡得住。
	//
	// 与 SKU / 商品那几条不同，这里是先查后改的两次快照。承认这个窗口：
	// 一个并发的「建子分类」可以插在两者之间。代价是一棵子树的祖先被软删，
	// 而它的补救是一次查询就能发现的（path 指向一个 deleted 的祖先）；
	// 把它压进一条语句要在 UPDATE 的 WHERE 里塞两个 NOT EXISTS，
	// 而那会让「为什么没删成」的答案退化回一个 0，又得再查一次才知道是哪一条。
	children, err := t.q.CountLiveCategoryChildren(ctx, &id)
	if err != nil {
		return err
	}
	if children > 0 {
		return fmt.Errorf("category %d 下还有 %d 个子分类: %w", id, children, ErrCategoryHasChildren)
	}
	products, err := t.q.CountLiveCategoryProducts(ctx, id)
	if err != nil {
		return err
	}
	if products > 0 {
		return fmt.Errorf("category %d 下还有 %d 件商品: %w", id, products, ErrCategoryHasProducts)
	}

	r, err := t.q.SoftDeleteCategory(ctx, id)
	if err != nil {
		return err
	}
	if r.VisibleRows == 0 {
		return fmt.Errorf("category %d: %w", id, ErrCatalogNotFound)
	}
	if r.DeletedRows == 0 {
		// 看得见却没删成，只可能是已经删过了 —— 契约同样是 404。
		return fmt.Errorf("category %d 已经软删过: %w", id, ErrCatalogNotFound)
	}
	return nil
}
