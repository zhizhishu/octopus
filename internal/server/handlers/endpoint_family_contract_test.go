package handlers

import (
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	appmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 这条契约测试守的是一类**只会在上线后爆**的缺陷：
//
// 加了一个新的 /v1 relay 端点，但忘了在 middleware.auth 的
// apiKeyEndpointFamilyForPath 里登记族归属 → 该路径落到 Unclassified →
// AllowsEndpointFamily 对 Unclassified 恒 false（fail-closed）→
// **不管什么 API key 打过来都是 403**。
//
// 最阴的地方是常规 handler 测试抓不到它：那些测试直接构造 gin 上下文调 handler，
// 根本不经过 APIKeyAuth 中间件，于是全绿。2026-10-25 的 /v1/systemone 就是这么
// 踩的 —— 端点、测试全绿，实际一个请求都过不去。
//
// 这里改成反过来：拿**真实注册的路由表**逐条问族归属，任何一条落在 Unclassified
// 就报错并列出路径。漏登记从此在测试期就红，不用等用户撞。
//
// 它只能发现"没归类"，发现不了"归错族" —— 后者需要人判断（例如视频为什么不能
// 跟着 chat 走），不是机械规则能咬定的。
func TestEveryRegisteredRelayRouteHasAnEndpointFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}

	// 同一条路径可能注册多个方法；族判断只看路径，去重后逐条问。
	seen := make(map[string]struct{})
	var unclassified []string
	var checked int

	for _, r := range engine.Routes() {
		path := r.Path
		// 只查 relay 侧端点。管理/自助接口走 /api/，本来就不属于任何族。
		isRelayPath := strings.HasPrefix(path, "/v1/") ||
			path == "/chat/completions" ||
			path == "/responses" ||
			path == "/responses/compact" ||
			strings.HasPrefix(path, "/backend-api/")
		if !isRelayPath {
			continue
		}
		// gin 会给出带 :param 的真实形态，族表按前缀判断，直接问即可。
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		checked++

		family := middleware.EndpointFamilyForPath(path)
		normalized, ok := appmodel.NormalizeAPIKeyEndpointFamily(family)
		if !ok || normalized == appmodel.APIKeyEndpointFamilyUnclassified {
			unclassified = append(unclassified, r.Method+" "+path)
		}
	}

	if checked == 0 {
		t.Fatal("没有枚举到任何 relay 路由 —— 契约测试自身失效了（路由注册表为空），" +
			"这比漏登记更糟：它会让本测试永远为真")
	}

	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("%d 条已注册的 relay 路由没有端点族归属，这些路径对所有 API key 都会 403：\n  %s\n\n"+
			"修法：在 internal/server/middleware/auth.go 的 apiKeyEndpointFamilyForPath 里给它们登记族。",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}

	t.Logf("检查了 %d 条 relay 路由，全部有族归属", checked)
}

// 反向守卫：确保上面那条测试不是空转。
//
// 一个"逐条问族归属"的测试有个天然陷阱 —— 如果哪天有人把 Unclassified 的判定
// 放宽（比如让 Normalize 接受任意字符串），上面那条会因为"都能归类"而永久变绿，
// 守不住任何东西。这里直接钉住"未登记路径必须被拒"这个前提。
func TestUnregisteredRelayPathIsRejectedByContract(t *testing.T) {
	const fabricated = "/v1/__definitely_not_a_registered_endpoint__"
	family := middleware.EndpointFamilyForPath(fabricated)
	normalized, ok := appmodel.NormalizeAPIKeyEndpointFamily(family)
	if ok && normalized != appmodel.APIKeyEndpointFamilyUnclassified {
		t.Fatalf("未登记路径 %q 被归到了 %q。族表的 fail-closed 前提已经失效，"+
			"上面那条契约测试将不再能发现漏登记 —— 先修这里再谈别的。", fabricated, normalized)
	}
}
