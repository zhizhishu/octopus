package balancer

import (
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func circuitKeyChannelID(key string) (int, bool) {
	head, _, ok := strings.Cut(key, ":")
	if !ok {
		return 0, false
	}
	channelID, err := strconv.Atoi(head)
	if err != nil {
		return 0, false
	}
	return channelID, true
}

func SnapshotChannel(channelID int) model.ChannelCircuitStatus {
	status := model.ChannelCircuitStatus{}
	globalBreaker.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		keyChannelID, ok := circuitKeyChannelID(keyString)
		if !ok || keyChannelID != channelID {
			return true
		}
		entry, ok := value.(*circuitEntry)
		if !ok {
			return true
		}

		entry.mu.Lock()
		switch entry.State {
		case StateOpen:
			cooldown := getCooldownForPolicy(entry.TripCount, entry.Policy)
			remaining := cooldown - time.Since(entry.LastFailureTime)
			if remaining <= 0 {
				entry.State = StateHalfOpen
				remaining = 0
			}
			status.Tripped = true
			status.OpenKeys++
			if seconds := int(remaining.Seconds()); seconds > status.RemainingSeconds {
				status.RemainingSeconds = seconds
			}
		case StateHalfOpen:
			status.Tripped = true
			status.OpenKeys++
		}
		entry.mu.Unlock()
		return true
	})
	return status
}

func ResetChannel(channelID int) int {
	deleted := 0
	globalBreaker.Range(func(key, _ any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		keyChannelID, ok := circuitKeyChannelID(keyString)
		if ok && keyChannelID == channelID {
			globalBreaker.Delete(key)
			deleted++
		}
		return true
	})
	return deleted
}
