package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// AI 员工与接入密钥（AI 经营 M9 任务 1，docs/AI经营-M9设计.md §2）。
//
// AI 员工是 kind = 2 的 staff：角色与管辖范围复用人的那一套（staff_scopes、checkRoleScopes），
// 于是它之后调用任何能力都走和人逐字相同的判权（authorizeStore 等）。与人的差别只有三条，都是安全边界：
//   - 不许是管理员（库上有 CHECK，这里先给一句读得懂的 422）；
//   - 不能登录后台（人的令牌 / 会话查询只认 kind = 1）；
//   - 只能用接入密钥（kagt_…）进来，密钥只存 sha256、明文只在创建时回一次。
//
// 管 AI 员工只有**本店管理员**能做：它等于给一个外部程序发一把长期有效的钥匙。
// 平台级管理员不行 —— 密钥与 AI 员工的 created_by 带复合外键指向本店的员工，平台员工不属于任何一家店。

// AgentKeyPrefix 是接入密钥明文的前缀：一眼认得出、也方便密钥扫描工具按前缀告警。
const AgentKeyPrefix = "kagt_"

// agentKeyRandomBytes 是明文的随机部分（256 bit）。
const agentKeyRandomBytes = 32

// agentKeyPrefixLen 是列表里显示的明文前几位（含 kagt_），给人认是哪一把。
const agentKeyPrefixLen = 12

// MaxAgentKeyDays 是接入密钥的最长有效期（天）；0 表示不过期（演示站的定时 agent 用）。
const MaxAgentKeyDays = 3650

var (
	// ErrAgentNotFound：这个 id 不是本店的 AI 员工。契约 404。
	ErrAgentNotFound = errors.New("AI 员工不存在")
	// ErrAgentKeyNotFound：要吊销的密钥不存在、已吊销或不属于这名 AI 员工。契约 404。
	ErrAgentKeyNotFound = errors.New("接入密钥不存在或已吊销")
	// ErrAgentKeyInvalid：接入时密钥无效（形状不对、查不到、吊销、过期）。契约 401，不区分原因。
	ErrAgentKeyInvalid = errors.New("接入密钥无效")
)

// AgentView 是后台看到的一名 AI 员工（含管辖范围与密钥元数据）。
type AgentView struct {
	repository.Agent
	Scopes repository.StaffScopes
	Keys   []repository.AgentKey
}

// AgentKeyIssued 是新发的一把密钥：Secret 只在这一次响应里出现。
type AgentKeyIssued struct {
	Key    repository.AgentKey
	Secret string
}

// requireShopAdmin：本店管理员（不是平台级）。AI 员工与密钥的全部写操作都要它。
func requireShopAdmin(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireMerchantAdmin(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if id.Platform() {
		return auth.StaffIdentity{}, fmt.Errorf("%w: AI 员工只能由本店管理员管理，平台级账号不行", ErrRoleForbidden)
	}
	return id, nil
}

func checkAgentRole(role int16) error {
	switch role {
	case auth.StaffRoleOperator, auth.StaffRoleRegionManager, auth.StaffRoleStoreManager:
		return nil
	case auth.StaffRoleAdmin:
		return fmt.Errorf("%w: AI 员工不能是管理员（能改店铺设置、设默认门店、管员工）", ErrStaffBadRequest)
	default:
		return fmt.Errorf("%w: role 只能是 2 操作员 / 3 大区管理员 / 4 门店管理员", ErrStaffBadRequest)
	}
}

func agentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 50 {
		return "", fmt.Errorf("%w: name 必填，不超过 50 字", ErrStaffBadRequest)
	}
	return name, nil
}

// scopeAdminAgentCreate 是新建 AI 员工的幂等作用域。
const scopeAdminAgentCreate = "admin.agents.create"

