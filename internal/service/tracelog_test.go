package service

import (
	"context"
	"errors"
	"testing"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/traceid"
)

// 这一层的判据只有一条：**非管理员一律拿不到开关**。而它不是自动成立的 ——
// 首版实现里handler 声明了「只给管理员」，代码里却只挂了 staffAuth（它只验身份），
// 于是运营把采样率打到 100 能成功。TestAdminPermissionMatrix 抓到之后才补上这一层。
//
// 所以这里的用例要能构造出「**运营 / 大区管理员 / 门店管理员**都进不来」的情形，
// 而不是只测管理员能进。

func TestTraceLogStateAdminOnly(t *testing.T) {
	t.Cleanup(func() { traceid.SetSampleRate(0); traceid.ForceOff("0123456789abcdef0123456789abcdef") })

	for _, role := range []int16{auth.StaffRoleOperator, 3, 4} {
		name := map[int16]string{auth.StaffRoleOperator: "操作员", 3: "大区管理员", 4: "门店管理员"}[role]
		t.Run(name+"不能读", func(t *testing.T) {
			s := NewTraceLogService()
			_, err := s.Get(ctxWithStaffRole(role))
			if !errors.Is(err, ErrRoleForbidden) {
				t.Fatalf("Get 返回 %v，期望 ErrRoleForbidden", err)
			}
		})
		t.Run(name+"不能写", func(t *testing.T) {
			s := NewTraceLogService()
			n := int64(100)
			_, err := s.Set(ctxWithStaffRole(role), &n, "", nil)
			if !errors.Is(err, ErrRoleForbidden) {
				t.Fatalf("Set 返回 %v，期望 ErrRoleForbidden", err)
			}
			// 权限不够时**绝不能把采样率改了** —— 只判错误不判副作用，
			// 会得到「403 但开关真的动了」这一种最难查的状态。
			if traceid.SampleRate() != 0 {
				t.Fatalf("被拒之后采样率是 %d，它真的被改了", traceid.SampleRate())
			}
		})
	}

	t.Run("管理员能读能写", func(t *testing.T) {
		s := NewTraceLogService()
		st, err := s.Get(ctxWithStaffRole(auth.StaffRoleAdmin))
		if err != nil {
			t.Fatalf("管理员 Get 失败：%v", err)
		}
		if st.SampleRate != 0 || st.SlowMillis != 200 {
			t.Fatalf("默认值不对：sample_rate=%d slow=%d", st.SampleRate, st.SlowMillis)
		}
		n := int64(100)
		if _, err := s.Set(ctxWithStaffRole(auth.StaffRoleAdmin), &n, "", nil); err != nil {
			t.Fatalf("管理员 Set 失败：%v", err)
		}
		if traceid.SampleRate() != 100 {
			t.Fatalf("采样率是 %d，管理员改不动", traceid.SampleRate())
		}
	})
}

func TestTraceLogSetBadTraceID(t *testing.T) {
	t.Cleanup(func() { traceid.ForceOff("0123456789abcdef0123456789abcdef") })
	ctx := ctxWithStaffRole(auth.StaffRoleAdmin)

	t.Run("形状不对报参数错", func(t *testing.T) {
		yes := true
		_, err := NewTraceLogService().Set(ctx, nil, "not-a-valid-id", &yes)
		if !errors.Is(err, ErrStaffBadRequest) {
			t.Fatalf("返回 %v，期望 ErrStaffBadRequest", err)
		}
	})

	t.Run("移一个不在白名单的要说清楚", func(t *testing.T) {
		no := false
		_, err := NewTraceLogService().Set(ctx, nil, "0123456789abcdef0123456789abcdef", &no)
		if err == nil || !errors.Is(err, ErrStaffBadRequest) {
			t.Fatalf("移一个不在白名单的号返回 %v，应当报参数错", err)
		}
	})

	t.Run("空请求体是空操作且不改任何东西", func(t *testing.T) {
		traceid.SetSampleRate(25)
		t.Cleanup(func() { traceid.SetSampleRate(0) })
		before := traceid.SampleRate()
		st, err := NewTraceLogService().Set(ctx, nil, "", nil)
		if err != nil {
			t.Fatalf("空操作返回 %v", err)
		}
		if traceid.SampleRate() != before {
			t.Fatalf("空操作把采样率从 %d 改成了 %d", before, traceid.SampleRate())
		}
		if st.SampleRate != before {
			t.Fatalf("回给调用方的状态是 %d，进程里是 %d", st.SampleRate, traceid.SampleRate())
		}
	})
}

// ctxWithStaffRole造一个「身份是真的、只有角色不同」的 ctx。
//
// 照抄 compliance_test.go 的 staffContext，只把 Role 变成参数 ——
// 身份是伪造的时权限判断就没意义了，而零值 Role（0）既不是管理员也不是
// 操作员，拿它测会得到「因为角色非法而被拒」而不是「因为角色不够而被拒」。
func ctxWithStaffRole(role int16) context.Context {
	merchant := int64(1)
	return auth.NewStaffContext(context.Background(), auth.StaffIdentity{
		StaffID:    1,
		SessionID:  1,
		MerchantID: &merchant,
		Role:       role,
		Status:     auth.StaffStatusActive,
	})
}
