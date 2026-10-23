package client

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TLS 1.3 密码套件的 IANA 名字是固定的（RFC 8446 / IANA TLS Cipher Suites）。
//
// 曾经 tls_faces.go 里 0x1301 和 0x1303 的注释是**写反的**：0x1301 标成
// CHACHA20、0x1303 标成 AES_128。数值本身对（跟真机抓包一致），但注释错了，
// 而"注释写着一个名字、值是另一个"正是最危险的形态：下一个人会按注释去
// 修正数值，一次 shape 事故就这么来的。
//
// 这条用**权威映射表**核对源码里每一处 `0x13xx, // NAME` 注释。
var tls13CipherNames = map[uint16]string{
	0x1301: "TLS_AES_128_GCM_SHA256",
	0x1302: "TLS_AES_256_GCM_SHA384",
	0x1303: "TLS_CHACHA20_POLY1305_SHA256",
	0x1304: "TLS_AES_128_CCM_SHA256",
	0x1305: "TLS_AES_128_CCM_8_SHA256",
}

func TestTLS13CipherCommentsMatchIANA(t *testing.T) {
	src, err := os.ReadFile("tls_faces.go")
	if err != nil {
		t.Fatalf("read tls_faces.go: %v", err)
	}

	// 按行扫：每个密码套件条目是 `0x1301, // TLS_AES_128_GCM_SHA256` 这种同行注释。
	// 不走 ast.CommentGroup：行尾注释在 AST 里是独立 group，按列位置反查更绕，
	// 而这里要核对的恰恰就是"值和它旁边那句注释"的对应关系。
	re := regexp.MustCompile("^\\s*(0x13[0-9a-fA-F]{2})\\s*,\\s*//\\s*(.*)$")

	checked := 0
	for i, line := range strings.Split(string(src), "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		code, err := strconv.ParseUint(m[1], 0, 16)
		if err != nil {
			continue
		}
		want, ok := tls13CipherNames[uint16(code)]
		if !ok {
			continue
		}
		comment := m[2]
		if !strings.Contains(strings.ToUpper(comment), want) {
			t.Errorf("tls_faces.go:%d: 注释 %q 与 IANA 不符，0x%04x 应当是 %s\n这种错误会让后来者按注释去改数值，造成 shape 事故",
				i+1, strings.TrimSpace(comment), code, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没有核对到任何 TLS 1.3 密码套件注释：本测试失去了意义，说明注释格式变了，请同步更新解析逻辑")
	}
}
