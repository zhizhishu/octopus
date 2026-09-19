package behavior

import (
	"encoding/base64"
	"math"
	"strings"
	"unicode"
)

// Claude extended thinking 的 thinking.signature 是 Anthropic 用自己私钥签发的
// AEAD 密文（外层 base64，内层 protobuf），并且**绑定了模型名**。中转站可以改写
// 文本、可以冒充模型名，但无法伪造一段能被 Anthropic 自己解开的密文——伪造的密文
// 要么结构不对，要么在回放时解封不出内容。
//
// 这是本项目能拿到的最硬证据，且不需要任何基线。本文件只做「结构判定」与解析；
// 采集与回放流程在 signature_probe.go。

// signatureModelField 是 protobuf 里绑定模型名的字段号。
//
// 结构（上游手写解析器逐字对应）：
//
//	raw    = base64decode(sig)
//	inner  = raw   的 2 号字段（len-delimited）
//	header = inner 的 1 号字段（len-delimited）
//	  header 的 6 号字段 -> 绑定的模型名（utf-8 且可打印才采信）
//	  header 的 8 号字段 -> block type
//	inner 的 2/3/4 号字段 -> nonce 段（只记长度）
//	inner 的 5 号字段     -> ciphertext（记长度 + 香农熵）
const (
	sigFieldModelName  = 6
	sigFieldBlockType  = 8
	sigOuterInnerField = 2
	sigInnerHeaderFld  = 1
	sigCiphertextField = 5
)

// 结构判定的阈值，与上游一致。
//
// 熵阈值取 7.5：真 AEAD 密文的字节熵应逼近 8.0（即接近均匀分布），
// 明显低于这个值说明那段字节不是密文，而是被填了可读内容或重复字节。
const (
	sigMinCiphertextEntropy = 7.5
	sigMinNonceSegments     = 2
)

// SignatureInfo 一次 thinking.signature 的结构解析结果。
//
// 注意 OK 的含义被收窄为「这段签名在结构上像真 AEAD 密文且绑定了模型名」，
// 它**不等于**签名是真的。真正的验证只有一条路：把签名回放给上游，
// 看它能不能解封出推理内容（见 signature_probe.go）。
type SignatureInfo struct {
	// OK 结构判定通过。
	OK bool
	// BoundModel 是签名内绑定的模型名。中转站若把 A 模型的签名安在 B 模型上，
	// 这里读出来的名字会和请求的模型名对不上。
	BoundModel string
	// BlockType 是 thinking(1) / redacted_thinking(2) 之类的块类型。
	BlockType uint64
	// NonceLengths 各 nonce 段长度。
	NonceLengths []int
	// CiphertextLen 密文段长度（字节）。
	CiphertextLen int
	// CiphertextEntropy 密文段香农熵。
	CiphertextEntropy float64
	// Reason 未通过时说明原因，通过时为空。
	Reason string
}

// ParseSignature 解析一段 thinking.signature。
//
// 任何一步失败都返回 OK=false 与原因，不返回 error——"这段签名不合结构"
// 本身就是审计结论，不是调用错误。
func ParseSignature(sig string) SignatureInfo {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return SignatureInfo{Reason: "签名为空"}
	}
	// base64 可能缺 padding，补齐到 4 的倍数。
	if pad := len(sig) % 4; pad != 0 {
		sig += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(sig, "="))
		if err != nil {
			return SignatureInfo{Reason: "base64 解码失败"}
		}
	}
	inner, ok := protoLenField(raw, sigOuterInnerField)
	if !ok {
		return SignatureInfo{Reason: "缺少外层 inner 字段"}
	}
	header, ok := protoLenField(inner, sigInnerHeaderFld)
	if !ok {
		return SignatureInfo{Reason: "缺少 header 字段"}
	}

	info := SignatureInfo{}
	if name, ok := protoLenField(header, sigFieldModelName); ok {
		// 只在确实可打印时才采信，避免把密文噪声当成模型名。
		if txt := strings.TrimSpace(string(name)); txt != "" && isPrintableUTF8(txt) {
			info.BoundModel = txt
		}
	}
	if v, ok := protoVarintField(header, sigFieldBlockType); ok {
		info.BlockType = v
	}
	for _, num := range []int{2, 3, 4} {
		if seg, ok := protoLenField(inner, num); ok {
			info.NonceLengths = append(info.NonceLengths, len(seg))
		}
	}
	if ct, ok := protoLenField(inner, sigCiphertextField); ok {
		info.CiphertextLen = len(ct)
		info.CiphertextEntropy = shannonEntropy(ct)
	}

	switch {
	case info.BoundModel == "":
		info.Reason = "签名未绑定模型名"
	case len(info.NonceLengths) < sigMinNonceSegments:
		info.Reason = "nonce 段数不足"
	case info.CiphertextLen == 0:
		info.Reason = "缺少密文段"
	case info.CiphertextLen >= sigEntropyMinSample &&
		info.CiphertextEntropy < expectedMaxEntropy(info.CiphertextLen)-sigEntropyTolerance:
		info.Reason = "密文段熵值偏低，不像 AEAD 密文"
	default:
		info.OK = true
	}
	return info
}

