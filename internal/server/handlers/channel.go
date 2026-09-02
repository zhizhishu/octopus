package handlers

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeltest"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/task"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/safe"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listChannel),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateChannel),
		).
		AddRoute(
			router.NewRoute("/enable", http.MethodPost).
				Handle(enableChannel),
		).
		AddRoute(
			router.NewRoute("/reset-circuit", http.MethodPost).
				Handle(resetChannelCircuit),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteChannel),
		).
		AddRoute(
			router.NewRoute("/fetch-model", http.MethodPost).
				Handle(fetchModel),
		).
		AddRoute(
			router.NewRoute("/test", http.MethodPost).
				Handle(testChannel),
		).
		AddRoute(
			router.NewRoute("/test-proxy", http.MethodPost).
				Handle(testChannelProxy),
		)
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/sync", http.MethodPost).
				Handle(syncChannel),
		).
		AddRoute(
			router.NewRoute("/import-csv", http.MethodPost).
				Handle(importChannelCSV),
		).
		AddRoute(
			router.NewRoute("/last-sync-time", http.MethodGet).
				Handle(getLastSyncTime),
		)
}

func listChannel(c *gin.Context) {
	channels, err := op.ChannelList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	for i, channel := range channels {
		stats := op.StatsChannelGet(channel.ID)
		channels[i].Stats = &stats
		circuitStatus := balancer.SnapshotChannel(channel.ID)
		channels[i].CircuitTripped = circuitStatus.Tripped
		channels[i].CircuitRemainingSecs = circuitStatus.RemainingSeconds
		channels[i].CircuitOpenKeys = circuitStatus.OpenKeys
	}
	resp.Success(c, channels)
}

func createChannel(c *gin.Context) {
	var channel model.Channel
	if err := c.ShouldBindJSON(&channel); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := channel.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.ChannelCreate(&channel, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	stats := op.StatsChannelGet(channel.ID)
	channel.Stats = &stats
	scheduleChannelPostProcess([]model.Channel{channel})
	resp.Success(c, channel)
}

func updateChannel(c *gin.Context) {
	var req model.ChannelUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := req.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	channel, err := op.ChannelUpdate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	stats := op.StatsChannelGet(channel.ID)
	channel.Stats = &stats
	scheduleChannelPostProcess([]model.Channel{*channel})
	// An enable/disable toggled through the edit form must reconcile access-plan routes
	// promptly, not only at the tail of post-process (which can wait on model fetch). The
	// sync is idempotent + mutex-serialized, so this extra call is a cheap safety net.
	if req.Enabled != nil {
		scheduleAccessPlanChannelSync()
	}
	resp.Success(c, channel)
}

func importChannelCSV(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "CSV file is required")
		return
	}
	if file.Size > 1<<20 {
		resp.Error(c, http.StatusBadRequest, "CSV file is too large; maximum is 1 MiB")
		return
	}
	f, err := file.Open()
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "failed to open CSV file")
		return
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "failed to read CSV file")
		return
	}
	if len(content) > 1<<20 {
		resp.Error(c, http.StatusBadRequest, "CSV file is too large; maximum is 1 MiB")
		return
	}
	options := model.ChannelCSVImportOptions{
		DryRun:     strings.EqualFold(c.PostForm("dry_run"), "true"),
		ReplaceKey: strings.EqualFold(c.PostForm("replace_key"), "true"),
	}
	result, changed, err := op.ChannelImportCSV(c.Request.Context(), content, options)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(changed) > 0 {
		scheduleChannelPostProcess(changed)
	}
	resp.Success(c, result)
}

func scheduleChannelPostProcess(channels []model.Channel) {
	if len(channels) == 0 {
		return
	}
	copied := append([]model.Channel(nil), channels...)
	safe.SafeGo("channel-post-process", func() {
		channels := copied
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for idx := range channels {
			channel := &channels[idx]
			if channel.AutoSync {
				fetchedModels, err := helper.FetchModels(ctx, *channel)
				if err != nil {
					log.Warnf("failed to auto-sync channel %s models after change: %v", channel.Name, err)
				} else {
					discoveredModels := model.NormalizeChannelModelNames(xstrings.TrimCompact(fetchedModels))
					if !sameStringSlice(model.ChannelDiscoveredModelNames(*channel), discoveredModels) {
						updated, err := op.ChannelUpdate(&model.ChannelUpdateRequest{
							ID:               channel.ID,
							DiscoveredModels: &discoveredModels,
						}, ctx)
						if err != nil {
							log.Warnf("failed to persist discovered models for channel %s: %v", channel.Name, err)
						} else {
							channel = updated
						}
					}
				}
			}
			modelArray := model.ChannelSelectedModelNames(*channel)
			helper.LLMPriceAddToDB(modelArray, ctx)
			helper.ChannelBaseUrlDelayUpdate(channel, ctx)
		}
		runAccessPlanChannelSync(ctx)
	})
}