// CreateAgent 实现 POST /admin/agents（Idempotency-Key 必填：重试不会建出第二名 AI 员工）。
func (s *StaffService) CreateAgent(ctx context.Context, name string, role int16,
	scopes repository.StaffScopes, idemKey string) (AgentView, bool, error) {
	id, err := requireShopAdmin(ctx)
	if err != nil {
		return AgentView{}, false, err
	}
	if idemKey == "" {
		return AgentView{}, false, ErrIdempotencyKeyMissing
	}
	if name, err = agentName(name); err != nil {
		return AgentView{}, false, err
	}
	if err := checkAgentRole(role); err != nil {
		return AgentView{}, false, err
	}
	scopes = repository.StaffScopes{RegionIDs: normalizeIDs(scopes.RegionIDs), StoreIDs: normalizeIDs(scopes.StoreIDs)}
	hash, err := adminRequestHash(nil, struct {
		Name      string  `json:"name"`
		Role      int16   `json:"role"`
		RegionIDs []int64 `json:"region_ids"`
		StoreIDs  []int64 `json:"store_ids"`
	}{name, role, scopes.RegionIDs, scopes.StoreIDs})
	if err != nil {
		return AgentView{}, false, err
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return AgentView{}, false, err
	}
	// staff.email 非空：合成一个不可投递的地址（.invalid 顶级域）。邮件登录这条找回通道对 AI 员工天然不通。
	email := "agent-" + hex.EncodeToString(buf[:]) + "@agent.keel.invalid"
	return idempotentTx(ctx, s.repo, repository.StaffSubject(id.StaffID), scopeAdminAgentCreate, idemKey, hash,
		archivedCreated, func(tx repository.Tx) (AgentView, error) {
			if err := checkRoleScopes(ctx, tx, false, role, scopes); err != nil {
				return AgentView{}, err
			}
			a, err := tx.CreateAgentStaff(ctx, email, name, role, id.StaffID)
			if err != nil {
				return AgentView{}, err
			}
			if err := tx.ReplaceStaffScopes(ctx, a.ID, scopes); err != nil {
				return AgentView{}, err
			}
			return AgentView{Agent: a, Scopes: scopes, Keys: []repository.AgentKey{}}, nil
		})
}

// ListAgents 实现 GET /admin/agents。管理员才看得到（列表本身就是「谁拿着钥匙」）。
//
// 每一名都带上管辖范围（契约 AdminAgent.region_ids / store_ids 必返）：此前列表只回空数组，
// 后台「管辖范围」一列对大区 / 门店管理员永远显示「—」，看上去像范围没生效（判权一直是按库里的范围走的）。
// AI 员工一家店只有几名，逐个查范围不值得为它另写一条聚合查询。
func (s *StaffService) ListAgents(ctx context.Context) ([]AgentView, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return nil, err
	}
	var out []AgentView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		agents, e := tx.ListAgents(ctx)
		if e != nil {
			return e
		}
		out = make([]AgentView, 0, len(agents))
		for _, a := range agents {
			sc, e := tx.ListStaffScopes(ctx, a.ID)
			if e != nil {
				return e
			}
			out = append(out, AgentView{Agent: a, Scopes: sc})
		}
		return nil
	})
	return out, err
}

// GetAgent 实现 GET /admin/agents/{staff_id}：含管辖范围与全部密钥（元数据）。
func (s *StaffService) GetAgent(ctx context.Context, staffID int64) (AgentView, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return AgentView{}, err
	}
	var out AgentView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		return loadAgentView(ctx, tx, staffID, &out)
	})
	return out, err
}

func loadAgentView(ctx context.Context, tx repository.Tx, staffID int64, out *AgentView) error {
	a, err := tx.FindAgent(ctx, staffID)
	if errors.Is(err, repository.ErrAgentNotFound) {
		return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, staffID)
	}
	if err != nil {
		return err
	}
	sc, err := tx.ListStaffScopes(ctx, staffID)
	if err != nil {
		return err
	}
	keys, err := tx.ListAgentKeys(ctx, staffID)
	if err != nil {
		return err
	}
	*out = AgentView{Agent: a, Scopes: sc, Keys: keys}
	return nil
}

