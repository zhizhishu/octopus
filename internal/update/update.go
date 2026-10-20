package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/client"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

const (
	// 本项目是独立项目，**不再引用原作者仓库**。
	//
	// 这两行原本指向上游作者的发布页，用途是给"非 future 构建"做官方版本更新。
	// 但本项目永远走 future 构建（见 IsFutureBuild），那段代码在这里是死路；
	// 更关键的是它会把上游身份带进界面与网络请求。所以整条官方更新路径一并移除：
	// 更新只认自己的构建流水线，版本号只认自己的提交。
	//
	// 保留这些常量名只是为了让历史调用点编译通过，它们的值现在都指向本项目。
	futureRepo           = "https://github.com/zhizhishu/octopus"
	futurePackageURL     = "https://github.com/zhizhishu/octopus/pkgs/container/octopus"
	futureImage          = "ghcr.io/zhizhishu/octopus:future"
	futureLatestApiUrl   = "https://api.github.com/repos/zhizhishu/octopus/actions/workflows/future-build.yml/runs?branch=future&status=success&per_page=1"
	futureUpdateBodyHint = "版本由本项目自己的构建流水线发布，更新请拉取 ghcr.io/zhizhishu/octopus:future。"
)

type LatestInfo struct {
	TagName      string `json:"tag_name"`
	PublishedAt  string `json:"published_at"`
	Body         string `json:"body"`
	Message      string `json:"message"`
	SourceRepo   string `json:"source_repo,omitempty"`
	UpdateRepo   string `json:"update_repo,omitempty"`
	UpdateURL    string `json:"update_url,omitempty"`
	UpdateMethod string `json:"update_method,omitempty"`
	UpdateHint   string `json:"update_hint,omitempty"`
	FutureBuild  bool   `json:"future_build,omitempty"`
}

type BuildInfo struct {
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	BuildTime      string `json:"build_time"`
	Author         string `json:"author"`
	Repo           string `json:"repo"`
	Image          string `json:"image"`
	ImageTag       string `json:"image_tag"`
	PackageURL     string `json:"package_url"`
	FutureBuild    bool   `json:"future_build"`
	DisplayVersion string `json:"display_version"`
}

type FutureLatestInfo struct {
	Commit          string `json:"commit"`
	CommitShort     string `json:"commit_short"`
	RunID           int64  `json:"run_id"`
	RunNumber       int64  `json:"run_number"`
	Status          string `json:"status"`
	Conclusion      string `json:"conclusion"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	HTMLURL         string `json:"html_url"`
	Image           string `json:"image"`
	PackageURL      string `json:"package_url"`
	UpdateAvailable bool   `json:"update_available"`
	CurrentCommit   string `json:"current_commit"`
	Message         string `json:"message,omitempty"`
}

type futureWorkflowRunsResponse struct {
	Message      string `json:"message"`
	WorkflowRuns []struct {
		ID         int64  `json:"id"`
		RunNumber  int64  `json:"run_number"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
		HTMLURL    string `json:"html_url"`
	} `json:"workflow_runs"`
}

var github_pat = os.Getenv(strings.ToUpper(conf.APP_NAME) + "_GITHUB_PAT")

