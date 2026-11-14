package relay

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// A pointer is shared by racing attempt metric copies. Only the final request
// outcome settles it; channel retries never create another receipt.
type relayUserQuota struct {
	receipt   *model.UserQuotaReceipt
	mu        sync.Mutex
	finished  bool
	hasResult bool
	cost      float64
	success   bool
}

func admitRelayUserQuota(c *gin.Context, requestModel string) (*relayUserQuota, error) {
	receipt, err := op.UserQuotaAdmit(c.Request.Context(), op.UserQuotaAdmission{
		SourceKey: "relay:" + uuid.NewString(), SourceKind: "relay",
		UserID: c.GetInt("user_id"), RequestModel: requestModel, Units: 1,
	}, nil)
	if err != nil || receipt == nil {
		return nil, err
	}
	return &relayUserQuota{receipt: receipt}, nil
}

func relayUserQuotaErrorStatus(err error) int {
	if errors.Is(err, op.ErrUserQuotaModel) {
		return http.StatusBadRequest
	}
	if errors.Is(err, op.ErrUserQuotaExceeded) {
		return http.StatusPaymentRequired
	}
	return http.StatusInternalServerError
}

// finish returns true for configured requests even if persistence fails: a
// separate legacy debit must never split the receipt from the user's balance.
func (q *relayUserQuota) finish(ctx context.Context, cost float64, success bool) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.finished {
		return true
	}
	q.hasResult, q.cost, q.success = true, cost, success
	q.settleLocked(ctx)
	return true
}

func (q *relayUserQuota) settleLocked(ctx context.Context) {
	failed, outcome := int64(0), "completed"
	if !q.success {
		failed, outcome = q.receipt.Units, "failed"
	}
	_, err := op.UserQuotaSettle(ctx, q.receipt.SourceKey, q.receipt.UserID, q.cost, failed, outcome, nil)
	if err != nil {
		log.Warnf("user quota settlement pending: user=%d error=%v", q.receipt.UserID, err)
		return
	}
	q.finished = true
}

// A validation/route return after admission still has an observable failed
// accepted unit, and every normal handler return releases its reservation.
func (q *relayUserQuota) close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.finished {
		return
	}
	if !q.hasResult {
		q.cost, q.success = 0, false
	}
	ctx, cancel := metricsPersistContext()
	defer cancel()
	q.settleLocked(ctx)
}
