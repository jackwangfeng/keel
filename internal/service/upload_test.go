package service_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 「限时地址真的会过期」这一条只能在这一层验。
//
// handler 那一层验不了：唯一的办法是等 5 分钟（一条要等 5 分钟的测试等于一条
// 不会被跑的测试），而把 exp 改小再打过去验的是签名 —— 改了 exp 签名就对不上，
// 两种拒绝在那一层是同一个响应。所以这里换掉时钟。
//
// 它守的是一条真的会被绕过的规则：把 BlobFor 里那个时间比较删掉，
// handler 那一组测试**一条都不会红**，而每一个发出去过的文件地址从此永久有效。
func TestUploadBlobLinkStopsWorkingAfterItExpires(t *testing.T) {
	const merchantID = 42
	signer := auth.NewSigner([]byte("keel-test-secret-key-32-bytes-long!!"))

	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	clock := base
	svc := service.NewUploadService(uploadRepoStub{}, nil, signer).
		WithClock(func() time.Time { return clock })

	ctx := tenant.NewContext(context.Background(), merchantID)
	target, err := svc.RedirectTarget(ctx, 7)
	if err != nil {
		t.Fatalf("签不出限时地址: %v", err)
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	exp, sig := u.Query().Get("exp"), u.Query().Get("sig")

	// 还没过期：**阳性对照**。没有它，下面那条断言在「BlobFor 恒失败」的
	// 实现下也是绿的。这里的失败来自 store 为 nil（地址本身已经通过校验），
	// 所以判据是「不是 ErrUploadLinkInvalid」。
	clock = base.Add(4 * time.Minute)
	if _, err := svc.BlobFor(ctx, 7, exp, sig); errors.Is(err, service.ErrUploadLinkInvalid) {
		t.Fatalf("地址签出来 4 分钟就被判成无效了（契约：默认有效期 5 分钟）: %v", err)
	}

	// 过期之后。
	clock = base.Add(5*time.Minute + time.Second)
	_, err = svc.BlobFor(ctx, 7, exp, sig)
	if !errors.Is(err, service.ErrUploadLinkInvalid) {
		t.Fatalf("过了 5 分钟之后这个地址还能用（err=%v）—— "+
			"「限时」两个字不成立，每一个发出去过的文件地址都是永久的", err)
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Errorf("过期那条错误是 %q，里面没有「过期」两个字 —— "+
			"它会和「签名对不上」在日志里混成一团", err)
	}
}

// uploadRepoStub 让 RedirectTarget 拿到一条 purpose = 1 的记录。
//
// 用替身而不是真库：这条测试要的是**时钟**，而真库会把它变成一条需要
// 数据库、需要夹具、需要清理的测试 —— 而那些东西一条都不参与被测的逻辑。
type uploadRepoStub struct{}

func (uploadRepoStub) WithTenant(ctx context.Context, fn func(repository.Tx) error) error {
	return fn(uploadTxStub{})
}

type uploadTxStub struct{ repository.Tx }

func (uploadTxStub) FindUpload(_ context.Context, id int64) (repository.Upload, error) {
	return repository.Upload{
		ID:          id,
		Purpose:     repository.UploadPurposeProductImage,
		StorageKey:  "42/ab/abcdef.webp",
		ContentType: "image/webp",
		SizeBytes:   3,
	}, nil
}
