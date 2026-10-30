package op

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/tokenbank"
)

// ErrTokenizerBaselineExists 该型号已有基线（需显式 replace 才覆盖）。
var ErrTokenizerBaselineExists = errors.New("该型号已有分词器基线")

// TokenizerBaselineList 列出全部本地可信基线。
func TokenizerBaselineList(ctx context.Context) ([]model.TokenizerBaseline, error) {
	var rows []model.TokenizerBaseline
	err := db.GetDB().WithContext(ctx).Order("id ASC").Find(&rows).Error
	return rows, err
}

// TokenizerBaselineLocals 把库里的基线转成探针可用的形态。解析失败的行原样带出
// （空计数），由探针层逐条校验后跳过并记警告——不在这里静默吞掉。
func TokenizerBaselineLocals(ctx context.Context) ([]tokenbank.LocalBaseline, error) {
	rows, err := TokenizerBaselineList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tokenbank.LocalBaseline, 0, len(rows))
	for _, r := range rows {
		var counts map[string]int
		_ = json.Unmarshal([]byte(r.Counts), &counts)
		out = append(out, tokenbank.LocalBaseline{
			Model: r.Model, Label: r.Label, Counts: counts, BankGeneratedAt: r.BankGeneratedAt,
		})
	}
	return out, nil
}

// TokenizerBaselineFromResultInput 从一条检测结果提升基线的参数。
type TokenizerBaselineFromResultInput struct {
	ResultID int64
	Label    string
	Replace  bool
}

// TokenizerBaselineFromResult 把一条已完成的「录制分词器基线」检测结果存成可信基线。
//
// 只接受真实检测产出的计数：结果必须 done、报告里必须有 tokenizer_baseline 探针且
// recordable=true（无漂移、72 条全答复），并且用当前内置库可以合入（库版本一致）。
// 渠道"可信"与否由调用的管理员判断——这一步无法替他验证，界面与接口都要写明。
func TokenizerBaselineFromResult(ctx context.Context, in TokenizerBaselineFromResultInput) (model.TokenizerBaseline, error) {
	var res model.ModelAuditResult
	if err := db.GetDB().WithContext(ctx).Where("id = ?", in.ResultID).First(&res).Error; err != nil {
		return model.TokenizerBaseline{}, fmt.Errorf("检测结果不存在: %w", err)
	}
	if res.Status != model.AuditResultDone {
		return model.TokenizerBaseline{}, fmt.Errorf("检测结果尚未完成（%s）", res.Status)
	}
	var report struct {
		Results []struct {
			ProbeID string         `json:"probe_id"`
			OK      bool           `json:"ok"`
			Data    map[string]any `json:"data"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Report), &report); err != nil {
		return model.TokenizerBaseline{}, fmt.Errorf("检测报告无法解析: %w", err)
	}
	var data map[string]any
	for _, r := range report.Results {
		if r.ProbeID == "tokenizer_baseline" && r.OK {
			data = r.Data
		}
	}
	if data == nil {
		return model.TokenizerBaseline{}, fmt.Errorf("这条检测没有跑「录制分词器基线」")
	}
	if ok, _ := data["recordable"].(bool); !ok {
		reason, _ := data["reason"].(string)
		return model.TokenizerBaseline{}, fmt.Errorf("这次录制不可用作基线: %s", reason)
	}
	rawCounts, _ := data["counts"].(map[string]any)
	counts := make(map[string]int, len(rawCounts))
	for k, v := range rawCounts {
		f, ok := v.(float64)
		if !ok || f != float64(int(f)) {
			return model.TokenizerBaseline{}, fmt.Errorf("计数 %s 不是整数", k)
		}
		counts[k] = int(f)
	}
	bankAt, _ := data["bank_generated_at"].(string)
	target, _ := data["target_model"].(string)
	if strings.TrimSpace(target) == "" {
		target = res.UpstreamModel
	}
	if strings.TrimSpace(target) == "" {
		target = res.Model
	}
	local := tokenbank.LocalBaseline{Model: target, Label: in.Label, Counts: counts, BankGeneratedAt: bankAt}
	bank, err := tokenbank.Builtin()
	if err != nil {
		return model.TokenizerBaseline{}, err
	}
	if _, err := bank.WithLocal([]tokenbank.LocalBaseline{local}); err != nil {
		return model.TokenizerBaseline{}, err
	}
	norm := tokenbank.NormalizeModel(target)
	encoded, err := json.Marshal(counts)
	if err != nil {
		return model.TokenizerBaseline{}, err
	}
	row := model.TokenizerBaseline{
		Model:             norm,
		Label:             strings.TrimSpace(in.Label),
		Counts:            string(encoded),
		BankGeneratedAt:   bankAt,
		SourceChannelID:   res.ChannelID,
		SourceChannelName: res.ChannelName,
		SourceResultID:    res.ID,
	}
	tx := db.GetDB().WithContext(ctx)
	var existing model.TokenizerBaseline
	found := tx.Where("model = ?", norm).Limit(1).Find(&existing)
	if found.Error != nil {
		return model.TokenizerBaseline{}, found.Error
	}
	if found.RowsAffected > 0 {
		if !in.Replace {
			return model.TokenizerBaseline{}, ErrTokenizerBaselineExists
		}
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		if err := tx.Save(&row).Error; err != nil {
			return model.TokenizerBaseline{}, err
		}
		return row, nil
	}
	if err := tx.Create(&row).Error; err != nil {
		return model.TokenizerBaseline{}, err
	}
	return row, nil
}

// TokenizerBaselineDelete 删除一条基线。
func TokenizerBaselineDelete(ctx context.Context, id int64) error {
	res := db.GetDB().WithContext(ctx).Where("id = ?", id).Delete(&model.TokenizerBaseline{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("基线不存在")
	}
	return nil
}
