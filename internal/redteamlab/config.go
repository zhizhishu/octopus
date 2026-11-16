package redteamlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxCases         = 8
	MaxPromptRunes   = 3000
	MaxResponseRunes = 4000
	MaxTokensCeiling = 512
)

var caseIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)

type Case struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Expected  string `json:"expected"`
	MaxTokens int    `json:"max_tokens"`
}
type Config struct {
	Enabled bool   `json:"enabled"`
	Cases   []Case `json:"cases"`
}

func Parse(raw string) (Config, error) {
	var c Config
	if strings.TrimSpace(raw) == "" {
		return Config{Cases: []Case{}}, nil
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("invalid lab config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if c.Cases == nil {
		c.Cases = []Case{}
	}
	return c, nil
}

func (c Config) Validate() error {
	if len(c.Cases) > MaxCases {
		return fmt.Errorf("最多保存 %d 个独立测试案例", MaxCases)
	}
	seen := make(map[string]bool, len(c.Cases))
	for _, item := range c.Cases {
		if !caseIDPattern.MatchString(item.ID) || seen[item.ID] {
			return errors.New("案例 ID 不合法或重复")
		}
		seen[item.ID] = true
		if n := utf8.RuneCountInString(strings.TrimSpace(item.Name)); n < 1 || n > 80 {
			return errors.New("案例名称长度需在 1-80 字内")
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(item.Prompt)); n < 5 || n > MaxPromptRunes {
			return fmt.Errorf("案例题目需在 5-%d 字之间", MaxPromptRunes)
		}
		if utf8.RuneCountInString(item.Expected) > 160 {
			return errors.New("匹配标记不能超过 160 字")
		}
		if item.MaxTokens < 16 || item.MaxTokens > MaxTokensCeiling {
			return fmt.Errorf("单次输出预算需在 16-%d tokens", MaxTokensCeiling)
		}
	}
	return nil
}

func FindCase(c Config, id string) (Case, bool) {
	for _, item := range c.Cases {
		if item.ID == id {
			return item, true
		}
	}
	return Case{}, false
}

// Score only checks a literal marker. It does NOT assert actual safety-policy bypass.
func Score(expected, response string) string {
	if strings.TrimSpace(expected) == "" {
		return "unscored"
	}
	if strings.Contains(response, expected) {
		return "matched"
	}
	return "not_matched"
}

func ClipResponse(value string) (string, bool) {
	r := []rune(value)
	if len(r) <= MaxResponseRunes {
		return value, false
	}
	return string(r[:MaxResponseRunes]), true
}
