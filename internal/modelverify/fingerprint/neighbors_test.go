package fingerprint

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRegenerateNeighbors 重算 data/neighbors.json。只在换参考库时手动跑：
//
//	OCTOPUS_REGEN_FP_NEIGHBORS=1 go test ./internal/modelverify/fingerprint -run TestRegenerateNeighbors
func TestRegenerateNeighbors(t *testing.T) {
	if os.Getenv("OCTOPUS_REGEN_FP_NEIGHBORS") != "1" {
		t.Skip("设置 OCTOPUS_REGEN_FP_NEIGHBORS=1 才重算近邻表")
	}
	det, err := LoadDetector()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.MarshalIndent(computeNeighbors(det), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("data/neighbors.json", append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNeighborsMatchDetector：近邻表必须与当前检测器配套、覆盖每个模型，且不含自己。
func TestNeighborsMatchDetector(t *testing.T) {
	table, err := LoadNeighbors()
	if err != nil {
		t.Fatalf("近邻表不可用（换库后要重算）: %v", err)
	}
	det, err := LoadDetector()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range det.ModelIDs {
		list := table.Models[id]
		if len(list) == 0 {
			t.Fatalf("%s 没有近邻", id)
		}
		for i, n := range list {
			if n.Model == id {
				t.Fatalf("%s 的近邻里出现了自己", id)
			}
			if i > 0 && n.Distance < list[i-1].Distance {
				t.Fatalf("%s 的近邻没有按距离排序", id)
			}
		}
	}
	if got := NeighborsOf(det.ModelIDs[0], 3); len(got) != 3 {
		t.Fatalf("NeighborsOf 应返回 3 个，得到 %d", len(got))
	}
}
