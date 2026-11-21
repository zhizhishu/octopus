package balancer

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
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
				entry.writeSeq = nextCircuitRevision()
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
	for _, hold := range listHolds(channelID) {
		status.Tripped = true
		status.OpenKeys++
		if hold.RemainingSeconds > status.RemainingSeconds {
			status.RemainingSeconds = hold.RemainingSeconds
		}
	}
	return status
}

// ResetChannel clears every breaker entry and soft runtime cooldown of the channel.
func ResetChannel(channelID int) int {
	return ResetScoped(channelID, 0, "")
}

// CircuitEntryView is one non-closed breaker entry of a channel, as the admin UI lists it.
type CircuitEntryView struct {
	KeyID               int    `json:"key_id"`
	Model               string `json:"model"`
	Endpoint            string `json:"endpoint,omitempty"`
	Capability          string `json:"capability,omitempty"`
	State               string `json:"state"`
	RemainingSeconds    int    `json:"remaining_seconds"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	TripCount           int    `json:"trip_count"`
	Reason              string `json:"reason,omitempty"`
}

type parsedCircuitKey struct {
	channelID  int
	keyID      int
	model      string
	endpoint   string
	capability string
}

// parseCircuitKey splits channelID:keyID:model[:endpoint=E:capability=C]. A model name may
// itself contain ':' (llama3:8b), so only the scoped suffix marker ends the model segment.
func parseCircuitKey(key string) (parsedCircuitKey, bool) {
	head, rest, ok := strings.Cut(key, ":")
	if !ok {
		return parsedCircuitKey{}, false
	}
	keyPart, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return parsedCircuitKey{}, false
	}
	channelID, err := strconv.Atoi(head)
	if err != nil {
		return parsedCircuitKey{}, false
	}
	keyID, err := strconv.Atoi(keyPart)
	if err != nil {
		return parsedCircuitKey{}, false
	}
	parsed := parsedCircuitKey{channelID: channelID, keyID: keyID, model: rest}
	if idx := strings.Index(rest, ":endpoint="); idx >= 0 {
		parsed.model = rest[:idx]
		scope := rest[idx+len(":endpoint="):]
		endpoint, capability, _ := strings.Cut(scope, ":capability=")
		parsed.endpoint = endpoint
		parsed.capability = capability
	}
	return parsed, true
}

func circuitStateName(state CircuitState) string {
	switch state {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// ListChannelCircuits returns the channel's open and half-open breaker entries, so an
// operator can see exactly which key/model is held and reset just that one.
func ListChannelCircuits(channelID int) []CircuitEntryView {
	views := make([]CircuitEntryView, 0)
	globalBreaker.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		parsed, ok := parseCircuitKey(keyString)
		if !ok || parsed.channelID != channelID {
			return true
		}
		entry, ok := value.(*circuitEntry)
		if !ok {
			return true
		}
		entry.mu.Lock()
		state := entry.State
		remaining := 0
		if state == StateOpen {
			cooldown := getCooldownForPolicy(entry.TripCount, entry.Policy)
			left := cooldown - time.Since(entry.LastFailureTime)
			if left > 0 {
				remaining = int((left + time.Second - 1) / time.Second)
			}
		}
		view := CircuitEntryView{
			KeyID:               parsed.keyID,
			Model:               parsed.model,
			Endpoint:            parsed.endpoint,
			Capability:          parsed.capability,
			State:               circuitStateName(state),
			RemainingSeconds:    remaining,
			ConsecutiveFailures: entry.ConsecutiveFailures,
			TripCount:           entry.TripCount,
			Reason:              entry.Policy.reason,
		}
		entry.mu.Unlock()
		if state != StateClosed {
			views = append(views, view)
		}
		return true
	})
	views = append(views, listHolds(channelID)...)
	sort.Slice(views, func(i, j int) bool {
		if views[i].Model != views[j].Model {
			return views[i].Model < views[j].Model
		}
		if views[i].KeyID != views[j].KeyID {
			return views[i].KeyID < views[j].KeyID
		}
		if views[i].Endpoint != views[j].Endpoint {
			return views[i].Endpoint < views[j].Endpoint
		}
		return views[i].Capability < views[j].Capability
	})
	return views
}

// ResetScoped clears the channel's breaker entries narrowed by key and model: keyID <= 0
// matches every key, an empty modelName matches every model. It also lifts the matching
// soft runtime cooldowns, so a reset model is routable at once instead of waiting out
// a second, independent clock. Returns the number of breaker entries removed.
func ResetScoped(channelID, keyID int, modelName string) int {
	deleted := 0
	globalBreaker.Range(func(key, _ any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		parsed, ok := parseCircuitKey(keyString)
		if !ok || parsed.channelID != channelID {
			return true
		}
		if keyID > 0 && parsed.keyID != keyID {
			return true
		}
		if modelName != "" && parsed.model != modelName {
			return true
		}
		globalBreaker.Delete(key)
		deleted++
		return true
	})
	deleted += clearHolds(channelID, keyID, modelName)
	clearRuntimeCooldowns(channelID, keyID, modelName)
	return deleted
}
