package op

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/xredact"
	"github.com/bestruirui/octopus/internal/utils/xstrings"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

const (
	channelCSVImportMaxRows = 1000
	channelCSVImportKeyNote = "CSV import"
)

type normalizedChannelCSVRow struct {
	Row                int
	Type               outbound.OutboundType
	Name               string
	BaseURL            string
	APIKey             string
	Models             []string
	Model              string
	Custom             string
	CSVType            string
	AnthropicContext1M bool
}

func ChannelImportCSV(ctx context.Context, content []byte, options model.ChannelCSVImportOptions) (model.ChannelCSVImportResult, []model.Channel, error) {
	rows, err := parseChannelCSV(content)
	if err != nil {
		return model.ChannelCSVImportResult{}, nil, err
	}

	result := model.ChannelCSVImportResult{
		Total: len(rows),
		Rows:  make([]model.ChannelCSVImportRowResult, 0, len(rows)),
	}
	if options.DryRun {
		for _, row := range rows {
			result.Skipped++
			result.Rows = append(result.Rows, model.ChannelCSVImportRowResult{Row: row.Row, Name: row.Name, Action: "skipped"})
		}
		return result, nil, nil
	}

	existing, err := ChannelList(ctx)
	if err != nil {
		return result, nil, err
	}
	byName := make(map[string]model.Channel, len(existing))
	for _, channel := range existing {
		byName[strings.ToLower(strings.TrimSpace(channel.Name))] = channel
	}

	changed := make([]model.Channel, 0, len(rows))
	for _, row := range rows {
		if old, ok := byName[strings.ToLower(row.Name)]; ok {
			updated, err := updateChannelFromCSV(ctx, old, row, options)
			if err != nil {
				result.Failed++
				result.Rows = append(result.Rows, model.ChannelCSVImportRowResult{Row: row.Row, Name: row.Name, Action: "failed", Error: safeChannelCSVError(err)})
				continue
			}
			result.Updated++
			result.Rows = append(result.Rows, model.ChannelCSVImportRowResult{Row: row.Row, Name: row.Name, Action: "updated", ChannelID: updated.ID})
			changed = append(changed, *updated)
			continue
		}

		channel := model.Channel{
			Name:        row.Name,
			Type:        row.Type,
			Enabled:     true,
			Priority:    0,
			BaseUrls:    []model.BaseUrl{{URL: row.BaseURL, Delay: 0}},
			Keys:        []model.ChannelKey{{Enabled: true, ChannelKey: row.APIKey, Remark: channelCSVImportKeyNote}},
			Model:       row.Model,
			CustomModel: row.Custom,
			SelectedModels: model.SplitChannelModelCSV(
				row.Model,
				row.Custom,
			),
			AnthropicContext1M: row.AnthropicContext1M,
			AutoSync:           false,
			Cloak:              model.ChannelCloak{Mode: "auto"},
		}
		if err := ChannelCreate(&channel, ctx); err != nil {
			result.Failed++
			result.Rows = append(result.Rows, model.ChannelCSVImportRowResult{Row: row.Row, Name: row.Name, Action: "failed", Error: safeChannelCSVError(err)})
			continue
		}
		result.Created++
		result.Rows = append(result.Rows, model.ChannelCSVImportRowResult{Row: row.Row, Name: row.Name, Action: "created", ChannelID: channel.ID})
		changed = append(changed, channel)
	}
	return result, changed, nil
}

func updateChannelFromCSV(ctx context.Context, old model.Channel, row normalizedChannelCSVRow, options model.ChannelCSVImportOptions) (*model.Channel, error) {
	selectedModels := model.SplitChannelModelCSV(row.Model, row.Custom)
	req := model.ChannelUpdateRequest{
		ID:             old.ID,
		Type:           &row.Type,
		BaseUrls:       &[]model.BaseUrl{{URL: row.BaseURL, Delay: 0}},
		Model:          &row.Model,
		CustomModel:    &row.Custom,
		SelectedModels: &selectedModels,
	}
	if row.AnthropicContext1M {
		req.AnthropicContext1M = &row.AnthropicContext1M
	}
	if options.ReplaceKey {
		for _, key := range old.Keys {
			if key.ID > 0 {
				req.KeysToDelete = append(req.KeysToDelete, key.ID)
			}
		}
		req.KeysToAdd = []model.ChannelKeyAddRequest{{Enabled: true, ChannelKey: row.APIKey, Remark: channelCSVImportKeyNote}}
	} else if !channelHasKey(old, row.APIKey) {
		req.KeysToAdd = []model.ChannelKeyAddRequest{{Enabled: true, ChannelKey: row.APIKey, Remark: channelCSVImportKeyNote}}
	}
	return ChannelUpdate(&req, ctx)
}

func channelHasKey(channel model.Channel, apiKey string) bool {
	apiKey = strings.TrimSpace(apiKey)
	for _, key := range channel.Keys {
		if strings.TrimSpace(key.ChannelKey) == apiKey {
			return true
		}
	}
	return false
}

