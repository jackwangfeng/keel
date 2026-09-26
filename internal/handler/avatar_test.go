package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
)

// 头像归属（PATCH /me 的 avatar_url）：只收本人用 purpose=2 传的 /api/v1/uploads/{id}，
// 与退款凭证同一套核对；设上的标已引用，换掉 / 清掉的旧头像取消引用、交给孤儿回收。

func avatarReferenced(t *testing.T, id int64) bool {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE id = $1 AND referenced`, id) == 1
}

func patchAvatar(t *testing.T, bs buyerShop, b couponBuyer, url string) (api.User, int) {
	t.Helper()
	w := bs.call(t, http.MethodPatch, "/api/v1/me", fmt.Sprintf(`{"avatar_url":%q}`, url), b)
	var u api.User
	if w.Code == http.StatusOK {
		decodeInto(t, w, http.StatusOK, "改头像", &u)
	}
	return u, w.Code
}

func TestAvatarMustBeTheBuyersOwnUpload(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "头像")
	other := bs.newBuyer(t, "别人")
	a1 := mustBuyerUpload(t, bs.couponShop, b, 2, []byte("头像一 "+bs.Suffix))
	a2 := mustBuyerUpload(t, bs.couponShop, b, 2, []byte("头像二 "+bs.Suffix))
	theirs := mustBuyerUpload(t, bs.couponShop, other, 2, []byte("别人的头像 "+bs.Suffix))
	proof := mustBuyerUpload(t, bs.couponShop, b, 3, []byte("我的凭证 "+bs.Suffix))
	productImage := seedUpload(t, bs.adminShop, []byte("商品图 "+bs.Suffix))

	for name, url := range map[string]string{
		"外链":      "https://thirdwx.qlogo.cn/mmopen/x/132",
		"别人的头像":   theirs.Url,
		"自己的退款凭证": proof.Url,
		"商品图":     productImage.Url,
		"带参数的变体":  a1.Url + "?v=2",
		"不存在的 id": "/api/v1/uploads/999999999",
		"路径穿越":    "/api/v1/uploads/../uploads/1",
	} {
		w := bs.call(t, http.MethodPatch, "/api/v1/me", fmt.Sprintf(`{"avatar_url":%q}`, url), b)
		p := problemOf(t, w, http.StatusUnprocessableEntity)
		hit := false
		if p.Errors != nil {
			for _, e := range *p.Errors {
				hit = hit || (e.Field != nil && *e.Field == "avatar_url")
			}
		}
		if !hit {
			t.Errorf("%s 当头像应 422 且点名 avatar_url：%s", name, w.Body.String())
		}
	}
	if avatarReferenced(t, theirs.Id) || avatarReferenced(t, proof.Id) {
		t.Fatal("被拒的请求把别人的头像或凭证标成了已引用")
	}

	// 设上 a1：标已引用。
	if u, code := patchAvatar(t, bs, b, a1.Url); code != http.StatusOK || u.AvatarUrl == nil || *u.AvatarUrl != a1.Url {
		t.Fatalf("设自己的头像：%d %+v", code, u)
	}
	if !avatarReferenced(t, a1.Id) {
		t.Fatal("设上的头像没有标成已引用 —— 24 小时后会被孤儿回收删掉")
	}
	// 换成 a2：a2 已引用，a1 取消引用。
	patchAvatar(t, bs, b, a2.Url)
	if !avatarReferenced(t, a2.Id) || avatarReferenced(t, a1.Id) {
		t.Fatalf("换头像之后：新 %v（期望 true），旧 %v（期望 false）", avatarReferenced(t, a2.Id), avatarReferenced(t, a1.Id))
	}
	// 同一个头像再设一次：不能把它自己取消引用。
	patchAvatar(t, bs, b, a2.Url)
	if !avatarReferenced(t, a2.Id) {
		t.Fatal("把同一个头像再设一次，它被取消了引用 —— 正在用的头像 24 小时后会被删掉")
	}
	// 只改昵称：头像的引用不动。
	wantStatus(t, bs.call(t, http.MethodPatch, "/api/v1/me", `{"nickname":"只改昵称"}`, b), http.StatusOK, "只改昵称")
	if !avatarReferenced(t, a2.Id) {
		t.Fatal("只改昵称把头像取消了引用")
	}
	// 清掉：a2 取消引用。
	if u, code := patchAvatar(t, bs, b, ""); code != http.StatusOK || u.AvatarUrl != nil {
		t.Fatalf("清头像：%d %+v", code, u)
	}
	if avatarReferenced(t, a2.Id) {
		t.Fatal("清掉的头像仍是已引用")
	}

	// 旧地址不是本人的上传：这一版之前存下的外链照常换掉；旧地址恰好指着别人的头像时
	// 也碰不到别人的文件（取消引用只动本人的 purpose=2）。
	patchAvatar(t, bs, other, theirs.Url)
	if !avatarReferenced(t, theirs.Id) {
		t.Fatal("别人设上的头像没有标成已引用")
	}
	adminExec(t, `UPDATE users SET avatar_url = $2 WHERE id = $1`, b.UserID, theirs.Url)
	if _, code := patchAvatar(t, bs, b, a1.Url); code != http.StatusOK {
		t.Fatalf("旧头像是别人的地址时换头像：%d", code)
	}
	if !avatarReferenced(t, theirs.Id) {
		t.Fatal("换头像把别人正在用的头像取消了引用")
	}
	adminExec(t, `UPDATE users SET avatar_url = 'https://img.example.com/legacy.png' WHERE id = $1`, b.UserID)
	if _, code := patchAvatar(t, bs, b, a2.Url); code != http.StatusOK {
		t.Fatalf("旧头像是外链时换头像：%d", code)
	}

	// 换下来的头像交给孤儿回收：创建超过 24 小时即删。
	// （上面那一段把 avatar_url 手工改成过别的地址，a1 还停在已引用；这里走一遍正常的换头像。）
	patchAvatar(t, bs, b, a1.Url)
	patchAvatar(t, bs, b, a2.Url)
	ageUpload(t, a1.Id, 25)
	if _, err := newUploadGC().CollectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if uploadRowExists(t, a1.Id) {
		t.Fatal("换下来、超过 24 小时的旧头像没有被回收")
	}
	if !uploadRowExists(t, a2.Id) {
		t.Fatal("正在用的头像被回收了")
	}
}
