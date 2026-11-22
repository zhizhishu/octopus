package model

type UsageReportRow struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Usage  StatsUsage       `json:"usage"`
	Models []UsageReportRow `json:"models"`
}

type UsageReportDay struct {
	Date   string             `json:"date"`
	Cost   float64            `json:"cost"`
	Models map[string]float64 `json:"models"`
}

type UsageReport struct {
	Start   string           `json:"start"`
	End     string           `json:"end"`
	Group   string           `json:"group"`
	Summary StatsUsage       `json:"summary"`
	Daily   []UsageReportDay `json:"daily"`
	Rows    []UsageReportRow `json:"rows"`
}
