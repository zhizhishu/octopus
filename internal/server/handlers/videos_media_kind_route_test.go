package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 【路由真起】这条测试不做任何功能断言，只回答两个问题：
//  1. 全部路由注册进一个真 engine 不会 panic（同层级的字面量段与 :param 在
//     gin 的路由树里会抢同一个位置，注册顺序错了就是启动时 panic 或某条路径
//     永远匹配不上）；
//  2. 契约里那 9 条视频路径每一条都真的挂到了预期的 handler 上。
//
// 用 engine.Routes() 查表而不是发真实请求：这里要验的是「路由表长什么样」，
// 发请求会连带把鉴权/DB/上游都拖进来，失败了也说不清是路由问题还是别的。

func TestVideoMediaKindRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// 注册阶段就 panic 的路由树会直接把这条用例炸掉——这正是它要守的东西。
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	registered := make(map[string]string, len(engine.Routes()))
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = route.Handler
	}


	// kind 子路径的创建入口必须是「带 kind 的那一层」，不能退回裸 createVideo：
	// 后者会把 kind 记成空串，轮询于是回到 /v1/videos/{id}，上游 404。
	//
	// gin 的 RouteInfo.Handler 是完整函数名（包路径 + "." + 名字），闭包还会多一段
	// ".func1"，所以先剥掉包路径再按「完全相等 / 点号前缀」比。
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPost, "/v1/videos", "createVideo"},
		{http.MethodPost, "/v1/videos/generations", "createVideoWithMediaKind"},
		{http.MethodPost, "/v1/videos/edits", "createVideoWithMediaKind"},
		{http.MethodPost, "/v1/videos/extensions", "createVideoWithMediaKind"},
		{http.MethodGet, "/v1/videos/:id", "getVideo"},
		{http.MethodGet, "/v1/videos/generations/:id", "getVideo"},
		{http.MethodGet, "/v1/videos/edits/:id", "getVideo"},
		{http.MethodGet, "/v1/videos/extensions/:id", "getVideo"},
		{http.MethodGet, "/v1/videos/generations/:id/content", "getVideoContent"},
		{http.MethodGet, "/v1/videos/edits/:id/content", "getVideoContent"},
		{http.MethodGet, "/v1/videos/extensions/:id/content", "getVideoContent"},
	}

	for _, tc := range cases {
		key := tc.method + " " + tc.path
		handler, ok := registered[key]
		if !ok {
			t.Fatalf("%s is not registered (contract route missing)", key)
		}
		if !handlerNameMatches(handler, tc.want) {
			t.Fatalf("%s is handled by %q, want a %s handler", key, handler, tc.want)
		}
	}
}

// 同一张路由表还必须真的能匹配：Handler 名字对了但路径匹配不到，
// 对客户端来说和没注册是一回事（gin 会回 404 page not found）。
//
// 这里只验匹配，不看 handler 结果：用 video family 的 key 走真实 engine，
// 只要不是 gin 自己的 "404 page not found"，就证明请求已经落到了 handler 上。
func TestVideoMediaKindRoutesMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	videoKey := createVideosFamilyAPIKey(t, ctx, user, "video family", "sk-octopus-video-media-kind", nil)

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/videos/generations"},
		{http.MethodPost, "/v1/videos/edits"},
		{http.MethodPost, "/v1/videos/extensions"},
		{http.MethodGet, "/v1/videos/generations/task-abc"},
		{http.MethodGet, "/v1/videos/edits/task-abc"},
		{http.MethodGet, "/v1/videos/extensions/task-abc"},
		{http.MethodGet, "/v1/videos/generations/task-abc/content"},
		{http.MethodGet, "/v1/videos/edits/task-abc/content"},
		{http.MethodGet, "/v1/videos/extensions/task-abc/content"},
	} {
		rec := doVideosRequest(engine, tc.method, tc.path, videoKey.APIKey)
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s %s did not match any route", tc.method, tc.path)
		}
		t.Logf("%s %s -> %d %.120s", tc.method, tc.path, rec.Code, rec.Body.String())
	}
}

// TestHandlerNameMatchesCoversBothBuildShapes 直接钉住「函数名会漂」这件事本身。
//
// 背景：这条 helper 存在的原因就是 CI 与本机给出的名字不同
// （handlers.createVideoWithMediaKind.func2 vs handlers.init.22.createVideoWithMediaKind.func2）。
// 如果只靠 TestVideoMediaKindRoutesRegistered 间接覆盖，那么下次有人把 helper 改回
// TrimPrefix 朴素比较时，**本机依然绿**，只有云端会红 —— 又变成一条"本地绿云端红"的坑。
// 所以把两种真实形态都写成显式用例。
func TestHandlerNameMatchesCoversBothBuildShapes(t *testing.T) {
	const pkg = "github.com/zhizhishu/octopus/internal/server/handlers."
	cases := []struct {
		name string
		full string
		want string
		ok   bool
	}{
		{
			name: "本机形态：包路径后直接是函数名",
			full: pkg + "createVideoWithMediaKind.func2",
			want: "createVideoWithMediaKind",
			ok:   true,
		},
		{
			name: "CI 形态：中间多一段 init.N（序号会漂）",
			full: pkg + "init.22.createVideoWithMediaKind.func2",
			want: "createVideoWithMediaKind",
			ok:   true,
		},
		{
			name: "CI 形态但序号不同（漂到别的值也得过）",
			full: pkg + "init.7.createVideoWithMediaKind.func1",
			want: "createVideoWithMediaKind",
			ok:   true,
		},
		{
			name: "普通函数名（非闭包）",
			full: pkg + "getVideo",
			want: "getVideo",
			ok:   true,
		},
		{
			name: "别的 handler 不能冒充",
			full: pkg + "init.22.createVideo.func2",
			want: "createVideoWithMediaKind",
			ok:   false,
		},
		{
			name: "裸 createVideo 就是裸 createVideo",
			full: pkg + "createVideo",
			want: "createVideoWithMediaKind",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := handlerNameMatches(tc.full, tc.want); got != tc.ok {
				t.Fatalf("handlerNameMatches(%q, %q) = %v, want %v", tc.full, tc.want, got, tc.ok)
			}
		})
	}
}
