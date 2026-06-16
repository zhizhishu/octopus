package model

import "testing"

func TestNormalizeAPIKeyEndpointFamiliesAcceptsOpenAIAlias(t *testing.T) {
	families, err := NormalizeAPIKeyEndpointFamilies([]APIKeyEndpointFamily{"openai", APIKeyEndpointFamilyGemini})
	if err != nil {
		t.Fatalf("normalize endpoint families: %v", err)
	}
	want := []APIKeyEndpointFamily{
		APIKeyEndpointFamilyOpenAICompatible,
		APIKeyEndpointFamilyGemini,
	}
	if len(families) != len(want) {
		t.Fatalf("families length: got %#v want %#v", families, want)
	}
	for i := range want {
		if families[i] != want[i] {
			t.Fatalf("families: got %#v want %#v", families, want)
		}
	}
}
