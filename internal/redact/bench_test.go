package redact

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// benchBody builds a chat body with n messages of ~1KB each, half containing secrets.
func benchBody(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-test","messages":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		if i%2 == 0 {
			fmt.Fprintf(&b, `{"role":"user","content":"message %d with contact a%d@example.com and some padding text to make this a realistic size for a chat turn %s"}`, i, i, strings.Repeat("x", 900))
		} else {
			fmt.Fprintf(&b, `{"role":"assistant","content":"reply %d %s"}`, i, strings.Repeat("y", 900))
		}
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkRedactJSONBody(b *testing.B) {
	e, err := NewEngine()
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{4, 20, 100} {
		body := benchBody(size)
		b.Run(fmt.Sprintf("messages=%d/bytes=%d", size, len(body)), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, _ := e.NewSession("HPSIBEG", "openai_chat", false)
				if _, err := s.RedactJSONBody(body); err != nil {
					b.Fatal(err)
				}
				s.Close()
			}
		})
	}
}

func BenchmarkRestoreJSONBody(b *testing.B) {
	e, err := NewEngine()
	if err != nil {
		b.Fatal(err)
	}
	s0, _ := e.NewSession("HPSIBEG", "openai_chat", false)
	redacted, err := s0.RedactJSONBody(benchBody(20))
	if err != nil {
		b.Fatal(err)
	}
	s0.Close()
	b.SetBytes(int64(len(redacted)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, _ := e.NewSession("HPSIBEG", "openai_chat", false)
		if _, err := s.RedactJSONBody(redacted); err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}

// realisticBody builds chat turns of natural English prose (word lengths 3-11).
func realisticBody(n int) []byte {
	r := rand.New(rand.NewSource(42))
	words := []string{"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog",
		"information", "development", "placeholder", "conversation", "analysis",
		"running", "through", "system", "gateway", "redaction", "streaming", "protocol"}
	var b strings.Builder
	b.WriteString(`{"model":"gpt-test","messages":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		var turn strings.Builder
		for w := 0; w < 150; w++ {
			turn.WriteString(words[r.Intn(len(words))])
			turn.WriteString(" ")
		}
		esc := strings.ReplaceAll(strings.ReplaceAll(turn.String(), `"`, `\"`), "\n", "")
		if i%2 == 0 {
			fmt.Fprintf(&b, `{"role":"user","content":"%s"}`, esc)
		} else {
			fmt.Fprintf(&b, `{"role":"assistant","content":"%s"}`, esc)
		}
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkRedactRealistic(b *testing.B) {
	e, err := NewEngine()
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{4, 20, 100} {
		body := realisticBody(size)
		b.Run(fmt.Sprintf("msgs=%d/bytes=%d", size, len(body)), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, _ := e.NewSession("HPSIBEG", "openai_chat", false)
				if _, err := s.RedactJSONBody(body); err != nil {
					b.Fatal(err)
				}
				s.Close()
			}
		})
	}
}