// UpdateAgent 实现 PATCH /admin/agents/{staff_id}：改名、改角色 / 范围、停用。
// 角色与范围一起判（checkRoleScopes），与改人的员工同一条规则。
func (s *StaffService) UpdateAgent(ctx context.Context, staffID int64, name *string, role, status *int16,
	regionIDs, storeIDs *[]int64) (AgentView, error) {
	if _, err := requireShopAdmin(ctx); err != nil {
		return AgentView{}, err
	}
	if name == nil && role == nil && status == nil && regionIDs == nil && storeIDs == nil {
		return AgentView{}, fmt.Errorf("%w: name、role、status、region_ids、store_ids 至少给一个", ErrStaffBadRequest)
	}
	if name != nil {
		n, err := agentName(*name)
		if err != nil {
			return AgentView{}, err
		}
		name = &n
	}
	if role != nil {
		if err := checkAgentRole(*role); err != nil {
			return AgentView{}, err
		}
	}
	if status != nil && *status != auth.StaffStatusActive && *status != auth.StaffStatusDisabled {
		return AgentView{}, fmt.Errorf("%w: status 只能是 1 或 2", ErrStaffBadRequest)
	}
	var out AgentView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var before AgentView
		if err := loadAgentView(ctx, tx, staffID, &before); err != nil {
			return err
		}
		newRole := before.Role
		if role != nil {
			newRole = *role
		}
		sc := before.Scopes
		if regionIDs != nil {
			sc.RegionIDs = normalizeIDs(*regionIDs)
		}
		if storeIDs != nil {
			sc.StoreIDs = normalizeIDs(*storeIDs)
		}
		// 改成全店范围的角色时，旧的范围一并清掉 —— 否则 checkRoleScopes 会因为「操作员不带范围」拒掉一次合法的改角色。
		if role != nil && (newRole == auth.StaffRoleOperator) && regionIDs == nil && storeIDs == nil {
			sc = repository.StaffScopes{}
		}
		if role != nil || regionIDs != nil || storeIDs != nil {
			if err := checkRoleScopes(ctx, tx, false, newRole, sc); err != nil {
				return err
			}
		}
		if _, err := tx.UpdateAgent(ctx, staffID, name, role, status); err != nil {
			if errors.Is(err, repository.ErrAgentNotFound) {
				return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, staffID)
			}
			return err
		}
		if role != nil || regionIDs != nil || storeIDs != nil {
			if err := tx.ReplaceStaffScopes(ctx, staffID, sc); err != nil {
				return err
			}
		}
		return loadAgentView(ctx, tx, staffID, &out)
	})
	return out, err
}

// CreateAgentKey 实现 POST /admin/agents/{staff_id}/keys。expiresInDays = 0 表示不过期。
func (s *StaffService) CreateAgentKey(ctx context.Context, staffID int64, name string,
	expiresInDays int) (AgentKeyIssued, error) {
	id, err := requireShopAdmin(ctx)
	if err != nil {
		return AgentKeyIssued{}, err
	}
	if name, err = agentName(name); err != nil {
		return AgentKeyIssued{}, err
	}
	if expiresInDays < 0 || expiresInDays > MaxAgentKeyDays {
		return AgentKeyIssued{}, fmt.Errorf("%w: expires_in_days 取 0（不过期）到 %d", ErrStaffBadRequest, MaxAgentKeyDays)
	}
	var raw [agentKeyRandomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return AgentKeyIssued{}, err
	}
	secret := AgentKeyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	var exp *time.Time
	if expiresInDays > 0 {
		t := time.Now().UTC().AddDate(0, 0, expiresInDays)
		exp = &t
	}
	var out AgentKeyIssued
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgent(ctx, staffID); errors.Is(err, repository.ErrAgentNotFound) {
			return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, staffID)
		} else if err != nil {
			return err
		}
		k, err := tx.CreateAgentKey(ctx, staffID, name, secret[:agentKeyPrefixLen], HashAgentKey(secret), exp, id.StaffID)
		if err != nil {
			return err
		}
		out = AgentKeyIssued{Key: k, Secret: secret}
		return nil
	})
	return out, err
}

