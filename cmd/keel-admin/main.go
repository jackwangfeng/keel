// Command keel-admin：运维侧的后台入口工具。现在只有一个子命令：签发一次性登录 token。
//
//	keel-admin issue-login -staff 1
//	keel-admin issue-login -email platform@example.com
//	keel-admin issue-login -email boss@shop.com -merchant shop-a
//
// 解决的问题：后台没有密码，邮箱链接回 501，会话过期之后若没人能「重签登录 token」
// 就进不去——引导通道又只在「库里没有在岗平台管理员」时开。这条命令用**管理员连接**
// （与 make migrate 同一套 KEEL_ADMIN_* / PG*）直接签一串 kind=2 的 token，明文打到
// stdout，**不进 app 日志**。
//
// 权限模型：谁能连上维护库，谁就能签——与「能跑 migrate」同一道门。不要把它做成
// 公网 HTTP；脚本入口见 scripts/issue-login-token.sh。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "issue-login" {
		fmt.Fprintln(os.Stderr, "用法：")
		fmt.Fprintln(os.Stderr, "  keel-admin issue-login -staff <id>")
		fmt.Fprintln(os.Stderr, "  keel-admin issue-login -email <addr> [-merchant <code>]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("issue-login", flag.ExitOnError)
	staffID := fs.Int64("staff", 0, "员工 id（与 -email 二选一）")
	email := fs.String("email", "", "员工邮箱（与 -staff 二选一；商家级要加 -merchant）")
	merchant := fs.String("merchant", "", "商家 code；与 -email 合用定位商家级员工；平台级不要传")
	_ = fs.Parse(os.Args[2:])

	token, meta, err := issueLogin(context.Background(), *staffID, strings.TrimSpace(*email), strings.TrimSpace(*merchant))
	if err != nil {
		fmt.Fprintln(os.Stderr, "签发失败:", err)
		os.Exit(1)
	}
	// 人读的信息进 stderr；stdout 只有明文，方便管道 / 复制。
	fmt.Fprintln(os.Stderr, meta)
	fmt.Fprintln(os.Stdout, token)
}

type staffRow struct {
	ID         int64
	Email      string
	Status     int16
	Kind       int16
	MerchantID *int64
	Code       *string
}

func issueLogin(ctx context.Context, staffID int64, email, merchantCode string) (token string, meta string, err error) {
	if (staffID <= 0) == (email == "") {
		return "", "", fmt.Errorf("请指定 -staff 或 -email 之一")
	}
	if merchantCode != "" && email == "" {
		return "", "", fmt.Errorf("-merchant 只能与 -email 合用")
	}

	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return "", "", fmt.Errorf("连维护库失败（检查 PGHOST/PGPORT/KEEL_ADMIN_*）: %w", err)
	}
	defer conn.Close(ctx)

	var s staffRow
	switch {
	case staffID > 0:
		err = conn.QueryRow(ctx, `
			SELECT s.id, s.email, s.status, s.kind, s.merchant_id, m.code
			  FROM staff s
			  LEFT JOIN merchants m ON m.id = s.merchant_id
			 WHERE s.id = $1 AND s.deleted_at IS NULL`, staffID).
			Scan(&s.ID, &s.Email, &s.Status, &s.Kind, &s.MerchantID, &s.Code)
	case merchantCode == "":
		err = conn.QueryRow(ctx, `
			SELECT s.id, s.email, s.status, s.kind, s.merchant_id, NULL::text
			  FROM staff s
			 WHERE s.email = $1 AND s.merchant_id IS NULL AND s.deleted_at IS NULL`, email).
			Scan(&s.ID, &s.Email, &s.Status, &s.Kind, &s.MerchantID, &s.Code)
	default:
		err = conn.QueryRow(ctx, `
			SELECT s.id, s.email, s.status, s.kind, s.merchant_id, m.code
			  FROM staff s
			  JOIN merchants m ON m.id = s.merchant_id AND m.code = $2 AND m.deleted_at IS NULL
			 WHERE s.email = $1 AND s.deleted_at IS NULL`, email, merchantCode).
			Scan(&s.ID, &s.Email, &s.Status, &s.Kind, &s.MerchantID, &s.Code)
	}
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", "", fmt.Errorf("找不到这个员工（已删 / 邮箱与商家对不上 / id 不对）")
		}
		return "", "", err
	}
	if s.Kind != 1 {
		return "", "", fmt.Errorf("staff_id=%d 是 AI 员工（kind=2），登录走接入密钥 kagt_…，不是这条命令", s.ID)
	}
	if s.Status != auth.StaffStatusActive {
		return "", "", fmt.Errorf("staff_id=%d 未在岗（status=%d），先启用再签", s.ID, s.Status)
	}

	plain, err := auth.NewOpaqueToken()
	if err != nil {
		return "", "", err
	}
	expireAt := time.Now().UTC().Add(auth.StaffEmailLinkTTL)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 与 ReissueLoginToken 同一句：旧的还活着的登录链接一并作废。
	if _, err := tx.Exec(ctx, `
		UPDATE staff_tokens
		   SET revoked_at = now()
		 WHERE staff_id = $1 AND kind = $2
		   AND used_at IS NULL AND revoked_at IS NULL AND expire_at > now()`,
		s.ID, repository.StaffTokenEmailLink); err != nil {
		return "", "", fmt.Errorf("作废旧 token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
		VALUES ($1, $2, $3, $4)`,
		s.ID, auth.HashStaffToken(plain), repository.StaffTokenEmailLink, expireAt); err != nil {
		return "", "", fmt.Errorf("写入新 token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}

	scope := "平台级"
	if s.Code != nil {
		scope = "商家 " + *s.Code
	}
	meta = fmt.Sprintf("staff_id=%d email=%s（%s）expire_at=%s（UTC，15 分钟，用掉即失效）\n粘到登录页「已有登录 token」。再签一次会作废这一串。",
		s.ID, s.Email, scope, expireAt.Format(time.RFC3339))
	return plain, meta, nil
}
