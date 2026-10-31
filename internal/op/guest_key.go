package op

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 访客 Key（管理员专用、临时给客人用）。
//
// 不是一套平行系统：访客 Key 就是一把 APIKey，多一个 Guest 标记，归属固定为建它的管理员。
// 于是转发、选路、计价、日志（relay_logs.api_key_id）、IP / 客户端（user_access_seen.api_key_id）、
// 用量（StatsAPIKey / APIKeyUsage）全部复用普通 Key 的同一条路径，一行都不分叉。
//
// 和普通 Key 的区别只在「权限更窄」：
//   - 只有管理员能建 / 列 / 改 / 作废 / 删（handler 层 AdminOnly，这里再判一次）；
//   - 必须有模型白名单（不能为空 = 不能「全部模型」）、必须有过期时间（临时）；
//   - 鉴权只放行模型转发端点（/api/* 一律 403，包括 /api/v1/apikey/login|stats，所以进不了控制台）；
//   - 建好后 Guest 标记、归属、Key 本身都不能改；普通 /apikey/update 动不了它；
//   - 作废（RevokedAt）是终态，不能再启用。
//
// 计费：归属是管理员，管理员本身不扣余额（UserRecordUsage 对管理员不记账），
// 所以花的是「管理员的池子」；真正的闸是这把 Key 自己的 MaxCost（必填、> 0；按 StatsAPIKey 累计花费
// 加上在途预留判，见 apikey_quota.go）。每次请求的费用照常算、照常记进日志和这把 Key 的用量。

var (
	errGuestKeyAdminOnly    = errors.New("only admin can manage guest keys")
	errGuestKeyNotFound     = errors.New("guest key not found")
	errGuestKeyUseGuestAPI  = errors.New("this is a guest key; manage it under API Key → Guest")
	errAPIKeyRevoked        = errors.New("API key has been revoked")
	errGuestKeyModels       = errors.New("guest key needs at least one model")
	errGuestKeyExpire       = errors.New("guest key needs an expire time")
	errGuestKeyExpireTooFar = fmt.Errorf("guest key can last at most %d days", GuestKeyMaxDays)
	errGuestKeyMaxCost      = errors.New("guest key needs a spend cap greater than 0")
	errGuestKeyRevokedEdit  = errors.New("guest key has been revoked")
)

// GuestKeyMaxDays：访客 Key 最长有效期。临时给人用的，不给「永不过期」。
const GuestKeyMaxDays = 365

const guestKeyMaxModels = 200

// ErrGuestKeyRevoked 给中间件判别用。
var ErrGuestKeyRevoked = errAPIKeyRevoked

func guestKeyRequireAdmin(actor model.User) error {
	if !actor.IsAdmin() || !actor.IsActive() {
		return errGuestKeyAdminOnly
	}
	return nil
}

func guestKeyNormalizeModels(models []string) (string, error) {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.ContainsAny(m, ",\n\r\t") {
			return "", fmt.Errorf("invalid model name %q", m)
		}
		if _, ok := seen[strings.ToLower(m)]; ok {
			continue
		}
		seen[strings.ToLower(m)] = struct{}{}
		out = append(out, m)
	}
	if len(out) == 0 {
		return "", errGuestKeyModels
	}
	if len(out) > guestKeyMaxModels {
		return "", fmt.Errorf("too many models (max %d)", guestKeyMaxModels)
	}
	return strings.Join(out, ","), nil
}

// guestKeyCheckModelsAvailable：访客 Key 限定的每个模型都必须在「当前可用模型」里
// （和 GuestKeyModels / 新建对话框里能选的是同一份：管理员自己能用的、有可用渠道的路由模型）。
// 比较口径和转发时判白名单一样（去掉 [1m] 后缀、不分大小写），所以这里放行的名字转发时一定认。
func guestKeyCheckModelsAvailable(models string, actor model.User, ctx context.Context) error {
	available, err := APIKeyModelsForUser(actor.ID, ctx)
	if err != nil {
		return fmt.Errorf("读取当前可用模型失败：%w", err)
	}
	have := make(map[string]struct{}, len(available))
	for _, m := range available {
		have[strings.ToLower(model.CleanOneMillionCapabilityModelName(m))] = struct{}{}
	}
	var missing []string
	for _, m := range strings.Split(models, ",") {
		name := strings.TrimSpace(m)
		if name == "" {
			continue
		}
		if _, ok := have[strings.ToLower(model.CleanOneMillionCapabilityModelName(name))]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("这些模型现在不在可用模型列表里：%s。请从可用模型里重新选。", strings.Join(missing, "、"))
	}
	return nil
}

func guestKeyValidateExpire(expireAt int64, now time.Time) error {
	if expireAt <= 0 {
		return errGuestKeyExpire
	}
	if expireAt <= now.Unix() {
		return errAPIKeyExpirePast
	}
	if expireAt > now.Add(GuestKeyMaxDays*24*time.Hour).Unix() {
		return errGuestKeyExpireTooFar
	}
	return nil
}

