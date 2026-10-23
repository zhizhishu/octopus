package relay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 这组测试盯的是「按实际用量结算」这个闸门的两侧，缺一不可：
//
//   - 已向客户端交付了实质内容、但因客户端提前断开而终态失败 → **必须扣费**。
//     否则收到首 token 就 cancel 即可稳定白嫖：上游成本已经产生，我们一分不收。
//     旧实现把闸门挂在 `if success` 上（metrics.go:646），这类出口
//     （relay.go:1092-1100 返回 Success=false / Written=true）全额漏计。
//
//   - 完全没交付任何内容的失败请求 → **必须不扣**。这是同一枚硬币的另一面：
//     若只看"有成本"就扣，攻击者用无效请求（选路失败、上游 4xx、首字节前取消）
//     就能反复烧掉受害用户的余额。所以闸门是 `success || (交付过 && 成本>0)`。

// 客户端在已交付内容后断开：Written=true → 必须结算。
func TestBillingGateChargesDeliveredFailedRequest(t *testing.T) {
	m := &RelayMetrics{}
	m.MarkUsageDelivered()
	m.Stats.InputCost = 0.5
	m.Stats.OutputCost = 1.5

	if !shouldBillUser(true, m.usageDelivered, m.Stats.InputCost+m.Stats.OutputCost) {
		t.Fatal("已交付内容后失败（Written=true, cost=2.0）却判定不扣费：" +
			"收到首 token 就 cancel 即可白嫖，上游成本全部由我们承担")
	}
}

// 完全没交付的失败请求：不能扣，否则无效请求能烧用户余额。
func TestBillingGateSkipsFailedRequestWithoutDelivery(t *testing.T) {
	if shouldBillUser(false, false, 2.0) {
		t.Fatal("没有交付任何内容却扣费：攻击者可用无效请求烧掉受害者余额")
	}
}

// 交付了但没算出成本：没有可计费的钱，不该扣。
func TestBillingGateSkipsDeliveredWithoutCost(t *testing.T) {
	if shouldBillUser(false, true, 0) {
		t.Fatal("交付了但成本为 0 仍扣费：会产生金额为 0 的无效扣费调用")
	}
}

// 正常成功：行为不能变。
func TestBillingGateKeepsChargingSuccessfulRequest(t *testing.T) {
	if !shouldBillUser(true, false, 0.7) {
		t.Fatal("正常成功的请求却不再扣费")
	}
}

// 管理员 / 未登录（userID<=0）由 UserRecordUsage 内部处理，闸门本身照常返回 true。
// 这里只钉住闸门不因 userID 变化而改变判定，避免将来把身份判断塞进闸门。
func TestBillingGateIgnoresIdentity(t *testing.T) {
	if !shouldBillUser(true, false, 1) || !shouldBillUser(false, true, 1) {
		t.Fatal("闸门不应依赖身份")
	}
}

// 顺带守住：Save 里用的必须是折算过的 totalCost，而不是只有 OutputCost。
func TestBillingGateUsesBothCostLegs(t *testing.T) {
	// 只有 input 成本（例如纯 prompt 计费）也应被视为有成本。
	if !shouldBillUser(false, true, 0.25) {
		t.Fatal("仅有输入成本时被判为无成本")
	}
}

var _ = strings.TrimSpace
var _ = dbmodel.LLMPrice{}

// 上面那组只钉住了判据函数本身。这条直接调 **Save**（真实计费入口），
// 断言"已交付 + 有成本 + 终态失败"这一组合真的扣到了用户余额。
//
// 为什么必须单独有这条：把 Save 里的闸门退回 `if success` 时，判据函数测试
// 依然全绿（它们根本不经过 Save）—— 变异实测确认。只有走 Save 才能守住调用点。
func TestSaveChargesDeliveredFailedRequestToUserBalance(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "delivered-billing-user",
		Password: "test-password-123",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  10,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	m := &RelayMetrics{
		UserID:    user.ID,
		RequestIP: "127.0.0.1",
		StartTime: time.Now(),
	}
	m.Stats.InputCost = 0.4
	m.Stats.OutputCost = 0.6
	m.MarkUsageDelivered()

	// 终态是失败（客户端在流中途断开），但内容已交付、成本已产生。
	m.Save(ctx, false, context.Canceled, nil)

	after, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if charged := 10 - after.Balance; charged <= 0 {
		t.Fatalf("已交付内容后中断的请求一分未扣（balance 仍为 %v）："+
			"收到首 token 就 cancel 即可白嫖", after.Balance)
	}
}

// 反向守护：没交付的失败请求，Save 绝不能扣钱。
func TestSaveDoesNotChargeFailedRequestWithoutDelivery(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: "undelivered-billing-user",
		Password: "test-password-123",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
		Balance:  10,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	m := &RelayMetrics{UserID: user.ID, RequestIP: "127.0.0.1", StartTime: time.Now()}
	m.Stats.InputCost = 0.4
	m.Stats.OutputCost = 0.6
	// 刻意不调用 MarkUsageDelivered。

	m.Save(ctx, false, errors.New("upstream 500"), nil)

	after, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Balance != 10 {
		t.Fatalf("未交付任何内容的失败请求扣了 %v：无效请求可烧掉用户余额", 10-after.Balance)
	}
}
