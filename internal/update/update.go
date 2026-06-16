package update

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/client"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const (
	upstreamRepo         = "https://github.com/bestruirui/octopus"
	futureRepo           = "https://github.com/zhizhishu/octopus"
	futurePackageURL     = "https://github.com/zhizhishu/octopus/pkgs/container/octopus"
	futureImage          = "ghcr.io/zhizhishu/octopus:future"
	updateUrl            = upstreamRepo + "/releases/latest/download"
	updateApiUrl         = "https://api.github.com/repos/bestruirui/octopus/releases/latest"
	futureLatestApiUrl   = "https://api.github.com/repos/zhizhishu/octopus/actions/workflows/future-build.yml/runs?branch=future&status=success&per_page=1"
	futureUpdateBodyHint = "Future builds keep the upstream version number, but updates are published by zhizhishu/octopus. Use ghcr.io/zhizhishu/octopus:future or the fork's GitHub package page."
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
	body, err := doRequestWithFallback(updateApiUrl)
	if err != nil {
		return nil, err
	}

	var latestInfo LatestInfo
	if err := json.Unmarshal(body, &latestInfo); err != nil {
		log.Debugf("unmarshal body failed: %v", err)
		return nil, err
	}
	if latestInfo.Message != "" {
		return nil, fmt.Errorf("failed to get latest info: %s", latestInfo.Message)
	}
	latestInfo.SourceRepo = upstreamRepo
	latestInfo.UpdateRepo = upstreamRepo
	latestInfo.UpdateURL = upstreamRepo + "/releases/latest"
	latestInfo.UpdateMethod = "release"
	if IsFutureBuild() {
		latestInfo.FutureBuild = true
		latestInfo.UpdateRepo = futureRepo
		latestInfo.UpdateURL = futurePackageURL
		latestInfo.UpdateMethod = "future-ghcr"
		latestInfo.UpdateHint = futureUpdateBodyHint
	}
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

func unzip(data []byte, dest string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		log.Debugf("new zip reader failed: %v", err)
		return err
	}

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		if !isPathInDest(fpath, dest) {
			log.Debugf("invalid file path: %s", fpath)
			return fmt.Errorf("invalid file path: %s", fpath)
		}

		info := f.FileInfo()
		if info.IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}

		if err := extractFile(f, fpath); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, fpath string) error {
	if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
		log.Debugf("mkdir all failed: %v", err)
		return err
	}

	outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		if err = os.Remove(fpath); err != nil {
			log.Debugf("remove file failed: %v", err)
			return err
		}
		outFile, err = os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			log.Debugf("open file failed: %v", err)
			return err
		}
	}
	defer outFile.Close()

	rc, err := f.Open()
	if err != nil {
		log.Debugf("open file failed: %v", err)
		return err
	}
	defer rc.Close()

	if _, err = io.Copy(outFile, rc); err != nil {
		log.Debugf("copy failed: %v", err)
		return err
	}
	return nil
}

func isPathInDest(fpath, dest string) bool {
	rel, err := filepath.Rel(dest, fpath)
	if err != nil {
		return false
	}
	return filepath.IsLocal(rel)
}
