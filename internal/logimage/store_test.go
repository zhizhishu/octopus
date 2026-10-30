package logimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPNG(seed string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte("body-"+seed)...)
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func useTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(SetDir(dir))
	return dir
}

func TestValidHashRejectsTraversalAndJunk(t *testing.T) {
	good := hashOf([]byte("x"))
	if !ValidHash(good) {
		t.Fatalf("valid hash rejected: %s", good)
	}
	for _, bad := range []string{
		"", "abc", good[:63], good + "0",
		"../../../../../../etc/passwd",
		"..%2F..%2Fetc%2Fpasswd" + good[:42],
		"ABCDEF" + good[6:], // 大写不收：文件名统一小写
		good[:62] + "/.",
		good[:63] + "g",
	} {
		if ValidHash(bad) {
			t.Fatalf("ValidHash(%q) should be false", bad)
		}
	}
	if _, _, ok := Find("../" + good[3:]); ok {
		t.Fatal("Find must reject an invalid hash before touching the filesystem")
	}
}

func TestSniffOnlyKnownImageTypes(t *testing.T) {
	cases := []struct {
		data []byte
		mime string
		ok   bool
	}{
		{testPNG("a"), "image/png", true},
		{[]byte("\xff\xd8\xff\xe0rest"), "image/jpeg", true},
		{[]byte("GIF89a....."), "image/gif", true},
		{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp", true},
		{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", false},
		{[]byte("%PDF-1.7"), "", false},
		{[]byte("ID3\x03audio"), "", false},
		{[]byte("\x89PN"), "", false},
	}
	for i, tc := range cases {
		mime, _, ok := Sniff(tc.data)
		if ok != tc.ok || mime != tc.mime {
			t.Fatalf("case %d: got %q %v want %q %v", i, mime, ok, tc.mime, tc.ok)
		}
	}
}

func TestEnqueueWritesOnceAndDedupes(t *testing.T) {
	dir := useTempDir(t)
	data := testPNG("dedupe")
	h := hashOf(data)
	if !Enqueue(h, "png", data) {
		t.Fatal("enqueue failed")
	}
	WaitIdle()
	p := filepath.Join(dir, h[:2], h+".png")
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("file not written correctly: %v", err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", st.Mode().Perm())
	}

	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	// 第二次：同一哈希不再入队，只刷新 mtime（孤儿清扫据此知道它刚被用过）。
	if !Enqueue(h, "png", data) {
		t.Fatal("dedupe enqueue should succeed")
	}
	WaitIdle()
	st, _ = os.Stat(p)
	if time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("dedupe hit must refresh mtime, got %v", st.ModTime())
	}
	files, err := List()
	if err != nil || len(files) != 1 || files[0].Hash != h || files[0].Ext != "png" {
		t.Fatalf("expected exactly one file, got %+v (%v)", files, err)
	}
}

func TestEnqueueRejectsBadInput(t *testing.T) {
	useTempDir(t)
	data := testPNG("bad")
	h := hashOf(data)
	if Enqueue("../"+h[3:], "png", data) {
		t.Fatal("invalid hash accepted")
	}
	if Enqueue(h, "svg", data) {
		t.Fatal("unknown ext accepted")
	}
	if Enqueue(h, "png", make([]byte, MaxImageBytes+1)) {
		t.Fatal("oversized image accepted")
	}
	WaitIdle()
	if files, _ := List(); len(files) != 0 {
		t.Fatalf("nothing should be written, got %+v", files)
	}
}

func TestEnqueueQueueFullReturnsFalseWithoutBlocking(t *testing.T) {
	useTempDir(t)
	resume := PauseWorkerForTest()
	accepted, rejected := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueSlots*3; i++ {
			d := testPNG(fmt.Sprintf("q%d", i))
			if Enqueue(hashOf(d), "png", d) {
				accepted++
			} else {
				rejected++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		resume()
		t.Fatal("Enqueue blocked on a full queue")
	}
	resume()
	WaitIdle()
	if rejected == 0 {
		t.Fatalf("expected some rejections once the queue is full (accepted=%d)", accepted)
	}
	if accepted > queueSlots+1 {
		t.Fatalf("accepted %d > queue capacity", accepted)
	}
	files, _ := List()
	if len(files) != accepted {
		t.Fatalf("accepted %d but wrote %d files", accepted, len(files))
	}
}

func TestListSkipsJunkAndRemoveDeletes(t *testing.T) {
	dir := useTempDir(t)
	data := testPNG("list")
	h := hashOf(data)
	Enqueue(h, "png", data)
	WaitIdle()
	_ = os.WriteFile(filepath.Join(dir, h[:2], "notes.txt"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "zz"), 0o700)
	other := hashOf([]byte("other"))
	_ = os.WriteFile(filepath.Join(dir, "zz", other+".png"), []byte("x"), 0o600) // 子目录名对不上哈希
	staleTmp := filepath.Join(dir, h[:2], tmpPrefix+"abc")
	_ = os.WriteFile(staleTmp, []byte("x"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(staleTmp, old, old)

	files, err := List()
	if err != nil || len(files) != 1 || files[0].Hash != h {
		t.Fatalf("List: %+v %v", files, err)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatal("stale temp file should be removed by List")
	}
	if err := Remove(h); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Find(h); ok {
		t.Fatal("file still present after Remove")
	}
}

func TestMaxTotalBytesEnvOverride(t *testing.T) {
	t.Setenv(envMaxBytes, "")
	if MaxTotalBytes() != DefaultMaxTotalBytes {
		t.Fatal("default cap expected")
	}
	t.Setenv(envMaxBytes, "12345")
	if MaxTotalBytes() != 12345 {
		t.Fatal("env override ignored")
	}
	t.Setenv(envMaxBytes, "-1")
	if MaxTotalBytes() != DefaultMaxTotalBytes {
		t.Fatal("invalid override must fall back to the default")
	}
}

func TestDirEnvOverride(t *testing.T) {
	restore := SetDir("")
	defer restore()
	t.Setenv(envDir, "/tmp/octopus-img-env")
	if Dir() != "/tmp/octopus-img-env" {
		t.Fatalf("env dir ignored: %s", Dir())
	}
	t.Setenv(envDir, "")
	if Dir() != defaultDir {
		t.Fatalf("default dir expected, got %s", Dir())
	}
}
