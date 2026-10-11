package model

import (
	"strconv"
	"testing"
)

// The two stream bounds shipped by default are a product decision with a measured
// basis, so they are pinned here rather than left as loose constants.
//
// Why these values:
//   - the pre-content deadline must be short enough to bound an upstream that keeps
//     sending non-meaningful events forever (the event-interval timer is reset by every
//     event we read, so it cannot do this job). 120s matches the only reference
//     implementation found to actually solve it, and gpt-load's first-byte budget.
//   - the event-interval timer now only covers a stall AFTER content was delivered. It
//     must fire before the CALLER gives up, otherwise the caller logs its own
//     cancellation (measured production symptom: 369s and 529s streams that ended as
//     octopus_client_canceled). 300s leaves room under the shortest caller patience we
//     have measured.
func TestDefaultStreamBoundsAreCallerPatienceAware(t *testing.T) {
	// The shortest client-side patience observed on real traffic. Keep the shipped
	// silence budget below it with margin, or the caller always wins the race.
	const shortestObservedCallerPatienceSeconds = 369

	settings := DefaultSettings()
	byKey := make(map[SettingKey]string, len(settings))
	for _, setting := range settings {
		byKey[setting.Key] = setting.Value
	}

	firstContent, ok := byKey[SettingKeyFirstTokenTimeOutDefault]
	if !ok {
		t.Fatalf("the pre-content deadline must be seeded by default")
	}
	if firstContent != DefaultFirstTokenTimeOutSeconds {
		t.Fatalf("seeded pre-content deadline = %q, want %q", firstContent, DefaultFirstTokenTimeOutSeconds)
	}

	idle, ok := byKey[SettingKeyRelayStreamDataTimeoutSec]
	if !ok {
		t.Fatalf("the event-interval timeout must be seeded by default")
	}
	if idle != DefaultRelayStreamDataIntervalTimeoutSeconds {
		t.Fatalf("seeded event-interval timeout = %q, want %q", idle, DefaultRelayStreamDataIntervalTimeoutSeconds)
	}

	firstContentSeconds, err := strconv.Atoi(DefaultFirstTokenTimeOutSeconds)
	if err != nil || firstContentSeconds <= 0 {
		t.Fatalf("the shipped pre-content deadline must be a positive integer, got %q (%v)", DefaultFirstTokenTimeOutSeconds, err)
	}
	idleSeconds, err := strconv.Atoi(DefaultRelayStreamDataIntervalTimeoutSeconds)
	if err != nil || idleSeconds <= 0 {
		t.Fatalf("the shipped event-interval timeout must be a positive integer, got %q (%v)", DefaultRelayStreamDataIntervalTimeoutSeconds, err)
	}
	if idleSeconds >= shortestObservedCallerPatienceSeconds {
		t.Fatalf("event-interval timeout %ds must fire before the shortest observed caller patience (%ds), or the caller keeps winning the race",
			idleSeconds, shortestObservedCallerPatienceSeconds)
	}
	if firstContentSeconds > idleSeconds {
		t.Fatalf("pre-content deadline (%ds) must not outlast the mid-answer silence budget (%ds): an upstream that never produces content is the worst case",
			firstContentSeconds, idleSeconds)
	}
}

// A previous shipped default must be registered as a legacy default, otherwise the row
// written by the old seed wins over the new constant forever on installed deployments.
func TestDefaultStreamBoundsRegisterTheirLegacyValues(t *testing.T) {
	if LegacyDefaultFirstTokenTimeOutSeconds != "0" {
		t.Fatalf("the pre-content deadline's legacy default must be the shipped-disabled value, got %q", LegacyDefaultFirstTokenTimeOutSeconds)
	}
	if LegacyDefaultRelayStreamDataIntervalTimeoutSeconds900 != "900" {
		t.Fatalf("the event-interval timeout's previous default must be registered, got %q", LegacyDefaultRelayStreamDataIntervalTimeoutSeconds900)
	}
	if LegacyDefaultRelayStreamDataIntervalTimeoutSeconds900 == DefaultRelayStreamDataIntervalTimeoutSeconds {
		t.Fatalf("the registered legacy value must differ from the shipped default, or the upgrade map is a no-op")
	}
	if LegacyDefaultFirstTokenTimeOutSeconds == DefaultFirstTokenTimeOutSeconds {
		t.Fatalf("the registered legacy value must differ from the shipped default, or the upgrade map is a no-op")
	}
}

// The third clock's shipped default is what the acceptance criteria were measured
// against, and it is deliberately far above this fleet's normal turn length: a ceiling
// that fires during ordinary traffic would cut legitimate answers and re-bill them on a
// second channel. 600s mirrors the reference implementation's
// `server.llm_request_timeout` and stays ~5x the observed 40-120s turns.
func TestDefaultRequestCeilingIsGenerousAndReferenceMatched(t *testing.T) {
	seconds, err := strconv.Atoi(DefaultRelayRequestTotalTimeoutSeconds)
	if err != nil {
		t.Fatalf("the ceiling default must be an integer number of seconds, got %q", DefaultRelayRequestTotalTimeoutSeconds)
	}
	if seconds != 600 {
		t.Fatalf("the shipped ceiling must stay at the reference implementation's order of magnitude (600s), got %d", seconds)
	}
	if seconds <= 300 {
		t.Fatalf("the ceiling (%ds) must stay above the mid-answer silence budget (300s), otherwise it would pre-empt the clock that can end a request honestly sooner", seconds)
	}
	if MaxRelayRequestTotalTimeoutSeconds < seconds {
		t.Fatalf("the validation bound (%d) must not reject the shipped default (%d)", MaxRelayRequestTotalTimeoutSeconds, seconds)
	}
}

func TestRequestCeilingSettingRejectsOutOfRangeValues(t *testing.T) {
	if err := (&Setting{Key: SettingKeyRelayRequestTotalTimeoutSec, Value: "600"}).Validate(); err != nil {
		t.Fatalf("600 must validate: %v", err)
	}
	if err := (&Setting{Key: SettingKeyRelayRequestTotalTimeoutSec, Value: "0"}).Validate(); err != nil {
		t.Fatalf("0 (disabled) must validate: %v", err)
	}
	if err := (&Setting{Key: SettingKeyRelayRequestTotalTimeoutSec, Value: "-1"}).Validate(); err == nil {
		t.Fatalf("a negative ceiling must be rejected")
	}
	if err := (&Setting{Key: SettingKeyRelayRequestTotalTimeoutSec, Value: strconv.Itoa(MaxRelayRequestTotalTimeoutSeconds + 1)}).Validate(); err == nil {
		t.Fatalf("a ceiling above the bound must be rejected, otherwise it overflows into 'disabled'")
	}
}
