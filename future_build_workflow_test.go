package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// future-build.yml 的发布契约：共享标签只能由 future 分支 head 在守卫下按构建
// digest 移动；sha 标签只是 per-commit 便利标签，re-run 可能覆盖，不能当发布依据。
const futureBuildWorkflow = ".github/workflows/future-build.yml"

var (
	runSHA       = "0123456789abcdef0123456789abcdef01234567"
	otherSHA     = "89abcdef0123456789abcdef0123456789abcdef"
	debianDigest = "sha256:" + strings.Repeat("1", 64)
	alpineDigest = "sha256:" + strings.Repeat("2", 64)
)

type workflow struct {
	Concurrency *struct {
		Group  string `yaml:"group"`
		Cancel bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Steps []struct {
			Run  string         `yaml:"run"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// loadFutureBuild 解析真实 workflow，并返回 digest 表达式已替换为真实值的 Promote 脚本。
func loadFutureBuild(t *testing.T) (workflow, string) {
	t.Helper()
	raw, err := os.ReadFile(futureBuildWorkflow)
	if err != nil {
		t.Fatalf("读取 workflow: %v", err)
	}
	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("解析 workflow: %v", err)
	}
	for _, step := range w.Jobs["image"].Steps {
		if strings.Contains(step.Run, "imagetools create") && strings.Contains(step.Run, "${IMAGE_NAME}@") {
			return w, strings.NewReplacer("${{ steps.build-debian.outputs.digest }}", debianDigest,
				"${{ steps.build-alpine.outputs.digest }}", alpineDigest).Replace(step.Run)
		}
	}
	t.Fatal("缺少按 digest 发布共享标签的 Promote 步骤")
	return w, ""
}

// 结构契约：PR/非 future 触发不发布；共享标签不在构建阶段推送；发布只认 digest。
func TestFutureBuildPublishingContract(t *testing.T) {
	w, _ := loadFutureBuild(t)
	if c := w.Concurrency; c == nil || !c.Cancel || !strings.Contains(c.Group, "github.ref") || strings.Contains(c.Group, "github.sha") {
		t.Fatalf("concurrency 必须按 ref 取消旧构建: %+v", w.Concurrency)
	}
	image := w.Jobs["image"]
	if !strings.Contains(image.If, "pull_request") || !strings.Contains(image.If, "refs/heads/future") {
		t.Fatalf("image job 必须排除 PR 并限定 future 分支: %q", image.If)
	}
	for _, step := range image.Steps {
		if tags, ok := step.With["tags"].(string); ok && !strings.Contains(tags, "outputs.tags") &&
			(!strings.Contains(tags, "type=sha") || strings.Contains(tags, "type=raw")) {
			t.Fatalf("构建阶段只允许 per-commit 的 sha 标签: %q", tags)
		}
	}
}

// promoteStubs 顶替 gh 与 docker，记录每次 create 并把 create 的 digest 回放给 inspect。
const promoteStubs = `
gh() {
	if [ -n "${STUB_GH_FAIL:-}" ]; then printf '%s\n' "${STUB_HEAD:-}"; return 1; fi
	printf '%s\n' "${STUB_HEAD:-}"
}
docker() {
	if [ "${3:-}" = "create" ]; then
		echo "CREATE $*" >&2
		for arg in "$@"; do case "$arg" in *"@sha256:"*) stub_last="${arg##*@}" ;; esac; done
		if [ -n "${STUB_CREATE_FAIL:-}" ]; then return 1; fi
		return 0
	fi
	if [ -n "${STUB_INSPECT_FAIL:-}" ]; then return 1; fi
	if [ -n "${STUB_INSPECT_OUT:-}" ]; then echo "${STUB_INSPECT_OUT}"; else echo "${stub_last:-}"; fi
}
`

func runPromote(t *testing.T, script string, stubEnv ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command("bash", "-c", promoteStubs+script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "IMAGE_NAME=ghcr.io/example/octopus",
		"GITHUB_REPOSITORY=example/octopus", "GITHUB_SHA=" + runSHA}, stubEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

// 行为契约：head 匹配才发布两个变体；head 前进则零发布；查询/发布/校验失败都不是成功。
func TestFutureBuildPromoteScript(t *testing.T) {
	_, script := loadFutureBuild(t)
	for _, tc := range []struct {
		name        string
		env         []string
		wantFail    bool
		wantCreates int
	}{
		{"head 匹配时两个变体都发布", []string{"STUB_HEAD=" + runSHA}, false, 2},
		{"head 已前进时不发布共享标签", []string{"STUB_HEAD=" + otherSHA}, false, 0},
		{"head 查询失败必须失败", []string{"STUB_GH_FAIL=1"}, true, 0},
		{"head 查询有部分输出仍须失败", []string{"STUB_GH_FAIL=1", "STUB_HEAD=" + runSHA}, true, 0},
		{"create 失败必须失败", []string{"STUB_HEAD=" + runSHA, "STUB_CREATE_FAIL=1"}, true, 1},
		{"inspect 失败必须失败", []string{"STUB_HEAD=" + runSHA, "STUB_INSPECT_FAIL=1"}, true, 1},
		{"inspect 不匹配必须失败", []string{"STUB_HEAD=" + runSHA, "STUB_INSPECT_OUT=" + alpineDigest}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, failed := runPromote(t, script, tc.env...)
			if creates := strings.Count(out, "CREATE "); failed != tc.wantFail || creates != tc.wantCreates {
				t.Fatalf("failed=%v creates=%d，期望 %v/%d\n%s", failed, creates, tc.wantFail, tc.wantCreates, out)
			}
		})
	}
}
