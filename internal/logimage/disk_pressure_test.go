package logimage

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zhizhishu/octopus/internal/utils/diskspace"
)

func TestImageDiskPressureRejectsWithoutQueueingAndRecovers(t *testing.T) {
	WaitIdle()
	dir := useTempDir(t)
	old := imageDiskAvailable
	var free atomic.Int64
	free.Store(1)
	imageDiskAvailable = func(string) (int64, bool) { return free.Load(), true }
	t.Cleanup(func() { WaitIdle(); imageDiskAvailable = old })
	data := testPNG("pressure")
	hash := hashOf(data)
	before := queuedBytes.Load()
	if Enqueue(hash, "png", data) {
		t.Fatal("low disk admitted an image")
	}
	if queuedBytes.Load() != before {
		t.Fatal("rejected image leaked queue reservation")
	}
	if _, _, found := Find(hash); found {
		t.Fatal("rejected image was written")
	}
	free.Store(diskspace.LogReserveBytes + int64(len(data)))
	if !Enqueue(hash, "png", data) {
		t.Fatal("exact reserve boundary rejected")
	}
	WaitIdle()
	if _, _, found := Find(hash); !found {
		t.Fatal("recovered image absent")
	}
	free.Store(0)
	if !Enqueue(hash, "png", data) {
		t.Fatal("existing image dedupe must not need new disk space")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected files: %v %v", files, err)
	}
}

func TestImageWriterRechecksPressureAfterAdmission(t *testing.T) {
	WaitIdle()
	root := useTempDir(t)
	old := imageDiskAvailable
	imageDiskAvailable = func(string) (int64, bool) { return 0, true }
	t.Cleanup(func() { WaitIdle(); imageDiskAvailable = old })
	data := testPNG("late-pressure")
	hash := hashOf(data)
	dir := filepath.Join(root, "not-created")
	err := writeFile(job{dir: dir, hash: hash, ext: "png", data: data})
	if !errors.Is(err, errImageDiskPressure) {
		t.Fatalf("expected pressure error: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("refusal created files: %v", err)
	}
	imageDiskAvailable = func(string) (int64, bool) { return 0, false }
	if err := writeFile(job{dir: dir, hash: hash, ext: "png", data: data}); err != nil {
		t.Fatalf("unknown availability changed legacy writes: %v", err)
	}
}

func TestImageDiskReserveIncludesAcceptedQueue(t *testing.T) {
	WaitIdle()
	old := imageDiskAvailable
	t.Cleanup(func() { imageDiskAvailable = old })
	imageDiskAvailable = func(string) (int64, bool) { return diskspace.LogReserveBytes + 100, true }
	if !imageDiskHasSpace(t.TempDir(), 100) {
		t.Fatal("exact boundary rejected")
	}
	if imageDiskHasSpace(t.TempDir(), 101) {
		t.Fatal("accepted queue not budgeted")
	}
}
