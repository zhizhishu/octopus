package op

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/logimage"
	"github.com/zhizhishu/octopus/internal/model"
)

// 本包测试的图片一律写临时目录，绝不落进源码树的 data/log_images。
func init() {
	dir, err := os.MkdirTemp("", "octopus-op-logimg-")
	if err != nil {
		panic(err)
	}
	logimage.SetDir(dir)
}

type testImage struct {
	hash string
	path string
}

// putImage 直接在图片目录放一个文件，并把 mtime 调到 age 之前。
func putImage(t *testing.T, seed string, size int, age time.Duration) testImage {
	t.Helper()
	data := make([]byte, size)
	copy(data, "\x89PNG\r\n\x1a\n")
	copy(data[8:], seed)
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	p := filepath.Join(logimage.Dir(), h[:2], h+".png")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return testImage{hash: h, path: p}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func setupLogImageTest(t *testing.T) context.Context {
	t.Helper()
	t.Cleanup(logimage.SetDir(t.TempDir()))
	t.Cleanup(logimage.WaitIdle)
	return setupRelayLogTest(t)
}

func refCount(t *testing.T, where string, args ...any) int64 {
	t.Helper()
	var n int64
	q := db.GetDB().Model(&model.RelayLogImage{})
	if where != "" {
		q = q.Where(where, args...)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRelayLogFlushWritesImageRefs(t *testing.T) {
	ctx := setupLogImageTest(t)
	a := putImage(t, "a", 100, 0)
	b := putImage(t, "b", 100, 0)
	if err := RelayLogAdd(ctx, model.RelayLog{
		RequestModelName:    "m",
		UpstreamRequestBody: "x",
		// 重复与非法哈希都不该写进引用表。
		UpstreamImageHashes: []string{a.hash, b.hash, a.hash, "../etc/passwd"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, ""); n != 2 {
		t.Fatalf("refs = %d, want 2", n)
	}
	// UpstreamImageHashes 不是列，也不出现在 API 里。
	var logs []model.RelayLog
	db.GetDB().Find(&logs)
	if len(logs) != 1 || len(logs[0].UpstreamImageHashes) != 0 {
		t.Fatalf("hashes must not round-trip through relay_logs: %+v", logs)
	}
}

func TestRelayLogImageOrphanSweep(t *testing.T) {
	ctx := setupLogImageTest(t)
	oldOrphan := putImage(t, "old-orphan", 100, 2*time.Hour)
	youngOrphan := putImage(t, "young-orphan", 100, 10*time.Minute)
	oldReferenced := putImage(t, "old-ref", 100, 5*time.Hour)
	if err := db.GetDB().Create(&model.RelayLog{ID: 5001, Time: time.Now().Unix()}).Error; err != nil {
		t.Fatal(err)
	}
	db.GetDB().Create(&model.RelayLogImage{LogID: 5001, Hash: oldReferenced.hash})
	// 指向已不存在日志的引用行：清扫时要删掉，否则文件永远「有引用」。
	danglingTarget := putImage(t, "dangling", 100, 3*time.Hour)
	db.GetDB().Create(&model.RelayLogImage{LogID: 999999, Hash: danglingTarget.hash})

	n, err := RelayLogImageSweep(ctx, RelayLogImageOrphanGrace)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	if exists(oldOrphan.path) || exists(danglingTarget.path) {
		t.Fatal("unreferenced files older than 1h must be removed")
	}
	if !exists(youngOrphan.path) {
		t.Fatal("young unreferenced file must survive the grace period")
	}
	if !exists(oldReferenced.path) {
		t.Fatal("referenced file must be kept")
	}
	if refCount(t, "log_id = ?", 999999) != 0 {
		t.Fatal("dangling reference row should be pruned")
	}
}

func TestRelayLogImageCleanupOnRetention(t *testing.T) {
	ctx := setupLogImageTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogKeepPeriod, "1"); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour).Unix()
	logs := []model.RelayLog{{ID: 6001, Time: oldTime}, {ID: 6002, Time: time.Now().Unix()}}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	onlyOld := putImage(t, "only-old", 100, 3*time.Hour)
	shared := putImage(t, "shared", 100, 3*time.Hour)
	db.GetDB().Create(&[]model.RelayLogImage{
		{LogID: 6001, Hash: onlyOld.hash},
		{LogID: 6001, Hash: shared.hash},
		{LogID: 6002, Hash: shared.hash},
	})

	if err := RelayLogSaveDBTask(ctx); err != nil {
		t.Fatal(err)
	}
	if refCount(t, "log_id = ?", 6001) != 0 {
		t.Fatal("refs of expired log must be deleted")
	}
	if exists(onlyOld.path) {
		t.Fatal("image only used by the expired log must be deleted")
	}
	if !exists(shared.path) {
		t.Fatal("image still used by a live log must be kept")
	}
}

func TestRelayLogImageCleanupOnClear(t *testing.T) {
	ctx := setupLogImageTest(t)
	if err := db.GetDB().Create(&model.RelayLog{ID: 7001, Time: time.Now().Unix()}).Error; err != nil {
		t.Fatal(err)
	}
	img := putImage(t, "clear", 100, 5*time.Minute)
	justWritten := putImage(t, "just-written", 100, 0)
	db.GetDB().Create(&model.RelayLogImage{LogID: 7001, Hash: img.hash})

	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if refCount(t, "") != 0 {
		t.Fatal("clear must drop all image refs")
	}
	if exists(img.path) {
		t.Fatal("clear should free the images of cleared logs right away")
	}
	if !exists(justWritten.path) {
		t.Fatal("an image written seconds ago may belong to a log still being flushed")
	}
}

func TestRelayLogImageCleanupOnStorageCap(t *testing.T) {
	ctx := setupLogImageTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000001"); err != nil {
		t.Fatal(err)
	}
	logs := []model.RelayLog{
		{ID: 8001, Time: 8001, RequestContent: string(make([]byte, 900))},
		{ID: 8002, Time: 8002, RequestContent: string(make([]byte, 900))},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	img := putImage(t, "cap", 100, 3*time.Hour)
	db.GetDB().Create(&model.RelayLogImage{LogID: 8001, Hash: img.hash})

	if err := RelayLogSaveDBTask(ctx); err != nil {
		t.Fatal(err)
	}
	if refCount(t, "log_id = ?", 8001) != 0 {
		t.Fatal("capacity purge must delete refs of purged logs")
	}
	if exists(img.path) {
		t.Fatal("image of a purged log must be swept")
	}
}

func TestRelayLogImageTotalCapEvictsUnreferencedFirst(t *testing.T) {
	ctx := setupLogImageTest(t)
	t.Setenv("OCTOPUS_LOG_IMAGE_MAX_BYTES", "2500")
	if err := db.GetDB().Create(&model.RelayLog{ID: 9001, Time: time.Now().Unix()}).Error; err != nil {
		t.Fatal(err)
	}
	// 4 × 1000 字节，上限 2500：要删两张。过了一分钟的无引用文件先删（即使它更新），
	// 再删有引用里最旧的，并删掉它的引用行。
	refOldest := putImage(t, "ref-oldest", 1000, 4*time.Hour)
	refNewer := putImage(t, "ref-newer", 1000, 3*time.Hour)
	unrefYoung := putImage(t, "unref-young", 1000, 10*time.Minute)
	refNewest := putImage(t, "ref-newest", 1000, 2*time.Hour)
	for _, im := range []testImage{refOldest, refNewer, refNewest} {
		db.GetDB().Create(&model.RelayLogImage{LogID: 9001, Hash: im.hash})
	}

	n, err := RelayLogImageSweep(ctx, RelayLogImageOrphanGrace)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	if exists(unrefYoung.path) || exists(refOldest.path) {
		t.Fatal("expected the unreferenced file and then the oldest referenced one to go")
	}
	if !exists(refNewer.path) || !exists(refNewest.path) {
		t.Fatal("newer referenced files must stay")
	}
	if refCount(t, "hash = ?", refOldest.hash) != 0 {
		t.Fatal("evicted image's reference rows must be removed")
	}
	if refCount(t, "") != 2 {
		t.Fatal("other refs must stay")
	}
}

func TestRelayLogImageSweepEmptyDirIsNoop(t *testing.T) {
	ctx := setupLogImageTest(t)
	t.Cleanup(logimage.SetDir(filepath.Join(t.TempDir(), "missing")))
	if n, err := RelayLogImageSweep(ctx, 0); err != nil || n != 0 {
		t.Fatalf("sweep on missing dir: n=%d err=%v", n, err)
	}
}

func TestRelayLogImageTotalCapSparesJustWrittenUnreferenced(t *testing.T) {
	ctx := setupLogImageTest(t)
	t.Setenv("OCTOPUS_LOG_IMAGE_MAX_BYTES", "2500")
	if err := db.GetDB().Create(&model.RelayLog{ID: 9101, Time: time.Now().Unix()}).Error; err != nil {
		t.Fatal(err)
	}
	// 刚写盘、日志还没落库的图（无引用、不到一分钟）要排在有引用的旧图后面删。
	refOldest := putImage(t, "ref-oldest-2", 1000, 4*time.Hour)
	refNewer := putImage(t, "ref-newer-2", 1000, 3*time.Hour)
	justWritten := putImage(t, "just-written-2", 1000, 0)
	for _, im := range []testImage{refOldest, refNewer} {
		db.GetDB().Create(&model.RelayLogImage{LogID: 9101, Hash: im.hash})
	}
	n, err := RelayLogImageSweep(ctx, RelayLogImageOrphanGrace)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if exists(refOldest.path) {
		t.Fatal("oldest referenced image should be evicted first")
	}
	if !exists(justWritten.path) || !exists(refNewer.path) {
		t.Fatal("just-written image and newer referenced image must stay")
	}
}
