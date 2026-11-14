package op

import (
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

const userQuotaEpsilon = 1e-9

func userQuotaLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("a named IANA time zone is required")
	}
	return time.LoadLocation(name)
}

func userQuotaDefaultTimezone() (string, error) {
	loc := statsLocation()
	name := loc.String()
	if name == "Local" {
		// A UTC local zone has an unambiguous stable IANA equivalent.
		_, offset := time.Now().In(loc).Zone()
		if offset == 0 {
			name = "Etc/UTC"
		} else {
			return "", fmt.Errorf("configure a named statistics time zone before creating a quota policy")
		}
	}
	if _, err := userQuotaLocation(name); err != nil {
		return "", err
	}
	return name, nil
}

func userQuotaWindows(policy model.UserQuotaPolicy, at time.Time) ([]model.UserQuotaWindow, error) {
	loc, err := userQuotaLocation(policy.Timezone)
	if err != nil {
		return nil, err
	}
	anchor := policy.AnchorAt
	if anchor == 0 {
		anchor = at.Unix()
	}
	const width int64 = 5 * 60 * 60
	offset := at.Unix() - anchor
	bucket := offset / width
	if offset < 0 && offset%width != 0 {
		bucket--
	}
	five := anchor + bucket*width
	local := at.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	return []model.UserQuotaWindow{
		{UserID: policy.UserID, Period: "five_hour", StartAt: five, EndAt: five + width},
		{UserID: policy.UserID, Period: "week", StartAt: monday.Unix(), EndAt: monday.AddDate(0, 0, 7).Unix()},
		{UserID: policy.UserID, Period: "month", StartAt: month.Unix(), EndAt: month.AddDate(0, 1, 0).Unix()},
	}, nil
}

func userQuotaLimits(policy model.UserQuotaPolicy, period string) (float64, int64) {
	switch period {
	case "five_hour":
		return policy.FiveHourCost, policy.FiveHourRequests
	case "week":
		return policy.WeeklyCost, policy.WeeklyRequests
	default:
		return policy.MonthlyCost, policy.MonthlyRequests
	}
}

func userQuotaReceiptStarts(receipt model.UserQuotaReceipt) map[string]int64 {
	return map[string]int64{
		"five_hour": receipt.FiveHourStart,
		"week":      receipt.WeekStart,
		"month":     receipt.MonthStart,
	}
}
