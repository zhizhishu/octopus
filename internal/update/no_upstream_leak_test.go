package update

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/conf"
)

// TestFutureBuildNeverExposesUpstreamRepo 守住一条用户明确要求的边界：
// future 构建（本项目）的版本信息里**不许出现上游作者的仓库地址**。
//
// 来龙去脉：GetLatestInfo 原来无条件去问上游的 release 接口，然后把上游地址
// 填进 SourceRepo；前端「版本来源」一行直接把它渲染成一个可点的链接。用户
// 反复要求界面上不要出现那个地址，但因为该 URL 是单独硬编码、没走 conf.Repo，
// 只改配置改不掉，于是一直冒出来。
//
// 所以这条测试盯的不是某个字符串常量，而是**对外输出的结构里有没有上游身份**。
func TestFutureBuildNeverExposesUpstreamRepo(t *testing.T) {
	origVersion := conf.Version
	origRepo := conf.Repo
	t.Cleanup(func() {
		conf.Version = origVersion
		conf.Repo = origRepo
	})

	conf.Version = "future"
	conf.Repo = "https://github.com/zhizhishu/octopus"

	if !IsFutureBuild() {
		t.Fatalf("前置条件不成立：future 构建未被识别")
	}

	info, err := GetLatestInfo()
	if err != nil {
		t.Fatalf("future 构建不应因外部请求失败而报错，得到: %v", err)
	}

	// 逐个字段检查，避免将来新增字段又漏掉。
	fields := map[string]string{
		"SourceRepo":   info.SourceRepo,
		"UpdateRepo":   info.UpdateRepo,
		"UpdateURL":    info.UpdateURL,
		"UpdateHint":   info.UpdateHint,
		"TagName":      info.TagName,
		"PublishedAt":  info.PublishedAt,
		"Body":         info.Body,
		"UpdateMethod": info.UpdateMethod,
	}
	for name, value := range fields {
		// 正面断言：凡是带 github.com 的字段，必须是本项目自己的仓库。
		// 这样不必在代码里写下别人的名字，也能保证任何外部仓库都进不来。
		if strings.Contains(value, "github.com/") && !strings.Contains(value, "zhizhishu/octopus") {
			t.Fatalf("future 构建的 %s 指向了外部仓库: %q", name, value)
		}
	}

	if !info.FutureBuild {
		t.Fatalf("future 构建必须标记 FutureBuild=true，否则前端会走官方分支显示")
	}
	if info.SourceRepo == "" {
		t.Fatalf("SourceRepo 不应为空，否则前端「版本来源」一行会缺失")
	}
}

// TestBuildInfoRepoComesFromConf 保证 BuildInfo.Repo 走 conf.Repo。
// 版本信息页的「GitHub 仓库」一行用它，必须是本项目自己的仓库。
func TestBuildInfoRepoComesFromConf(t *testing.T) {
	origRepo := conf.Repo
	t.Cleanup(func() { conf.Repo = origRepo })

	conf.Repo = "https://github.com/zhizhishu/octopus"
	info := GetBuildInfo()

	if info.Repo != conf.Repo {
		t.Fatalf("BuildInfo.Repo 应取自 conf.Repo，得到 %q", info.Repo)
	}
	if info.Repo != "https://github.com/zhizhishu/octopus" {
		t.Fatalf("BuildInfo.Repo 必须指向本项目: %q", info.Repo)
	}
}

// TestIsFutureBuildDetection 保证 future 判定对两种来源都成立：
// 编译期版本号，或仓库地址已改成自己的。任一满足即可，
// 否则一旦哪边没注入，界面就会退回去显示上游信息。
func TestIsFutureBuildDetection(t *testing.T) {
	origVersion := conf.Version
	origRepo := conf.Repo
	t.Cleanup(func() {
		conf.Version = origVersion
		conf.Repo = origRepo
	})

	cases := []struct {
		version string
		repo    string
		want    bool
	}{
		{"future", "https://github.com/zhizhishu/octopus", true},
		{"future-debf5b9", "https://github.com/zhizhishu/octopus", true},
		{"v1.2.3", "https://github.com/zhizhishu/octopus", true},
		{"future", "https://github.com/other/octopus", true},
		{"v1.2.3", "https://github.com/other/octopus", false},
	}
	for _, c := range cases {
		conf.Version = c.version
		conf.Repo = c.repo
		if got := IsFutureBuild(); got != c.want {
			t.Fatalf("version=%q repo=%q 期望 %v，得到 %v", c.version, c.repo, c.want, got)
		}
	}
}

// TestUpdateCoreAlwaysRefuses 守"永不自替换"。
//
// 本项目不做二进制自我更新：版本只由自己的镜像流水线产出，正确更新方式是拉新
// 镜像重启。这里断言的是**它无条件拒绝**——包括非 future 构建形态，
// 因为那条路原本会去下载外部发行包并覆盖自身，属于不该存在的出口。
func TestUpdateCoreAlwaysRefuses(t *testing.T) {
	origVersion := conf.Version
	origRepo := conf.Repo
	t.Cleanup(func() {
		conf.Version = origVersion
		conf.Repo = origRepo
	})

	for _, tc := range []struct{ version, repo string }{
		{"future", "https://github.com/zhizhishu/octopus"},
		{"v1.0.0", "https://github.com/zhizhishu/octopus"},
		{"v1.0.0", "https://github.com/other/octopus"},
	} {
		conf.Version = tc.version
		conf.Repo = tc.repo
		if err := UpdateCore(); err == nil {
			t.Fatalf("version=%q repo=%q 时 UpdateCore 必须拒绝执行", tc.version, tc.repo)
		}
	}
}
