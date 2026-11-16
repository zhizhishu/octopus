package redteamlab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const V2Schema = "octopus.redteam.case.v1"
const V2MaxCasesImported = 20
const V2MaxTurns = 4
const V2MaxInputRunes = 6000

var v2CaseID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)
var v2Category = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// V2CaseSpec is a transport-free, allowlisted case contract. No upstream URL,
// credentials, raw HTTP headers, arbitrary scripts or attacker provider config.
type V2CaseSpec struct {
	Schema        string   `json:"schema"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Category      string   `json:"category"`
	Prompts       []string `json:"prompts"`
	Expected      string   `json:"expected"`
	MaxTokens     int      `json:"max_tokens"`
	SourceKind    string   `json:"source_kind"`
	SourceURL     string   `json:"source_url"`
	SourceLicense string   `json:"source_license"`
	Tags          []string `json:"tags"`
}

func (s V2CaseSpec) Validate() error {
	if s.Schema != V2Schema {
		return fmt.Errorf("不支持的案例 schema")
	}
	if !v2CaseID.MatchString(s.ID) {
		return errors.New("案例 ID 必须为1-48位字母、数字、_或-")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(s.Name)); n < 1 || n > 80 {
		return errors.New("案例名称需要 1-80 字")
	}
	if !v2Category.MatchString(s.Category) {
		return errors.New("案例分类格式不合法")
	}
	if len(s.Prompts) < 1 || len(s.Prompts) > V2MaxTurns {
		return fmt.Errorf("每个案例最多 %d 轮", V2MaxTurns)
	}
	size := 0
	for _, value := range s.Prompts {
		n := utf8.RuneCountInString(strings.TrimSpace(value))
		if n < 5 || n > MaxPromptRunes {
			return errors.New("单轮题目长度需要 5-3000 字")
		}
		size += n
	}
	if size > V2MaxInputRunes {
		return errors.New("案例全部题目总长不能超过6000字")
	}
	if s.MaxTokens < 16 || s.MaxTokens > MaxTokensCeiling {
		return errors.New("单轮输出预算须在16-512 tokens")
	}
	if utf8.RuneCountInString(s.Expected) > 160 {
		return errors.New("标记最长160字；这不是真实绕过证明")
	}
	switch s.SourceKind {
	case "manual":
		if s.SourceLicense != "original" {
			return errors.New("手写案例来源许可须为 original")
		}
	case "imported":
		switch s.SourceLicense {
		case "MIT", "Apache-2.0", "CC0-1.0":
		default:
			return errors.New("导入案例仅接受已核实的MIT/Apache-2.0/CC0来源")
		}
		u, err := url.Parse(s.SourceURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("导入案例必须带合法的 HTTPS 来源")
		}
	default:
		return errors.New("来源类别仅允许 manual 或 imported")
	}
	if len(s.Tags) > 8 {
		return errors.New("最多8个标签")
	}
	for _, tag := range s.Tags {
		if n := utf8.RuneCountInString(strings.TrimSpace(tag)); n < 1 || n > 32 {
			return errors.New("标签长度需要1-32字")
		}
	}
	return nil
}

func (s V2CaseSpec) Canonical() (string, string, error) {
	if err := s.Validate(); err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(hash[:]), nil
}

// Score remains a literal evidence marker only; it NEVER asserts jailbreak.
func (s V2CaseSpec) Score(response string) string {
	return Score(s.Expected, response)
}
