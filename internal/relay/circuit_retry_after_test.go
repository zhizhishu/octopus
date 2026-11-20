package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/gin-gonic/gin"
)

func TestRouteSelectionAllBreakerSkipsCarriesEarliestRetryAfter(t *testing.T) {
	attempts := []dbmodel.ChannelAttempt{
		{ChannelID: 1, ChannelName: "a", Status: dbmodel.AttemptCircuitBreak, RetryAfter: 40 * time.Second},
		{ChannelID: 2, ChannelName: "b", Status: dbmodel.AttemptCircuitBreak, RetryAfter: 2500 * time.Millisecond},
	}
	err := routeSelectionErrorFromAttempts(attempts)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writeUpstreamRetryAfterHint(c, http.StatusServiceUnavailable, err)
	if got := w.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3 (2.5s rounded up)", got)
	}
}

func TestRouteSelectionMixedSkipsAdvertiseNoRetryAfter(t *testing.T) {
	attempts := []dbmodel.ChannelAttempt{
		{ChannelID: 1, ChannelName: "a", Status: dbmodel.AttemptCircuitBreak, RetryAfter: 10 * time.Second},
		{ChannelID: 2, ChannelName: "b", Status: dbmodel.AttemptSkipped},
	}
	err := routeSelectionErrorFromAttempts(attempts)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writeUpstreamRetryAfterHint(c, http.StatusServiceUnavailable, err)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none when a candidate was dropped for another reason", got)
	}
}
