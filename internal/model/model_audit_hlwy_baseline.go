package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
)

// ModelAuditHlwyBaseline 是本地保存的 hlwy 基准（管理员导入 / 从检测结果自采）。
//
// 只有六个字段，其余一律在 ContentJSON 里：
//
//   - 结构版本、来源、声明型号、采集时间、条件、数字、计数、统计全都已经在
//     numberdist.Baseline 的 JSON 里。再各自开一列，等于把同一份事实存两遍，
//     两遍迟早不一致，而「不一致时相信哪一列」没有正确答案。
//   - 真正需要可查询的只有三件事：按内容哈希幂等（payload_sha256 唯一索引）、
//     这条自采来自哪条检测结果（source_result_id，用于自比标记与追溯）、
//     以及展示名（label）。
//
// 不可变：没有任何 UPDATE/DELETE 路径。ID 由内容哈希限定，同一条结果重复保存
// 或同一份文件重复导入都落到同一行；改内容必然换哈希、换 ID，于是新行——
// 旧报告引用的仍是旧快照。
//
// 内置 7 份**不入库**：它们由 numberdist.LoadBuiltins() 从内嵌数据只读加载。
// 把内置数据 seed 进库，等于给了「运行时被改成另一份分布」的机会。
type ModelAuditHlwyBaseline struct {
	// ID 是 hash-qualified：导入件沿用 numberdist 的 "hlwy:<name>@<hash12>"，
	// 自采件是 "hlwy-capture:<hash12>"。长度上限 HlwyMaxBaselineIDBytes。
	ID string `json:"id" gorm:"primaryKey;size:128"`
	// PayloadSHA256 是幂等键（唯一索引）：同内容重复写入不会产生第二行。
	PayloadSHA256 string `json:"payload_sha256" gorm:"size:64;uniqueIndex"`
	// Label 是展示名；自采时可自定义，导入件沿用上游 name。
	Label string `json:"label" gorm:"size:255;default:''"`
	// SourceResultID 指向产生这份自采基准的检测结果；导入件为 0。
	SourceResultID int64 `json:"source_result_id" gorm:"index;default:0"`
	// ContentJSON 是归一化后的完整 numberdist.Baseline JSON。
	ContentJSON string    `json:"-" gorm:"type:text"`
	CreatedAt   time.Time `json:"created_at"`
}

// TableName 固定表名，避免 GORM 复数化规则将来变动导致迁移错位。
func (ModelAuditHlwyBaseline) TableName() string { return "model_audit_hlwy_baselines" }

const (
	// hlwyCaptureIDPrefix 是自采基准的 id 前缀。
	hlwyCaptureIDPrefix = "hlwy-capture:"
	// hlwyHashSuffixBytes 是 id 里保留的哈希前缀长度（12 位十六进制）。
	hlwyHashSuffixBytes = 12
)

// hlwyCapturePayload 是自采基准的规范化载荷。
//
// 只放**可复算的事实**：结构版本、来源结果 id、条件、有效数字。
// 标签/时间/备注这些展示字段刻意不进哈希——否则同一条结果改个名字就会变成
// 另一份「新基准」，幂等性立刻失效。
type hlwyCapturePayload struct {
	SchemaVersion  int                   `json:"schema_version"`
	SourceResultID int64                 `json:"source_result_id"`
	Conditions     numberdist.Conditions `json:"conditions"`
	RawNumbers     []int                 `json:"raw_numbers"`
}

// HlwyCaptureInput 是从一条已完成的 hlwy 检测结果生成基准所需的事实。
type HlwyCaptureInput struct {
	SourceResultID int64
	Label          string
	DeclaredModel  string
	APIType        string
	CollectedAt    *time.Time
	Conditions     numberdist.Conditions
	RawNumbers     []int
}

// HlwyCaptureHash 复算一份自采基准的规范化哈希。
//
// 用于两个地方：新建时给出 id/幂等键，以及校验既有行时「重新算一遍对不对得上」。
// 对不上就报错——一份被截断或改过数字的基准绝不能继续被当成参照物。
func HlwyCaptureHash(numbers []int, cond numberdist.Conditions, sourceResultID int64) (string, error) {
	if sourceResultID <= 0 {
		return "", errors.New("自采基准缺少来源结果 id")
	}
	if _, err := HlwyBinCounts(numbers); err != nil {
		return "", err
	}
	payload, err := json.Marshal(hlwyCapturePayload{
		SchemaVersion:  numberdist.SchemaVersion,
		SourceResultID: sourceResultID,
		Conditions:     cond,
		RawNumbers:     numbers,
	})
	if err != nil {
		return "", fmt.Errorf("规范化自采载荷失败: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// BuildHlwyCapture 用服务器侧证据重建一份自采基准（计数与统计一律重算）。
//
// 「只认有效且正常结束的自采」的门槛不在这里判（那要看任务与报告状态，属于
// op 层）；这里负责的是「拿到的数字必须能重建成一份自洽的基准」。
func BuildHlwyCapture(in HlwyCaptureInput, now time.Time) (numberdist.Baseline, error) {
	label := strings.TrimSpace(in.Label)
	if label == "" {
		return numberdist.Baseline{}, errors.New("自采基准缺少 label")
	}
	if err := validateHlwyLen("label", label, HlwyMaxLabelBytes); err != nil {
		return numberdist.Baseline{}, err
	}
	counts, err := HlwyBinCounts(in.RawNumbers)
	if err != nil {
		return numberdist.Baseline{}, err
	}
	sha, err := HlwyCaptureHash(in.RawNumbers, in.Conditions, in.SourceResultID)
	if err != nil {
		return numberdist.Baseline{}, err
	}

	var collectedAt *time.Time
	if in.CollectedAt != nil {
		t := in.CollectedAt.UTC()
		collectedAt = &t
	}
	numbers := append([]int(nil), in.RawNumbers...)
	return numberdist.Baseline{
		SchemaVersion: numberdist.SchemaVersion,
		ID:            hlwyCaptureIDPrefix + sha[:hlwyHashSuffixBytes],
		Label:         label,
		PayloadSHA256: sha,
		DeclaredModel: strings.TrimSpace(in.DeclaredModel),
		APIType:       strings.TrimSpace(in.APIType),
		CollectedAt:   collectedAt,
		ImportedAt:    now.UTC(),
		Source: numberdist.Source{
			Kind: numberdist.SourceLocal,
			Note: fmt.Sprintf("自采基准：服务器采样结果 #%d", in.SourceResultID),
		},
		Conditions: in.Conditions,
		RawNumbers: numbers,
		Counts:     counts,
		Stats:      numberdist.StatsOf(numbers),
	}, nil
}
