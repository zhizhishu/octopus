// Package logimage 把「真实出站请求体」里的内联图片存成单独文件。
//
// 日志正文只留一个带 sha256 的占位符，原图按内容哈希寻址存在数据目录下：
//
//	data/log_images/<哈希前两位>/<sha256>.<png|jpg|gif|webp>
//
// 本包只管文件：识别类型、后台写盘、列目录、删文件。哪张图还被哪条日志
// 引用（relay_log_images 表）、什么时候该删，由 internal/op 决定。
// 它不依赖 op / relay，三边（relay 写、op 清、handlers 读）都能引用。
package logimage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhizhishu/octopus/internal/utils/log"
)

const (
	// MaxImageBytes 是单张图（解码后）的上限。
	MaxImageBytes = 10 << 20
	// MaxImagesPerRequest / MaxBytesPerRequest 是单个请求能存的张数与总量。
	MaxImagesPerRequest = 16
	MaxBytesPerRequest  = 40 << 20
	// DefaultMaxTotalBytes 是整个图片目录的容量上限，可用环境变量覆盖。
	DefaultMaxTotalBytes int64 = 2 << 30

	envDir      = "OCTOPUS_LOG_IMAGE_DIR"
	envMaxBytes = "OCTOPUS_LOG_IMAGE_MAX_BYTES"
	defaultDir  = "data/log_images"

	// 后台写盘队列：按张数和字节双重封顶。满了就放弃这张（调用方退回 omitted
	// 占位），绝不阻塞转发。
	queueSlots        = 64
	queueMaxBytes     = 128 << 20
	tmpPrefix         = ".tmp-"
	staleTmpThreshold = time.Hour
)

var (
	dirMu       sync.RWMutex
	dirOverride string

	queue        = make(chan job, queueSlots)
	queuedBytes  atomic.Int64
	workerOnce   sync.Once
	inflight     sync.WaitGroup
	pauseWorker  atomic.Bool // 只给测试用：让队列堆满
	pauseRelease = make(chan struct{}, 1)
)

type job struct {
	dir  string // 入队那一刻的目录，写盘时不再重读（测试会换目录）
	hash string
	ext  string
	data []byte
}

// Dir 返回图片目录（不保证已存在）。
func Dir() string {
	dirMu.RLock()
	d := dirOverride
	dirMu.RUnlock()
	if d != "" {
		return d
	}
	if v := strings.TrimSpace(os.Getenv(envDir)); v != "" {
		return v
	}
	return defaultDir
}

// SetDir 改写目录（测试用；生产走环境变量）。返回恢复函数。
func SetDir(dir string) func() {
	dirMu.Lock()
	prev := dirOverride
	dirOverride = dir
	dirMu.Unlock()
	return func() {
		dirMu.Lock()
		dirOverride = prev
		dirMu.Unlock()
	}
}

// MaxTotalBytes 是目录总容量上限；OCTOPUS_LOG_IMAGE_MAX_BYTES 可覆盖（正整数字节）。
func MaxTotalBytes() int64 {
	if v := strings.TrimSpace(os.Getenv(envMaxBytes)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxTotalBytes
}

// ValidHash 只接受 64 位小写十六进制。拼路径之前必须先过这一关，杜绝 ../ 穿越。
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Sniff 按文件头判断图片类型，只认 png / jpeg / gif / webp。
// 不信客户端声明的 mime：svg 之类能带脚本的类型一律不收。
func Sniff(b []byte) (mime, ext string, ok bool) {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", "png", true
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", "jpg", true
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return "image/gif", "gif", true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp", "webp", true
	}
	return "", "", false
}

var extMIME = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// exts 固定顺序，查找文件时按此逐个试。
var exts = []string{"png", "jpg", "gif", "webp"}

// MIMEForExt 返回扩展名对应的 Content-Type；未知返回空串。
func MIMEForExt(ext string) string { return extMIME[ext] }

func pathFor(dir, hash, ext string) string {
	return filepath.Join(dir, hash[:2], hash+"."+ext)
}