func guestKeyClip(s string, max int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// GuestKeyAuthCheck 是鉴权中间件对访客 Key 多做的那几条（普通检查——启用 / 过期 / 归属启用——照旧在前面）。
// 任何一条不满足都拒绝：作废、归属不再是管理员（例如被降级）、模型白名单为空（防御：库里被改空也不放成「全部模型」）。
func GuestKeyAuthCheck(key model.APIKey, owner model.User) error {
	if !key.Guest {
		return nil
	}
	if key.IsRevoked() {
		return errAPIKeyRevoked
	}
	if !owner.IsAdmin() {
		return errors.New("guest key owner is no longer admin")
	}
	if len(SupportedModelsList(key.SupportedModels)) == 0 {
		return errGuestKeyModels
	}
	// 花费上限是访客 Key 唯一的闸（管理员不扣余额）。现在建 / 改都要求 > 0；
	// 以前建的、上限留空（0 = 不限）的旧访客 Key 一律拒绝，直到管理员给它设上一个上限——
	// 宁可先停用，也不让一把「不限额、花管理员钱」的 Key 继续跑。
	if key.MaxCost <= 0 {
		return errGuestKeyMaxCost
	}
	return nil
}

// GuestKeyList 列出所有访客 Key（新的在前由前端排）。只给管理员。
func GuestKeyList(actor model.User, ctx context.Context) ([]model.APIKey, error) {
	if err := guestKeyRequireAdmin(actor); err != nil {
		return nil, err
	}
	keys := make([]model.APIKey, 0)
	for _, k := range apiKeyCache.GetAll() {
		if !k.Guest {
			continue
		}
		attachAPIKeyUserName(&k)
		attachAPIKeyAccessPlans(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

// GuestKeyGet 取一把访客 Key；不是访客 Key 一律「不存在」。
func GuestKeyGet(id int, actor model.User, ctx context.Context) (model.APIKey, error) {
	if err := guestKeyRequireAdmin(actor); err != nil {
		return model.APIKey{}, err
	}
	k, err := APIKeyGet(id, ctx)
	if err != nil || !k.Guest {
		return model.APIKey{}, errGuestKeyNotFound
	}
	return k, nil
}

// GuestKeyCreate 建一把访客 Key。secret 由 handler 生成（auth.GenerateAPIKey）。
func GuestKeyCreate(req model.GuestKeyRequest, secret string, actor model.User, ctx context.Context) (model.APIKey, error) {
	if err := guestKeyRequireAdmin(actor); err != nil {
		return model.APIKey{}, err
	}
	if strings.TrimSpace(secret) == "" {
		return model.APIKey{}, errors.New("failed to generate key")
	}
	models, err := guestKeyNormalizeModels(req.Models)
	if err != nil {
		return model.APIKey{}, err
	}
	if err := guestKeyCheckModelsAvailable(models, actor, ctx); err != nil {
		return model.APIKey{}, err
	}
	if err := guestKeyValidateExpire(req.ExpireAt, time.Now()); err != nil {
		return model.APIKey{}, err
	}
	if req.MaxCost <= 0 {
		return model.APIKey{}, errGuestKeyMaxCost
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	key := model.APIKey{
		UserID:           actor.ID,
		Name:             guestKeyClip(req.Name, 100),
		Note:             guestKeyClip(req.Note, 500),
		APIKey:           secret,
		Enabled:          enabled,
		ExpireAt:         req.ExpireAt,
		MaxCost:          req.MaxCost,
		SupportedModels:  models,
		EndpointFamilies: req.EndpointFamilies,
		Guest:            true,
	}
	if err := APIKeyCreate(&key, ctx); err != nil {
		return model.APIKey{}, err
	}
	return APIKeyGet(key.ID, ctx)
}

// GuestKeyUpdate 改名字 / 备注 / 模型 / 过期 / 上限 / 启用 / 协议。归属、Key、访客标记不动。
func GuestKeyUpdate(req model.GuestKeyRequest, actor model.User, ctx context.Context) (model.APIKey, error) {
	existing, err := GuestKeyGet(req.ID, actor, ctx)
	if err != nil {
		return model.APIKey{}, err
	}
	if existing.IsRevoked() {
		return model.APIKey{}, errGuestKeyRevokedEdit
	}
	name := guestKeyClip(req.Name, 100)
	if name == "" {
		return model.APIKey{}, fmt.Errorf("API key name is required")
	}
	models, err := guestKeyNormalizeModels(req.Models)
	if err != nil {
		return model.APIKey{}, err
	}
	if err := guestKeyCheckModelsAvailable(models, actor, ctx); err != nil {
		return model.APIKey{}, err
	}
	// 和普通 Key 一样：没改过期时间就不拦（已过期的 Key 还能改别的）。
	if req.ExpireAt != existing.ExpireAt {
		if err := guestKeyValidateExpire(req.ExpireAt, time.Now()); err != nil {
			return model.APIKey{}, err
		}
	}
	if req.MaxCost <= 0 {
		return model.APIKey{}, errGuestKeyMaxCost
	}
	families := existing.EndpointFamilies
	if req.EndpointFamilies != nil {
		families, err = model.NormalizeAPIKeyEndpointFamilies(req.EndpointFamilies)
		if err != nil {
			return model.APIKey{}, err
		}
	}
	enabled := existing.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	patch := model.APIKey{
		ID:               existing.ID,
		Name:             name,
		Note:             guestKeyClip(req.Note, 500),
		SupportedModels:  models,
		ExpireAt:         req.ExpireAt,
		MaxCost:          req.MaxCost,
		Enabled:          enabled,
		EndpointFamilies: families,
	}
	if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{ID: existing.ID}).
		Select("name", "note", "supported_models", "expire_at", "max_cost", "enabled", "endpoint_families").
		Updates(&patch).Error; err != nil {
		return model.APIKey{}, fmt.Errorf("failed to update guest key: %w", err)
	}
	apiKeyCacheLock.Lock()
	if cur, ok := apiKeyCache.Get(existing.ID); ok {
		cur.Name, cur.Note, cur.SupportedModels = patch.Name, patch.Note, patch.SupportedModels
		cur.ExpireAt, cur.MaxCost, cur.Enabled, cur.EndpointFamilies = patch.ExpireAt, patch.MaxCost, patch.Enabled, patch.EndpointFamilies
		apiKeyCache.Set(existing.ID, cur)
	}
	apiKeyCacheLock.Unlock()
	return APIKeyGet(existing.ID, ctx)
}

// GuestKeyRevoke 作废：立刻失效、不能再启用；记录保留，日志 / 用量 / IP 还能查。
func GuestKeyRevoke(id int, actor model.User, ctx context.Context) (model.APIKey, error) {
	existing, err := GuestKeyGet(id, actor, ctx)
	if err != nil {
		return model.APIKey{}, err
	}
	if existing.IsRevoked() {
		return existing, nil
	}
	now := time.Now().Unix()
	if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("id = ?", id).
		Updates(map[string]any{"revoked_at": now, "enabled": false}).Error; err != nil {
		return model.APIKey{}, fmt.Errorf("failed to revoke guest key: %w", err)
	}
	apiKeyCacheLock.Lock()
	if cur, ok := apiKeyCache.Get(id); ok {
		cur.RevokedAt = now
		cur.Enabled = false
		apiKeyCache.Set(id, cur)
	}
	apiKeyCacheLock.Unlock()
	return APIKeyGet(id, ctx)
}

// GuestKeyDelete 删掉一把访客 Key（与普通删除同一个函数）。
func GuestKeyDelete(id int, actor model.User, ctx context.Context) error {
	if _, err := GuestKeyGet(id, actor, ctx); err != nil {
		return err
	}
	return APIKeyDelete(id, actor, ctx)
}

// GuestKeyUsage 一把访客 Key 在 today / 7d / 30d / all 的用量（与 /apikey/usage 同一个函数）。
func GuestKeyUsage(id int, rangeKey string, actor model.User, ctx context.Context) (model.UserUsageResponse, error) {
	if _, err := GuestKeyGet(id, actor, ctx); err != nil {
		return model.UserUsageResponse{}, err
	}
	return APIKeyUsage(id, rangeKey, actor, ctx)
}

// GuestKeyModels 访客 Key 能选的模型：管理员自己能用的（与普通 Key 的「可限定模型」同一个口径）。
func GuestKeyModels(actor model.User, ctx context.Context) ([]string, error) {
	if err := guestKeyRequireAdmin(actor); err != nil {
		return nil, err
	}
	return APIKeyModelsForUser(actor.ID, ctx)
}

// GuestKeyAccessGet 一把访客 Key 的来源 IP / 客户端。数据就是 user_access_seen（RelayLogAdd →
// UserAccessRecord 那一条路径，用户页用的同一份），按 api_key_id 取，再并上还没落库的。
func GuestKeyAccessGet(id int, actor model.User, ctx context.Context) (model.GuestKeyAccess, error) {
	key, err := GuestKeyGet(id, actor, ctx)
	if err != nil {
		return model.GuestKeyAccess{}, err
	}
	var rows []model.UserAccessSeen
	if err := db.GetDB().WithContext(ctx).
		Where("user_id = ? AND api_key_id = ?", key.UserID, key.ID).
		Order("last_seen DESC").Limit(userAccessDetailMaxRow).Find(&rows).Error; err != nil {
		return model.GuestKeyAccess{}, err
	}
	rows = userAccessMergePending(rows, func(r model.UserAccessSeen) bool { return r.UserID == key.UserID && r.APIKeyID == key.ID })
	out := model.GuestKeyAccess{APIKeyID: key.ID, KeepDays: UserAccessKeepDays, Last: userAccessLastOf(rows)}
	for _, r := range rows {
		out.TotalRequests += r.Requests
	}
	out.IPs = userAccessGroup(rows, func(r model.UserAccessSeen) string { return r.IP }, func(r model.UserAccessSeen) string { return r.UserAgent })
	out.Clients = userAccessGroup(rows, func(r model.UserAccessSeen) string { return r.UserAgent }, func(r model.UserAccessSeen) string { return r.IP })
	return out, nil
}
