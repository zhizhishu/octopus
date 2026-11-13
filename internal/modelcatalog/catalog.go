// Package modelcatalog contains shared capability hints for explicitly registered models.
// It does not select channels, grant model access, rewrite model IDs, or choose wire fields.
package modelcatalog

import "strings"

// Metadata is descriptive only. An empty Tokenizer means it is unknown; a family
// match is not evidence that a model uses another model's tokenizer.
type Metadata struct {
	Family          string
	ImageGeneration bool
	Reasoning       bool
	Tokenizer       string
}

var entries = map[string]Metadata{
	"gemini-4-argon":                   {Family: "google", Reasoning: true},
	"gemini-4-carbon":                  {Family: "google", Reasoning: true},
	"gemini-4-hydrogen":                {Family: "google", Reasoning: true},
	"gpt-image-2":                      {Family: "openai", ImageGeneration: true},
	"gpt-image-2.5":                    {Family: "openai", ImageGeneration: true},
	"gpt-image-2.6":                    {Family: "openai", ImageGeneration: true},
	"doubao-seedream-5-0-pro-260628":   {Family: "bytedance", ImageGeneration: true},
	"doubao-seedream-5-0-lite-260128":  {Family: "bytedance", ImageGeneration: true},
	"doubao-seedream-5-0-flash-260915": {Family: "bytedance", ImageGeneration: true},
	"doubao-seedream-5-0-260128":       {Family: "bytedance", ImageGeneration: true},
	"doubao-seedream-5-5-261028":       {Family: "bytedance", ImageGeneration: true},
	"imagen-4.0-generate-001":          {Family: "google", ImageGeneration: true},
	"imagen-4.0-ultra-generate-001":    {Family: "google", ImageGeneration: true},
	"imagen-4.0-fast-generate-001":     {Family: "google", ImageGeneration: true},
}

// Lookup recognizes a model ID, including a provider path, for metadata lookup
// only. Callers must keep the original ID when storing or forwarding a request.
// In particular, -high and -xhigh are part of the full model ID here; they do not
// synthesize reasoning_effort, thinkingLevel, or a reasoning budget.
func Lookup(modelID string) (Metadata, bool) {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	if metadata, ok := entries[name]; ok {
		return metadata, true
	}
	for _, base := range []string{"gemini-4-argon", "gemini-4-carbon", "gemini-4-hydrogen"} {
		if suffix, ok := strings.CutPrefix(name, base); ok && knownGemini4Suffix(suffix) {
			return entries[base], true
		}
	}
	return Metadata{}, false
}

// These are the observed qualifier/date forms. Recognition is a family hint,
// not a claim that every combination is enabled on a channel.
func knownGemini4Suffix(suffix string) bool {
	for _, date := range []string{"-20261013", "-20261110", "-20261112"} {
		if strings.HasSuffix(suffix, date) {
			suffix = strings.TrimSuffix(suffix, date)
			break
		}
	}
	switch suffix {
	case "", "-high", "-xhigh", "-b", "-cyber-permissive":
		return true
	default:
		return false
	}
}

// SameFamily reconciles the three reference-bank labels that differ from the
// audit labels. Keep this at the comparison boundary: published family labels,
// candidate ordering and model IDs retain their existing vocabulary.
func SameFamily(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return comparisonFamily(left) == comparisonFamily(right)
}

func comparisonFamily(family string) string {
	switch family {
	case "claude":
		return "anthropic"
	case "gpt":
		return "openai"
	case "gemini":
		return "google"
	default:
		return family
	}
}
