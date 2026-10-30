package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
)

// taskSubmitTarget 是 POST /v1/tasks/:key 的一个提交目标：任务类型 + 原生处理器。
type taskSubmitTarget struct {
	kind string
	run  func(c *gin.Context)
}

var midjourneySubmitModes = map[string]bool{
	MidjourneyModeImagine: true, MidjourneyModeChange: true, MidjourneyModeSimpleChange: true,
	MidjourneyModeDescribe: true, MidjourneyModeBlend: true, MidjourneyModeEdits: true,
	MidjourneyModeVideo: true, MidjourneyModeAction: true, MidjourneyModeModal: true,
	MidjourneyModeShorten: true, MidjourneyModeUpload: true, MidjourneyModeSwapFace: true,
}

// resolveTaskSubmitTarget 把 :key 映射到已有处理器。映射表只在这里一处；每个目标调用的
// 都是对应原生路由注册的同一个函数、同一个参数（见 handlers/videos.go、images.go、midjourney.go）。
func resolveTaskSubmitTarget(key string, c *gin.Context) (taskSubmitTarget, string) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "video", "videos":
		return taskSubmitTarget{TaskKindVideo, func(c *gin.Context) { VideosHandlerKind(c, "") }}, ""
	case "video-generations", "videos-generations":
		return taskSubmitTarget{TaskKindVideo, func(c *gin.Context) { VideosHandlerKind(c, VideoMediaKindGenerations) }}, ""
	case "video-edits", "videos-edits":
		return taskSubmitTarget{TaskKindVideo, func(c *gin.Context) { VideosHandlerKind(c, VideoMediaKindEdits) }}, ""
	case "video-extensions", "videos-extensions":
		return taskSubmitTarget{TaskKindVideo, func(c *gin.Context) { VideosHandlerKind(c, VideoMediaKindExtensions) }}, ""
	case "image", "images", "image-generations", "images-generations":
		return taskSubmitTarget{TaskKindImage, func(c *gin.Context) { ImagesAsyncSubmit(model.ImageTaskEndpointGenerations, c) }}, ""
	case "image-edits", "images-edits":
		return taskSubmitTarget{TaskKindImage, func(c *gin.Context) { ImagesAsyncSubmit(model.ImageTaskEndpointEdits, c) }}, ""
	case "midjourney", "mj":
		mode := strings.ToLower(strings.TrimSpace(c.Query("mode")))
		if mode == "" {
			mode = MidjourneyModeImagine
		}
		if !midjourneySubmitModes[mode] {
			return taskSubmitTarget{}, "unknown midjourney mode: " + mode
		}
		return taskSubmitTarget{TaskKindMidjourney, func(c *gin.Context) { MidjourneySubmitHandler(c, mode) }}, ""
	}
	return taskSubmitTarget{}, "unknown task type: " + key
}

// TaskSubmitHandler 处理 POST /v1/tasks/:key。
func TaskSubmitHandler(c *gin.Context) {
	target, msg := resolveTaskSubmitTarget(c.Param("key"), c)
	if msg != "" {
		taskError(c, http.StatusNotFound, "invalid_request_error", msg)
		return
	}
	runTaskSubmit(c, target)
}

// VideoGenerationSubmitHandler 处理 POST /v1/video/generations（new-api 的视频提交路由），
// 等同 POST /v1/tasks/video：请求体原样转发到上游 /v1/videos。
func VideoGenerationSubmitHandler(c *gin.Context) {
	target, _ := resolveTaskSubmitTarget("video", c)
	runTaskSubmit(c, target)
}

func runTaskSubmit(c *gin.Context, target taskSubmitTarget) {
	if target.kind == TaskKindMidjourney {
		// mode 是 /v1/tasks 自己的参数；midjourneyForward 会把查询串带到上游，先摘掉。
		q := c.Request.URL.Query()
		q.Del("mode")
		c.Request.URL.RawQuery = q.Encode()
	}
	orig := c.Writer
	capture := newTaskCaptureWriter(orig)
	c.Writer = capture
	target.run(c)
	c.Writer = orig

	status := capture.Status()
	body := capture.body.Bytes()
	if id := submittedTaskID(target.kind, status, body); id != "" {
		view := newTaskView(target.kind, id)
		var found *TaskView
		if target.kind != TaskKindMidjourney {
			found, _ = findOwnedTask(c, id, target.kind)
		}
		if found != nil {
			view = *found
		} else {
			// MJ 不在提交时再去上游问一次（那会多一条查询日志）；落库失败的视频/图片也走这里。
			view.Status = TaskStatusSubmitted
			view.CreatedAt = time.Now().Unix()
			if target.kind == TaskKindMidjourney {
				if row, _, ok := lookupOwnedMidjourneyTask(c.Request.Context(), c, id); ok {
					view.Model = row.Model
					view.Action = strings.ToLower(row.Action)
					if row.CreatedAt > 0 {
						view.CreatedAt = row.CreatedAt
					}
				}
			}
		}
		c.Header("Cache-Control", "private, no-store")
		c.JSON(http.StatusOK, view)
		return
	}
	// 没拿到任务（报错 / 上游未受理）：原样透传原生端点的回包。
	for k, vs := range capture.header {
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(status)
	if len(body) > 0 {
		_, _ = c.Writer.Write(body)
	}
}

// submittedTaskID 从原生端点的成功回包里取任务 id，判定口径与各原生处理器一致。
func submittedTaskID(kind string, status int, body []byte) string {
	if status < 200 || status >= 300 {
		return ""
	}
	switch kind {
	case TaskKindVideo:
		if ids := extractVideoTaskIDs(body); len(ids) > 0 {
			return ids[0]
		}
	case TaskKindImage:
		var payload struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(body, &payload) == nil {
			return strings.TrimSpace(payload.TaskID)
		}
	case TaskKindMidjourney:
		var parsed midjourneySubmitResponse
		if json.Unmarshal(body, &parsed) == nil && midjourneySubmitAccepted(status, parsed) {
			if s, ok := parsed.Result.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// taskCaptureWriter 截下委托处理器的整份回包（这三类提交都是一次性 JSON，不流式）。
type taskCaptureWriter struct {
	gin.ResponseWriter
	header http.Header
	status int
	body   bytes.Buffer
}

func newTaskCaptureWriter(w gin.ResponseWriter) *taskCaptureWriter {
	return &taskCaptureWriter{ResponseWriter: w, header: http.Header{}}
}

func (w *taskCaptureWriter) Header() http.Header { return w.header }
func (w *taskCaptureWriter) WriteHeader(code int) {
	if w.status == 0 && code > 0 {
		w.status = code
	}
}
func (w *taskCaptureWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
func (w *taskCaptureWriter) Write(b []byte) (int, error) {
	w.WriteHeaderNow()
	return w.body.Write(b)
}
func (w *taskCaptureWriter) WriteString(s string) (int, error) {
	w.WriteHeaderNow()
	return w.body.WriteString(s)
}
func (w *taskCaptureWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
func (w *taskCaptureWriter) Size() int {
	if w.status == 0 {
		return -1
	}
	return w.body.Len()
}
func (w *taskCaptureWriter) Written() bool { return w.status != 0 }
func (w *taskCaptureWriter) Flush()        {}
