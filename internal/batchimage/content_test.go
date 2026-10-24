package batchimage

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 一行假结果：给定 custom_id 与图片字节，拼成上游会返回的 JSONL 行。
func fakeResultLine(key string, payload []byte) string {
	encoded := base64.StdEncoding.EncodeToString(payload)
	return `{"key":"` + key + `","response":{"candidates":[{"content":{"parts":[` +
		`{"inlineData":{"mimeType":"image/png","data":"` + encoded + `"}}]}}]}}` + "\n"
}

// fakeProvider 只实现 OpenResult，返回一段固定 JSONL。
type fakeProvider struct {
	body string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Submit(context.Context, SubmitRequest) (*SubmitResult, error) {
	return nil, nil
}

func (f *fakeProvider) Poll(context.Context, PollRequest) (*PollResult, error) { return nil, nil }

func (f *fakeProvider) Cancel(context.Context, CancelRequest) error { return nil }

func (f *fakeProvider) OpenResult(context.Context, ResultRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func (f *fakeProvider) Cleanup(context.Context, CleanupRequest) error { return nil }

func fakeResolver(body string) ContentResolver {
	return func(context.Context, *model.BatchImageJob) (Provider, ChannelContext, error) {
		return &fakeProvider{body: body}, ChannelContext{}, nil
	}
}

func testJob() *model.BatchImageJob {
	return &model.BatchImageJob{BatchID: "imgbatch_test", Model: "m", ProviderOutputRef: "ref"}
}

// 行号兜底必须按**全部条目**的物理位置算，不能只按成功条目。
//
// 【这条盯的是一个真实的错位】结果流一行一条、按提交顺序，含失败行。
// 若把"成功条目"排成一个紧凑列表再去索引物理行号，只要前面有一行失败，
// 后面每一张图都会挂到别人名下 —— 用户拿到的是别人 prompt 生成的图，
// 而且我们的索引（按 key 匹配时是正确的）与打包下载（按行号兜底）会互相矛盾。
//
// 这里构造"第 1 条失败、第 2 条成功"，且**结果行不带 key**，强制走行号兜底。
func TestStreamAllImagesLineFallbackUsesAllItemsNotJustSuccesses(t *testing.T) {
	second := []byte("SECOND-ITEM-IMAGE")
	body := fakeResultLine("", []byte("FIRST-ITEM-IMAGE")) + fakeResultLine("", second)

	items := []model.BatchImageItem{
		{CustomID: "one", Status: model.BatchImageItemStatusFailed, SourceLineNumber: 1},
		{CustomID: "two", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 2},
	}

	var got []ItemImage
	err := StreamAllImages(context.Background(), fakeResolver(body), testJob(), items,
		func(image ItemImage) error {
			got = append(got, image)
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("emitted %d images, want 1 (only the successful item)", len(got))
	}
	if got[0].CustomID != "two" {
		t.Fatalf("image attributed to %q, want \"two\"", got[0].CustomID)
	}
	if string(got[0].Data) != string(second) {
		t.Fatalf("image bytes came from the wrong line: %q", string(got[0].Data))
	}
}

// key 匹配优先于行号：上游乱序时也必须归对人。
func TestStreamAllImagesMatchesByKeyWhenOutOfOrder(t *testing.T) {
	first := []byte("IMAGE-FOR-ALPHA")
	second := []byte("IMAGE-FOR-BETA")
	// 故意倒序返回。
	body := fakeResultLine("beta", second) + fakeResultLine("alpha", first)

	items := []model.BatchImageItem{
		{CustomID: "alpha", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 1},
		{CustomID: "beta", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 2},
	}

	seen := map[string]string{}
	err := StreamAllImages(context.Background(), fakeResolver(body), testJob(), items,
		func(image ItemImage) error {
			seen[image.CustomID] = string(image.Data)
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if seen["alpha"] != string(first) {
		t.Fatalf("alpha got %q, want %q", seen["alpha"], string(first))
	}
	if seen["beta"] != string(second) {
		t.Fatalf("beta got %q, want %q", seen["beta"], string(second))
	}
}

// 客户端断开（回调返回错误）必须立刻中断，且错误原样透出。
//
// 【为什么这条重要】不断的话，我们会把剩下几百 MB 从上游一路读进内存再丢弃，
// 而客户端早就不在了。
func TestStreamAllImagesAbortsOnCallbackError(t *testing.T) {
	body := fakeResultLine("a", []byte("A")) + fakeResultLine("b", []byte("B")) +
		fakeResultLine("c", []byte("C"))

	items := []model.BatchImageItem{
		{CustomID: "a", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 1},
		{CustomID: "b", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 2},
		{CustomID: "c", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 3},
	}

	sentinel := io.ErrClosedPipe
	calls := 0
	err := StreamAllImages(context.Background(), fakeResolver(body), testJob(), items,
		func(ItemImage) error {
			calls++
			return sentinel
		})
	if err == nil {
		t.Fatal("callback error was swallowed")
	}
	if err != sentinel {
		t.Fatalf("error = %v, want the callback's own error", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want 1 (the stream must stop immediately)", calls)
	}
}

// OpenItemImage：取单张图，找不到时给一个可判定的错误。
func TestOpenItemImage(t *testing.T) {
	body := fakeResultLine("alpha", []byte("ALPHA-BYTES"))

	reader, mimeType, err := OpenItemImage(context.Background(), fakeResolver(body), testJob(),
		&model.BatchImageItem{CustomID: "alpha", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ALPHA-BYTES" {
		t.Fatalf("bytes = %q", string(data))
	}
	if mimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", mimeType)
	}

	_, _, err = OpenItemImage(context.Background(), fakeResolver(body), testJob(),
		&model.BatchImageItem{CustomID: "nope", Status: model.BatchImageItemStatusSuccess, SourceLineNumber: 9})
	if err == nil {
		t.Fatal("missing item did not error")
	}
}