// accessPlanSyncMu serializes access-plan reconciliation so overlapping syncs (rapid
// channel enable/disable/delete bursts plus the post-process tail) can't interleave their
// route-target writes.
var accessPlanSyncMu sync.Mutex

// runAccessPlanChannelSync executes one incremental access-plan reconciliation under the
// package mutex. Reconciliation is idempotent and reconciles all plans with route profiles,
// refreshing the cache only when changes occur.
func runAccessPlanChannelSync(ctx context.Context) {
	accessPlanSyncMu.Lock()
	defer accessPlanSyncMu.Unlock()
	if err := op.AccessPlanSyncEnabledChannels(ctx); err != nil {
		log.Warnf("auto-sync access plan channels: %v", err)
	}
}

// scheduleAccessPlanChannelSync runs the incremental access-plan channel sync in the
// background so channel enable/disable/sync/delete returns immediately.
func scheduleAccessPlanChannelSync() {
	safe.SafeGo("access-plan-sync", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		runAccessPlanChannelSync(ctx)
	})
}

func sameStringSlice(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func enableChannel(c *gin.Context) {
	var request struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.ChannelEnabled(request.ID, request.Enabled, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	// Always reconcile: enabling adds the channel to every plan with a route
	// profile; disabling removes it so targets are evicted without a manual rebuild.
	scheduleAccessPlanChannelSync()
	resp.Success(c, nil)
}

func resetChannelCircuit(c *gin.Context) {
	var request struct {
		ID int `json:"id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if request.ID <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	balancer.ResetChannel(request.ID)
	resp.Success(c, nil)
}

func deleteChannel(c *gin.Context) {
	id := c.Param("id")
	idNum, err := strconv.Atoi(id)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.ChannelDel(idNum, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	// ChannelDel removed the row + its channel-cache entry; reconcile so every
	// plan with a route profile evicts the deleted channel's targets.
	scheduleAccessPlanChannelSync()
	resp.Success(c, nil)
}
func fetchModel(c *gin.Context) {
	var request model.Channel
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	models, err := helper.FetchModels(c.Request.Context(), request)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, models)
}

func testChannel(c *gin.Context) {
	var request model.ChannelTestRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	result, err := modeltest.RunChannel(c.Request.Context(), request.Channel, model.ModelTestRequest{
		Model:          request.Model,
		Endpoint:       request.Endpoint,
		Prompt:         request.Prompt,
		Stream:         request.Stream,
		TimeoutSeconds: request.TimeoutSeconds,
		Concurrency:    request.Concurrency,
	})
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, result)
}

func syncChannel(c *gin.Context) {
	task.SyncModelsTask()
	scheduleAccessPlanChannelSync()
	resp.Success(c, nil)
}

func getLastSyncTime(c *gin.Context) {
	time := task.GetLastSyncModelsTime()
	resp.Success(c, time)
}

// testChannelProxy tests TCP/HTTP reachability of base_url through the given
// socks5 or http proxy string, returning ok=true and delay_ms on success.
func testChannelProxy(c *gin.Context) {
	var req struct {
		ChannelProxy string `json:"channel_proxy"`
		BaseURL      string `json:"base_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	proxyStr := strings.TrimSpace(req.ChannelProxy)
	baseURL := strings.TrimSpace(req.BaseURL)
	if proxyStr == "" || baseURL == "" {
		resp.Error(c, http.StatusBadRequest, "channel_proxy and base_url are required")
		return
	}

	ch := &model.Channel{
		Proxy:        true,
		ChannelProxy: &proxyStr,
		BaseUrls:     []model.BaseUrl{{URL: baseURL}},
	}

	type proxyTestResult struct {
		OK      bool   `json:"ok"`
		DelayMs int64  `json:"delay_ms"`
		Message string `json:"message"`
	}

	start := time.Now()
	_, _, err := helper.CheckChannelProxyConnectivity(c.Request.Context(), ch)
	delayMs := time.Since(start).Milliseconds()

	if err != nil {
		resp.Success(c, proxyTestResult{OK: false, DelayMs: delayMs, Message: err.Error()})
		return
	}
	resp.Success(c, proxyTestResult{OK: true, DelayMs: delayMs, Message: ""})
}
