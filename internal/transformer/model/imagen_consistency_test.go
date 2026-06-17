package model

import "testing"

// TestIsImagenModelConsistency locks the single Imagen detection helper to the
// behavior both the model package and the relay Gemini images bridge rely on.
// The bridge previously used strings.HasPrefix(name, "imagen") while this
// package used strings.Contains(name, "imagen-"); names like "imagen4" or
// "imagen" then disagreed, splitting :predict (Imagen) vs :generateContent
// (Gemini image) routing. IsImagenModel must now be the only source of truth.
func TestIsImagenModelConsistency(t *testing.T) {
	imagenNames := []string{
		"imagen-4.0-generate-001",
		"imagen-3.0-generate-002",
		"models/imagen-3.0-generate-002",
		"google/imagen-4",
		"Imagen-4.0-Generate-001",     // case-insensitive
		"  imagen-4.0-generate-001  ", // surrounding whitespace
		"imagen",
		"imagen4",
		"imagen2",
	}
	for _, name := range imagenNames {
		if !IsImagenModel(name) {
			t.Fatalf("expected %q to be detected as an Imagen model", name)
		}
		// Consistency invariant: every Imagen model must also be recognized as
		// an image-generation model, otherwise detection and :predict routing
		// disagree (the original bug).
		if !isImageGenerationModelName(name) {
			t.Fatalf("expected Imagen model %q to also be an image-generation model", name)
		}
	}

	notImagen := []string{
		"",
		"gemini-2.5-flash-image",
		"gemini-2.5-pro",
		"gpt-image-2",
		"dall-e-3",
		"grok-2-image",
		"flux.1-kontext-pro",
		"my-image-model",
	}
	for _, name := range notImagen {
		if IsImagenModel(name) {
			t.Fatalf("expected %q not to be detected as an Imagen model", name)
		}
	}
}