// 熵判定的两个常量。
//
// 香农熵是**有限样本**的统计量，n 个字节最多只能测得约 log2(n) 比特：
// 一段 256 字节的完美随机数据，期望熵只有约 7.28，永远到不了 8。
// 上游用固定阈值 7.5，在 n < 512 时连真密文都会被判成"低熵伪造"。
// 所以阈值必须随样本量缩放：判据取"该样本量下的理论期望上限 - 容差"。
// 真密文紧贴期望上限，而填充成可读文本或重复字节的假密文会远低于它。
const (
	// sigEntropyTolerance 是实测熵相对理论上限允许的落差（比特/字节）。
	sigEntropyTolerance = 1.0
	// sigEntropyMinSample 是启用熵判定的最小密文长度。样本太少时理论期望上限
	// 本身就很低（64 字节只有约 5.6），判据失去区分力，此时直接跳过这一项——
	// 绑定模型名与 nonce 段数两项仍照常判定。
	sigEntropyMinSample = 128
)

// expectedMaxEntropy 返回 n 个字节序列在理论上可期望的最大香农熵。
//
// 模型：n 个样本随机投入 k 个取值（k=min(n,256)），期望熵约为
// log2(k) - (k-1)/(2n·ln2)。这是有限样本下的固有偏低，与数据是否真随机无关。
func expectedMaxEntropy(n int) float64 {
	if n <= 1 {
		return 0
	}
	domain := 256.0
	if float64(n) < domain {
		domain = float64(n)
	}
	return math.Log2(domain) - (domain-1)/(2*float64(n)*math.Ln2)
}

// MatchSignatureModel 判断签名绑定的模型名与请求的模型名是否一致。
//
// 大小写不敏感，并容忍上游在型号上追加日期/版本后缀（如 -20250929），
// 那属于正常的别名解析，不是替身特征。两边任一为空都返回 false——
// 「读不出模型名」不能当成「一致」。
func MatchSignatureModel(bound, requested string) bool {
	bound = strings.ToLower(strings.TrimSpace(bound))
	requested = strings.ToLower(strings.TrimSpace(requested))
	if bound == "" || requested == "" {
		return false
	}
	if bound == requested {
		return true
	}
	// 容忍上游补的日期后缀：claude-x-4-5 vs claude-x-4-5-20250929。
	return strings.HasPrefix(bound, requested+"-") || strings.HasPrefix(requested, bound+"-")
}

// ---------------------------------------------------------------- protobuf 遍历

// walkProto 遍历一段 protobuf，逐字段回调；回调返回 false 时提前停止。
//
// 手写而非引 protobuf 库：这里只需要读出几个字段，而且签名是**外部输入**，
// 引一个完整解析器等于把一个攻击面引进审计路径。遇到未知 wire type 直接停止
// （与上游一致）——继续走会错位，之后读到的字段号全不可信。
func walkProto(buf []byte, fn func(num, wire int, val uint64, bytes []byte) bool) {
	i := 0
	for i < len(buf) {
		tag, n := readVarint(buf[i:])
		if n == 0 {
			return
		}
		i += n
		num := int(tag >> 3)
		wire := int(tag & 0x7)
		if num == 0 {
			return
		}
		switch wire {
		case 0: // varint
			v, n := readVarint(buf[i:])
			if n == 0 {
				return
			}
			i += n
			if !fn(num, wire, v, nil) {
				return
			}
		case 1: // 64-bit
			if i+8 > len(buf) {
				return
			}
			i += 8
			if !fn(num, wire, 0, nil) {
				return
			}
		case 2: // len-delimited
			length, n := readVarint(buf[i:])
			if n == 0 {
				return
			}
			i += n
			if length > uint64(len(buf)-i) {
				return
			}
			seg := buf[i : i+int(length)]
			i += int(length)
			if !fn(num, wire, 0, seg) {
				return
			}
		case 5: // 32-bit
			if i+4 > len(buf) {
				return
			}
			i += 4
			if !fn(num, wire, 0, nil) {
				return
			}
		default:
			// 未知 wire type：位置不可信，停止。
			return
		}
	}
}

func protoLenField(buf []byte, want int) ([]byte, bool) {
	var out []byte
	found := false
	walkProto(buf, func(num, wire int, _ uint64, seg []byte) bool {
		if num == want && wire == 2 {
			out = seg
			found = true
			return false
		}
		return true
	})
	return out, found
}

func protoVarintField(buf []byte, want int) (uint64, bool) {
	var out uint64
	found := false
	walkProto(buf, func(num, wire int, val uint64, _ []byte) bool {
		if num == want && wire == 0 {
			out = val
			found = true
			return false
		}
		return true
	})
	return out, found
}

// readVarint 读一个 protobuf varint，返回 (值, 消耗字节数)；越界或超长返回 n=0。
func readVarint(buf []byte) (uint64, int) {
	var out uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		b := buf[i]
		out |= uint64(b&0x7f) << (7 * uint(i))
		if b < 0x80 {
			return out, i + 1
		}
	}
	return 0, 0
}

// shannonEntropy 计算字节序列的香农熵（bit/byte）。
func shannonEntropy(buf []byte) float64 {
	if len(buf) == 0 {
		return 0
	}
	var counts [256]int
	for _, b := range buf {
		counts[b]++
	}
	total := float64(len(buf))
	var e float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		e -= p * math.Log2(p)
	}
	return e
}

func isPrintableUTF8(s string) bool {
	for _, r := range s {
		if r == unicode.ReplacementChar || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