func IsFutureBuild() bool {
	return strings.HasPrefix(conf.Version, "future") || strings.Contains(conf.Repo, "zhizhishu/octopus")
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func isKnownCommit(commit string) bool {
	commit = strings.TrimSpace(commit)
	return commit != "" && commit != "unknown"
}

func GetBuildInfo() BuildInfo {
	info := BuildInfo{
		Version:     conf.Version,
		Commit:      conf.Commit,
		BuildTime:   conf.BuildTime,
		Author:      conf.Author,
		Repo:        conf.Repo,
		FutureBuild: IsFutureBuild(),
	}
	if info.FutureBuild {
		info.Image = futureImage
		info.ImageTag = "future"
		info.PackageURL = futurePackageURL
		if isKnownCommit(conf.Commit) {
			info.DisplayVersion = "future:" + shortCommit(conf.Commit)
		} else {
			info.DisplayVersion = "future"
		}
		return info
	}
	info.DisplayVersion = conf.Version
	return info
}

// doRequestWithFallback performs an HTTP GET request, first without proxy, then with proxy if failed.
func doRequestWithFallback(url string) ([]byte, error) {
	data, err := doRequest(url, false)
	if err == nil {
		return data, nil
	}
	log.Warnf("direct request failed, trying with proxy: %v", err)
	return doRequest(url, true)
}

func doRequest(url string, useProxy bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hc, err := client.GetHTTPClientSystemProxy(useProxy)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Debugf("new request failed: %v", err)
		return nil, err
	}

	if github_pat != "" {
		req.Header.Set("Authorization", "Bearer "+github_pat)
	}

	resp, err := hc.Do(req)
	if err != nil {
		log.Debugf("request failed: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("read body failed: %v", err)
		return nil, err
	}
	return data, nil
}

func GetLatestInfo() (*LatestInfo, error) {
	// future 构建（也就是本项目）**不联系上游仓库**。
	//
	// 原因有两层：
	//   1. 显示：上游身份一旦进入 LatestInfo，就会经 source_repo 显示在版本信息页上，
	//      用户明确要求界面上不许出现那个仓库地址。
	//   2. 归属：更新版本应当由自己的构建流水线决定，而不是去问原始作者的发布页。
	//      而且 UpdateCore 对 future 构建本来就是禁用的，这条查询没有任何用处。
	//
	// 版本与最新提交改由 GetFutureLatestInfo（自己的仓库）提供。
	if IsFutureBuild() {
		return &LatestInfo{
			TagName:      conf.Version,
			PublishedAt:  conf.BuildTime,
			SourceRepo:   futureRepo,
			UpdateRepo:   futureRepo,
			UpdateURL:    futurePackageURL,
			UpdateMethod: "future-ghcr",
			UpdateHint:   futureUpdateBodyHint,
			FutureBuild:  true,
		}, nil
	}

	body, err := doRequestWithFallback(futureLatestApiUrl)
	if err != nil {
		return nil, err
	}

	// 这里问的已经是本项目自己的构建记录，不再有"官方版本"这回事。
	// future 构建在第 190 行之前就返回了，走到这里只可能是别的构建形态；
	// 一律按本项目处理，避免任何情况下回落到上游。
	var latestInfo LatestInfo
	if err := json.Unmarshal(body, &latestInfo); err != nil {
		log.Debugf("unmarshal body failed: %v", err)
		return nil, err
	}
	latestInfo.SourceRepo = futureRepo
	latestInfo.UpdateRepo = futureRepo
	latestInfo.UpdateURL = futurePackageURL
	latestInfo.UpdateMethod = "future-ghcr"
	latestInfo.UpdateHint = futureUpdateBodyHint
	latestInfo.FutureBuild = true
	return &latestInfo, nil
}

func GetFutureLatestInfo() (*FutureLatestInfo, error) {
	body, err := doRequestWithFallback(futureLatestApiUrl)
	if err != nil {
		return nil, err
	}

	var runs futureWorkflowRunsResponse
	if err := json.Unmarshal(body, &runs); err != nil {
		log.Debugf("unmarshal future workflow runs failed: %v", err)
		return nil, err
	}
	if runs.Message != "" {
		return nil, fmt.Errorf("failed to get future latest info: %s", runs.Message)
	}
	if len(runs.WorkflowRuns) == 0 {
		return &FutureLatestInfo{
			Image:         futureImage,
			PackageURL:    futurePackageURL,
			CurrentCommit: conf.Commit,
			Message:       "no successful future build found",
		}, nil
	}

	run := runs.WorkflowRuns[0]
	latestCommit := strings.TrimSpace(run.HeadSHA)
	currentCommit := strings.TrimSpace(conf.Commit)
	updateAvailable := false
	if isKnownCommit(currentCommit) && isKnownCommit(latestCommit) {
		updateAvailable = !strings.HasPrefix(latestCommit, currentCommit) && !strings.HasPrefix(currentCommit, shortCommit(latestCommit))
	}

	return &FutureLatestInfo{
		Commit:          latestCommit,
		CommitShort:     shortCommit(latestCommit),
		RunID:           run.ID,
		RunNumber:       run.RunNumber,
		Status:          run.Status,
		Conclusion:      run.Conclusion,
		CreatedAt:       run.CreatedAt,
		UpdatedAt:       run.UpdatedAt,
		HTMLURL:         run.HTMLURL,
		Image:           futureImage,
		PackageURL:      futurePackageURL,
		UpdateAvailable: updateAvailable,
		CurrentCommit:   currentCommit,
	}, nil
}