// RevokeAgentKey 实现 DELETE /admin/agents/{staff_id}/keys/{key_id}。吊销即刻生效：下一次请求就 401。
func (s *StaffService) RevokeAgentKey(ctx context.Context, staffID, keyID int64) error {
	if _, err := requireShopAdmin(ctx); err != nil {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.FindAgent(ctx, staffID); errors.Is(err, repository.ErrAgentNotFound) {
			return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, staffID)
		} else if err != nil {
			return err
		}
		if err := tx.RevokeAgentKey(ctx, staffID, keyID); errors.Is(err, repository.ErrAgentKeyNotFound) {
			return fmt.Errorf("%w: key_id=%d", ErrAgentKeyNotFound, keyID)
		} else if err != nil {
			return err
		}
		return nil
	})
}

// HashAgentKey 是密钥明文的落库形状：hex(sha256)。
func HashAgentKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// LoadAgentIdentity 是接入密钥 → StaffIdentity（auth.AgentBearer 调它）。租户已由 Host 解析在 ctx 里；
// 在这家店的租户事务里按哈希查 —— 拿 A 店的密钥打 B 店的域名，查不到，401。
// 员工停用照样返回身份（Status 带出去），由中间件回 403，与人的会话同一个口径。
func (s *StaffService) LoadAgentIdentity(ctx context.Context, raw string) (auth.StaffIdentity, error) {
	if !strings.HasPrefix(raw, AgentKeyPrefix) || len(raw) > 200 {
		return auth.StaffIdentity{}, ErrAgentKeyInvalid
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	var out auth.StaffIdentity
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		k, err := tx.LoadAgentKey(ctx, HashAgentKey(raw))
		if errors.Is(err, repository.ErrAgentKeyNotFound) {
			return ErrAgentKeyInvalid
		}
		if err != nil {
			return err
		}
		sc, err := tx.ListStaffScopes(ctx, k.StaffID)
		if err != nil {
			return err
		}
		if err := tx.TouchAgentKey(ctx, k.KeyID); err != nil {
			return err
		}
		m := merchantID
		out = auth.StaffIdentity{StaffID: k.StaffID, MerchantID: &m, Role: k.Role, Status: k.Status,
			RegionIDs: sc.RegionIDs, StoreIDs: sc.StoreIDs, AgentKeyID: k.KeyID}
		return nil
	})
	return out, err
}

// AgentSelf 是 GET /agent/whoami 的读：当前 AI 员工自己（名称）。判权就是「凭密钥进来了」。
func (s *StaffService) AgentSelf(ctx context.Context) (repository.Agent, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return repository.Agent{}, err
	}
	var out repository.Agent
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		a, err := tx.FindAgent(ctx, id.StaffID)
		if errors.Is(err, repository.ErrAgentNotFound) {
			return fmt.Errorf("%w: staff_id=%d", ErrAgentNotFound, id.StaffID)
		}
		out = a
		return err
	})
	return out, err
}

// IsAgentKeyRejected 给 auth.AgentBearer 判「这是密钥不能用（401）」还是服务端故障（500）。
func IsAgentKeyRejected(err error) bool { return errors.Is(err, ErrAgentKeyInvalid) }

// RecordAgentToolCall 写一行 MCP 工具调用审计（00093），在调用返回之后、独立的短事务里。
// 调用方（handler/mcp.go）对它的失败只记日志：审计写失败不该改变工具的结果。
func (s *StaffService) RecordAgentToolCall(ctx context.Context, c repository.AgentToolCall) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error { return tx.RecordAgentToolCall(ctx, c) })
}