// Find 找到某个哈希的文件，返回路径与扩展名。hash 不合法或不存在时 ok=false。
func Find(hash string) (path, ext string, ok bool) {
	if !ValidHash(hash) {
		return "", "", false
	}
	dir := Dir()
	for _, e := range exts {
		p := pathFor(dir, hash, e)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, e, true
		}
	}
	return "", "", false
}

// Enqueue 把一张图交给后台写盘，不阻塞。
//
// 已存在同一哈希的文件：只刷新 mtime（让孤儿清扫知道它刚被用过），直接成功。
// 队列满或字节超限：返回 false，调用方应退回 omitted 占位。
// data 归队列所有，调用方之后不能再改它。
func Enqueue(hash, ext string, data []byte) bool {
	if !ValidHash(hash) || extMIME[ext] == "" || len(data) == 0 || len(data) > MaxImageBytes {
		return false
	}
	if p, _, ok := Find(hash); ok {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
		return true
	}
	n := int64(len(data))
	if queuedBytes.Add(n) > queueMaxBytes {
		queuedBytes.Add(-n)
		return false
	}
	workerOnce.Do(startWorker)
	inflight.Add(1)
	select {
	case queue <- job{dir: Dir(), hash: hash, ext: ext, data: data}:
		return true
	default:
		inflight.Done()
		queuedBytes.Add(-n)
		return false
	}
}

func startWorker() {
	go func() {
		for j := range queue {
			for pauseWorker.Load() {
				<-pauseRelease
			}
			if err := writeFile(j); err != nil {
				log.Warnf("log image write failed (%s): %v", j.hash[:12], err)
			}
			queuedBytes.Add(-int64(len(j.data)))
			inflight.Done()
		}
	}()
}

func writeFile(j job) error {
	dir := j.dir
	sub := filepath.Join(dir, j.hash[:2])
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return err
	}
	final := pathFor(dir, j.hash, j.ext)
	if _, err := os.Stat(final); err == nil {
		now := time.Now()
		_ = os.Chtimes(final, now, now)
		return nil
	}
	f, err := os.CreateTemp(sub, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(j.data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	// rename 是原子的：读者要么看到完整文件，要么看不到。并发写同一张图时
	// 后到的直接覆盖成同样的内容，无害。
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// WaitIdle 等后台队列写完（测试与优雅退出用）。
func WaitIdle() { inflight.Wait() }

// PauseWorkerForTest 让写盘协程停住，以便测试把队列塞满。返回恢复函数。
func PauseWorkerForTest() func() {
	pauseWorker.Store(true)
	return func() {
		pauseWorker.Store(false)
		select {
		case pauseRelease <- struct{}{}:
		default:
		}
	}
}

// File 是目录里的一张图。
type File struct {
	Hash    string
	Ext     string
	Path    string
	Size    int64
	ModTime time.Time
}

// List 列出目录里所有合法命名的图片文件；顺手删掉超过 1 小时的写盘残留临时文件。
// 目录不存在时返回空。
func List() ([]File, error) {
	dir := Dir()
	subs, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []File
	for _, sd := range subs {
		if !sd.IsDir() || len(sd.Name()) != 2 {
			continue
		}
		subPath := filepath.Join(dir, sd.Name())
		entries, err := os.ReadDir(subPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			name := e.Name()
			info, err := e.Info()
			if err != nil {
				continue
			}
			if strings.HasPrefix(name, tmpPrefix) {
				if time.Since(info.ModTime()) > staleTmpThreshold {
					_ = os.Remove(filepath.Join(subPath, name))
				}
				continue
			}
			dot := strings.LastIndexByte(name, '.')
			if dot < 0 {
				continue
			}
			hash, ext := name[:dot], name[dot+1:]
			if !ValidHash(hash) || hash[:2] != sd.Name() || extMIME[ext] == "" {
				continue
			}
			out = append(out, File{
				Hash:    hash,
				Ext:     ext,
				Path:    filepath.Join(subPath, name),
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
	}
	return out, nil
}

// Remove 删掉某个哈希的所有文件（不管扩展名）。不存在不算错。
func Remove(hash string) error {
	if !ValidHash(hash) {
		return nil
	}
	dir := Dir()
	var firstErr error
	for _, e := range exts {
		if err := os.Remove(pathFor(dir, hash, e)); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
