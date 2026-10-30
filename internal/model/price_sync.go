package model

import "time"

// PriceSyncRun is one entry of the small price-library sync history shown in the
// auto-update dialog. Only the newest few rows are kept.
type PriceSyncRun struct {
	ID      uint      `json:"id" gorm:"primaryKey"`
	At      time.Time `json:"at" gorm:"index"`
	Trigger string    `json:"trigger"` // "auto" | "manual"
	// Status: "ok" (applied or nothing to do), "failed" (fetch failed, nothing changed),
	// "aborted" (fetch came back abnormally small, nothing changed), "conflict" (a price
	// changed while the run was applying, nothing changed).
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Changed int    `json:"changed"`
	Held    int    `json:"held"`
	// HeldModels lists the models held back because their change was too big
	// (newline separated, so it needs no JSON column type).
	HeldModels string `json:"held_models"`
	Message    string `json:"message"`
}

const (
	PriceSyncTriggerAuto   = "auto"
	PriceSyncTriggerManual = "manual"

	PriceSyncStatusOK       = "ok"
	PriceSyncStatusFailed   = "failed"
	PriceSyncStatusAborted  = "aborted"
	PriceSyncStatusConflict = "conflict"
)
