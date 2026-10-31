package fingerprint

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// 指纹近邻：每个模型和库里哪几个模型"本来就难分开"。
//
// 结果页说"最像 X"时，用户最想知道的下一件事是"X 会不会和别的型号混"。
// 相邻小版本、同一底座的变体，取数习惯几乎一样，排名靠前的几名互换很正常；
// 把这层先天的模糊摆出来，用户才不会把"第二名是同族另一个型号"读成证据。
//
// 计算是离线的（data/neighbors.json），运行时只读表：全量两两比较要做几十亿次
// 浮点运算，不能放进检测路径。表与检测器绑定 bank_built_at，换库必须重算
// （见 TestNeighborsMatchDetector 和 TestRegenerateNeighbors）。
//
// 距离的定义（我们自己的口径，概念参考 lm-detector 的 nearestModels）：
// 以模型 A 自己的参考回答为准，先算 A 的"自身离散度"——每条回答到 A 其余回答的
// k 近邻距离取中位数；再算 A 的回答到 B 全部回答的 k 近邻距离中位数，除以 A 的
// 离散度。1.0 表示"B 离 A 跟 A 自己内部一样近"，越接近 1 越难分。

//go:embed data/neighbors.json
var neighborsBlob []byte

// neighborsKeep 是每个模型保留的近邻个数。结果页只显示前三个，多存两个留余地。
const neighborsKeep = 5

// Neighbor 是一个近邻及其相对距离。
type Neighbor struct {
	Model    string  `json:"model"`
	Distance float64 `json:"distance"`
}

// NeighborTable 是离线算好的近邻表。
type NeighborTable struct {
	Schema      string                `json:"schema"`
	BankBuiltAt string                `json:"bank_built_at"`
	Models      map[string][]Neighbor `json:"models"`
}

const neighborSchema = "octopus-fingerprint-neighbors-v1"

var (
	neighborsOnce   sync.Once
	neighborsCached *NeighborTable
	neighborsErr    error
)

// LoadNeighbors 读取近邻表。表与检测器不配套时返回错误——过期的近邻比没有更糟。
func LoadNeighbors() (*NeighborTable, error) {
	neighborsOnce.Do(func() {
		var table NeighborTable
		if err := json.Unmarshal(neighborsBlob, &table); err != nil {
			neighborsErr = fmt.Errorf("近邻表解析失败: %w", err)
			return
		}
		det, err := LoadDetector()
		if err != nil {
			neighborsErr = err
			return
		}
		if table.Schema != neighborSchema || table.BankBuiltAt != det.BankBuiltAt {
			neighborsErr = fmt.Errorf("近邻表与检测器不配套：表 %q / 检测器 %q", table.BankBuiltAt, det.BankBuiltAt)
			return
		}
		neighborsCached = &table
	})
	return neighborsCached, neighborsErr
}

// NeighborsOf 返回某模型最难分开的前 n 个近邻；表不可用时返回 nil（只是少一条提示）。
func NeighborsOf(model string, n int) []Neighbor {
	table, err := LoadNeighbors()
	if err != nil || table == nil {
		return nil
	}
	list := table.Models[model]
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return append([]Neighbor(nil), list...)
}

// computeNeighbors 从检测器的参考回答离线计算近邻表。
func computeNeighbors(det *Detector) *NeighborTable {
	refs := det.Ranker.References
	table := &NeighborTable{
		Schema:      neighborSchema,
		BankBuiltAt: det.BankBuiltAt,
		Models:      make(map[string][]Neighbor, len(refs)),
	}
	for a, own := range refs {
		spreadSamples := make([]float64, 0, len(own))
		for i, x := range own {
			rest := make([][]float64, 0, len(own)-1)
			rest = append(rest, own[:i]...)
			rest = append(rest, own[i+1:]...)
			spreadSamples = append(spreadSamples, nearest(x, rest))
		}
		spread := median(spreadSamples)
		if spread <= 0 {
			spread = 1e-12
		}
		list := make([]Neighbor, 0, len(refs)-1)
		for b, other := range refs {
			if b == a {
				continue
			}
			ds := make([]float64, 0, len(own))
			for _, x := range own {
				ds = append(ds, nearest(x, other))
			}
			list = append(list, Neighbor{Model: det.ModelIDs[b], Distance: median(ds) / spread})
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Distance < list[j].Distance })
		if len(list) > neighborsKeep {
			list = list[:neighborsKeep]
		}
		for i := range list {
			// 三位小数足够：这是给人看的"难分程度"，不参与任何判定。
			list[i].Distance = float64(int(list[i].Distance*1000+0.5)) / 1000
		}
		table.Models[det.ModelIDs[a]] = list
	}
	return table
}