func parseChannelCSV(content []byte) ([]normalizedChannelCSVRow, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("CSV is empty")
		}
		return nil, err
	}
	if err := validateChannelCSVHeader(header); err != nil {
		return nil, err
	}

	rows := make([]normalizedChannelCSVRow, 0)
	seenNames := map[string]bool{}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
			continue
		}
		rowNumber := len(rows) + 2
		if len(record) != 6 {
			return nil, fmt.Errorf("row %d: expected 6 fields, got %d", rowNumber, len(record))
		}
		if len(rows) >= channelCSVImportMaxRows {
			return nil, fmt.Errorf("too many rows: maximum is %d", channelCSVImportMaxRows)
		}
		row, err := normalizeChannelCSVRow(rowNumber, record)
		if err != nil {
			return nil, err
		}
		nameKey := strings.ToLower(row.Name)
		if seenNames[nameKey] {
			return nil, fmt.Errorf("row %d: duplicate channel name", rowNumber)
		}
		seenNames[nameKey] = true
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV has no channel rows")
	}
	return rows, nil
}

func validateChannelCSVHeader(header []string) error {
	want := []string{"type", "name", "baseurl", "apikey", "supportedmodels", "defaulttestmodel"}
	if len(header) != len(want) {
		return fmt.Errorf("CSV header must be type,name,baseURL,apiKey,supportedModels,defaultTestModel")
	}
	for idx, value := range header {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != want[idx] {
			return fmt.Errorf("CSV header must be type,name,baseURL,apiKey,supportedModels,defaultTestModel")
		}
	}
	return nil
}

func normalizeChannelCSVRow(rowNumber int, record []string) (normalizedChannelCSVRow, error) {
	csvType := strings.ToLower(strings.TrimSpace(record[0]))
	name := strings.TrimSpace(record[1])
	baseURL := strings.TrimSpace(record[2])
	apiKey := strings.TrimSpace(record[3])
	supported := strings.TrimSpace(record[4])
	defaultModel := strings.TrimSpace(record[5])

	channelType, err := channelTypeFromCSV(csvType)
	if err != nil {
		return normalizedChannelCSVRow{}, fmt.Errorf("row %d: %w", rowNumber, err)
	}
	if name == "" {
		return normalizedChannelCSVRow{}, fmt.Errorf("row %d: channel name is required", rowNumber)
	}
	if apiKey == "" {
		return normalizedChannelCSVRow{}, fmt.Errorf("row %d: apiKey is required", rowNumber)
	}
	normalizedBaseURL, err := normalizeCSVBaseURL(channelType, baseURL)
	if err != nil {
		return normalizedChannelCSVRow{}, fmt.Errorf("row %d: %w", rowNumber, err)
	}
	models, err := normalizeCSVModels(supported, defaultModel)
	if err != nil {
		return normalizedChannelCSVRow{}, fmt.Errorf("row %d: %w", rowNumber, err)
	}
	row := normalizedChannelCSVRow{
		Row:     rowNumber,
		Type:    channelType,
		Name:    name,
		BaseURL: normalizedBaseURL,
		APIKey:  apiKey,
		Models:  models,
		Model:   models[0],
		Custom:  strings.Join(models[1:], ","),
		CSVType: csvType,
		AnthropicContext1M: channelType == outbound.OutboundTypeAnthropic &&
			(model.ModelNamesWantAnthropicContext1M(xstrings.SplitTrimCompact("|", supported)) ||
				model.ModelNameWantsAnthropicContext1M(defaultModel)),
	}
	return row, nil
}

func channelTypeFromCSV(value string) (outbound.OutboundType, error) {
	switch value {
	case "openai", "deepseek":
		return outbound.OutboundTypeOpenAIChat, nil
	case "anthropic", "deepseek_anthropic":
		return outbound.OutboundTypeAnthropic, nil
	default:
		return 0, fmt.Errorf("unsupported type %q", value)
	}
}

func normalizeCSVBaseURL(channelType outbound.OutboundType, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("baseURL is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("baseURL must be an http(s) URL")
	}
	switch channelType {
	case outbound.OutboundTypeAnthropic:
		return xurl.TrimPathSuffixes(value, "/v1/messages", "/messages", "/v1/models", "/v1")
	default:
		return xurl.TrimPathSuffixes(value, "/v1/chat/completions", "/chat/completions", "/v1/responses", "/responses", "/v1/models", "/models", "/v1")
	}
}

func normalizeCSVModels(supported, defaultModel string) ([]string, error) {
	models := xstrings.SplitTrimCompact("|", supported)
	if len(models) == 0 {
		return nil, fmt.Errorf("supportedModels is required")
	}
	seen := map[string]bool{}
	deduped := make([]string, 0, len(models))
	for _, item := range models {
		item = model.CleanOneMillionCapabilityModelName(item)
		if strings.Contains(item, ",") {
			return nil, fmt.Errorf("model names must not contain comma")
		}
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, item)
	}
	if len(deduped) == 0 {
		return nil, fmt.Errorf("supportedModels is required")
	}
	defaultModel = model.CleanOneMillionCapabilityModelName(defaultModel)
	if defaultModel == "" {
		return deduped, nil
	}
	if strings.Contains(defaultModel, ",") {
		return nil, fmt.Errorf("defaultTestModel must not contain comma")
	}
	found := false
	for _, item := range deduped {
		if strings.EqualFold(item, defaultModel) {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("defaultTestModel must be included in supportedModels")
	}
	ordered := []string{defaultModel}
	for _, item := range deduped {
		if !strings.EqualFold(item, defaultModel) {
			ordered = append(ordered, item)
		}
	}
	return ordered, nil
}

func safeChannelCSVError(err error) string {
	if err == nil {
		return ""
	}
	return xredact.Secrets(err.Error())
}
