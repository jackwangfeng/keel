package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 店铺设置里商家自己能改的那一半（shop_preferences，00059，数据模型 §2）。
//
// 这张表是普通的 tenant 类表：读写都在 WithTenant 的事务里，RLS 把一个事务钉在
// 自己那一行上。原先 timezone / auto_confirm_days 在 shop_settings（tenant-root，
// 没有 RLS）上、走裸 SQL 读；00059 把它们搬了过来，理由写在那份迁移的文件头。

// 三列的列默认值（00059）。一家店没有 shop_preferences 那一行（开店不写它、
// 从没改过设置）时按它们算。
//
// 它们必须与列默认值是同一个数：两者分叉的后果是「改过店铺设置的店」与「没改过的店」
// 在同一个默认之下按不同的天数 / 时区跑，而谁都没改过这个配置。
// internal/handler 的店铺设置测试逐列核对。
const (
	DefaultAutoConfirmDays = 7
	DefaultShopTimezone    = "Asia/Shanghai"
	DefaultReturnShipDays  = 7
)

// ShopPreferences 是一家店可改的设置。UpdatedAt 为 nil 表示这家店还没有那一行，
// 其余字段是列默认值。
type ShopPreferences struct {
	ServicePhone    *string
	Timezone        string
	AutoConfirmDays int
	ReturnShipDays  int
	UpdatedAt       *time.Time
}

// DefaultShopPreferences 是没有那一行时的样子。
func DefaultShopPreferences() ShopPreferences {
	return ShopPreferences{
		Timezone:        DefaultShopTimezone,
		AutoConfirmDays: DefaultAutoConfirmDays,
		ReturnShipDays:  DefaultReturnShipDays,
	}
}

// ShopPreferencesTx 是店铺设置这一面。
type ShopPreferencesTx interface {
	// ShopPreferences 本店的设置；没有那一行时返回 DefaultShopPreferences()。
	ShopPreferences(ctx context.Context) (ShopPreferences, error)
	// ReplaceShopPreferences 整体替换（没有那一行就插一行），返回写后的值。
	// 字段的合法性由 service 判；越界的值在这里会撞上 CHECK（23514）。
	ReplaceShopPreferences(ctx context.Context, p ShopPreferences) (ShopPreferences, error)
}

func (t tenantTx) ShopPreferences(ctx context.Context) (ShopPreferences, error) {
	r, err := t.q.GetShopPreferences(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultShopPreferences(), nil
	}
	if err != nil {
		return ShopPreferences{}, err
	}
	return ShopPreferences{
		ServicePhone: r.ServicePhone, Timezone: r.Timezone,
		AutoConfirmDays: int(r.AutoConfirmDays), ReturnShipDays: int(r.ReturnShipDays),
		UpdatedAt: timePtr(r.UpdatedAt),
	}, nil
}

func (t tenantTx) ReplaceShopPreferences(ctx context.Context, p ShopPreferences) (ShopPreferences, error) {
	r, err := t.q.UpsertShopPreferences(ctx, db.UpsertShopPreferencesParams{
		ServicePhone:    p.ServicePhone,
		Timezone:        p.Timezone,
		AutoConfirmDays: int16(p.AutoConfirmDays),
		ReturnShipDays:  int16(p.ReturnShipDays),
	})
	if err != nil {
		return ShopPreferences{}, err
	}
	return ShopPreferences{
		ServicePhone: r.ServicePhone, Timezone: r.Timezone,
		AutoConfirmDays: int(r.AutoConfirmDays), ReturnShipDays: int(r.ReturnShipDays),
		UpdatedAt: timePtr(r.UpdatedAt),
	}, nil
}

// shopPreferences 在一个新的租户事务里读本店设置。给只要其中一项、手上又没有
// 事务的调用方（定时任务读天数、报表读时区）用。
func (r *Repo) shopPreferences(ctx context.Context) (ShopPreferences, error) {
	var p ShopPreferences
	err := r.WithTenant(ctx, func(tx Tx) error {
		var err error
		p, err = tx.ShopPreferences(ctx)
		return err
	})
	return p, err
}

// AutoConfirmDays 取本租户的「发货后多少天自动确认收货」（数据模型 §5 发货第三条规则）。
// 没有那一行时返回 DefaultAutoConfirmDays，而不是「不自动确认」：
// 后者的症状是一家新店的订单永远停在已发货，而表面上一切正常。
func (r *Repo) AutoConfirmDays(ctx context.Context) (int, error) {
	p, err := r.shopPreferences(ctx)
	return p.AutoConfirmDays, err
}

// ReturnShipDays 取本租户的「退货审核通过后多少天未寄回自动关闭」。
func (r *Repo) ReturnShipDays(ctx context.Context) (int, error) {
	p, err := r.shopPreferences(ctx)
	return p.ReturnShipDays, err
}

// ShopTimezone 取本租户的店铺时区名（IANA，如 Asia/Shanghai）。经营报表按它切自然日。
// 没有那一行时返回 DefaultShopTimezone。**不校验**它是不是合法的时区名：
// 那是 service 的事（它要 time.LoadLocation，失败时同样回落到默认值）——
// 写入口（PUT /admin/shop-settings）已经校验过，这里兜的是手工改库。
func (r *Repo) ShopTimezone(ctx context.Context) (string, error) {
	p, err := r.shopPreferences(ctx)
	return p.Timezone, err
}
