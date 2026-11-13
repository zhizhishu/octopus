package fingerprint

import "testing"

func TestCatalogBankAndCalibrationStayPaired(t *testing.T) {
	det, err := LoadDetector()
	if err != nil {
		t.Fatal(err)
	}
	models, err := LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Models) != 70 || len(det.ModelIDs) != 70 || len(det.ResponseCounts) != 70 {
		t.Fatalf("expected the reviewed 70-model snapshot: models=%d detector=%d counts=%d", len(models.Models), len(det.ModelIDs), len(det.ResponseCounts))
	}
	needed := map[string]bool{"gemini-4-argon": false, "gemini-4-carbon": false, "gemini-4-hydrogen": false}
	for i, model := range models.Models {
		if model.ID != det.ModelIDs[i] || model.ResponseCount != det.ResponseCounts[i] {
			t.Fatalf("model %d does not match detector ordering/counts: %+v", i, model)
		}
		if _, ok := needed[model.ID]; ok {
			needed[model.ID] = model.Family == "gemini" && model.ResponseCount == 36
		}
	}
	for id, found := range needed {
		if !found {
			t.Errorf("missing paired Gemini 4 enrollment: %s", id)
		}
	}
	if det.Calibration == nil || det.Calibration.Binding.BaseSHA256 != det.BaseSHA256 ||
		det.Calibration.Binding.ReferenceSHA256 != models.ReferenceSHA256 ||
		len(det.Calibration.Binding.ModelIDs) != len(det.ModelIDs) {
		t.Fatal("calibration is not bound to the reviewed bank")
	}
	for i, id := range det.ModelIDs {
		if det.Calibration.Binding.ModelIDs[i] != id {
			t.Fatalf("calibration model order differs at %d", i)
		}
	}
}
