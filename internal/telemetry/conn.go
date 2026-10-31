package telemetry

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// 入站请求头「原始顺序 + 原始大小写」记录器。
//
// Go 的 net/http 把请求头解析进 map，顺序和大小写都丢了；而这两样恰恰是区分客户端
// （Node / Python / Rust / Go 的 HTTP 库、各家 CLI）最有用的特征。所以在**入站** listener
// 上包一层：conn.Read 读到什么原样交给 http.Server（一个字节都不改、不多读、不少读），
// 同时旁路扫一遍字节流，把每个请求的头名按出现顺序记下来。
//
// 只看入站连接，跟出站（relay → 上游）没有任何关系：出站走 internal/client 自己的
// 连接，这里既不 import 也不碰它。
//
// 解析器只为「认出头名」服务，任何看不懂的情况（TLS 字节、HTTP/2 前言、坏请求、
// Upgrade 之后的 WebSocket 帧、队列堆积）都直接停掉这条连接上的记录，退回
// 「顺序未知」，绝不影响请求本身。

const (
	maxLineBytes    = 8 << 10 // 单行只留这么多（够放头名和 Content-Length）；超了照样往后扫
	maxNamesPerReq  = 128
	maxNameBytes    = 64
	maxQueuedBlocks = 8
)

type parseState uint8

const (
	stLine parseState = iota
	stHeaders
	stBody
	stChunkSize
	stChunkData
	stChunkCRLF
	stTrailers
	stStopped
)

type headerBlock struct {
	method string
	target string
	proto  string
	names  []string
}

// recorder 是一条入站连接上的旁路解析器。feed 只在读协程里调（http.Server 的读循环 /
// 后台探测读，它们之间由 net/http 自己串行化，但为了稳妥仍加锁）；next 在处理请求的协程里调。
type recorder struct {
	mu      sync.Mutex
	state   parseState
	line    []byte
	lineCut bool
	remain  int64

	cur        headerBlock
	curCL      int64
	curCLSet   bool
	curChunked bool
	curTEOther bool
	curUpgrade bool
	curConnUpg bool
	curBad     bool

	queue []headerBlock
}

func newRecorder() *recorder { return &recorder{curCL: -1} }

func (r *recorder) stop() {
	r.state = stStopped
	r.queue = nil
	r.line = nil
}

func (r *recorder) feed(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		// 旁路解析绝不能把请求带崩。
		if recover() != nil {
			r.stop()
		}
	}()
	for len(p) > 0 && r.state != stStopped {
		switch r.state {
		case stBody, stChunkData:
			n := int64(len(p))
			if n > r.remain {
				n = r.remain
			}
			r.remain -= n
			p = p[n:]
			if r.remain == 0 {
				if r.state == stBody {
					r.state = stLine
				} else {
					r.state = stChunkCRLF
				}
			}
		default:
			i := bytes.IndexByte(p, '\n')
			chunk := p
			if i >= 0 {
				chunk = p[:i]
			}
			if !r.lineCut {
				room := maxLineBytes - len(r.line)
				if len(chunk) > room {
					r.line = append(r.line, chunk[:room]...)
					r.lineCut = true
				} else {
					r.line = append(r.line, chunk...)
				}
			}
			if i < 0 {
				return
			}
			p = p[i+1:]
			line := bytes.TrimSuffix(r.line, []byte{'\r'})
			r.onLine(line)
			r.line = r.line[:0]
			r.lineCut = false
		}
	}
}

func (r *recorder) onLine(line []byte) {
	switch r.state {
	case stLine:
		if len(line) == 0 {
			return // 请求前的空行按 RFC 9112 忽略
		}
		parts := strings.SplitN(string(line), " ", 3)
		if len(parts) != 3 || !validMethod(parts[0]) || !strings.HasPrefix(parts[2], "HTTP/1.") {
			r.stop() // TLS 握手字节 / HTTP/2 前言 / 坏请求：不认
			return
		}
		r.cur = headerBlock{method: parts[0], target: parts[1], proto: parts[2]}
		r.curCL, r.curCLSet, r.curChunked, r.curTEOther, r.curUpgrade, r.curConnUpg, r.curBad = -1, false, false, false, false, false, false
		r.state = stHeaders
	case stHeaders:
		if len(line) == 0 {
			r.endHeaders()
			return
		}
		if line[0] == ' ' || line[0] == '\t' {
			return // obs-fold 续行
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			r.stop()
			return
		}
		name := string(line[:colon])
		value := strings.TrimSpace(string(line[colon+1:]))
		if len(r.cur.names) < maxNamesPerReq {
			n := name
			if len(n) > maxNameBytes {
				n = n[:maxNameBytes]
			}
			r.cur.names = append(r.cur.names, n)
		}
		switch strings.ToLower(name) {
		case "content-length":
			v, err := strconv.ParseInt(value, 10, 64)
			if err != nil || v < 0 || (r.curCLSet && v != r.curCL) {
				r.curBad = true
			}
			r.curCL, r.curCLSet = v, true
		case "transfer-encoding":
			codings := strings.Split(strings.ToLower(value), ",")
			if strings.TrimSpace(codings[len(codings)-1]) == "chunked" {
				r.curChunked = true
			} else {
				r.curTEOther = true
			}
		case "upgrade":
			r.curUpgrade = true
		case "connection":
			if strings.Contains(strings.ToLower(value), "upgrade") {
				r.curConnUpg = true
			}
		}
	case stChunkSize:
		s := string(line)
		if i := strings.IndexByte(s, ';'); i >= 0 {
			s = s[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(s), 16, 64)
		if err != nil || size < 0 {
			r.stop()
			return
		}
		if size == 0 {
			r.state = stTrailers
			return
		}
		r.remain = size
		r.state = stChunkData
	case stChunkCRLF:
		if len(line) != 0 {
			r.stop()
			return
		}
		r.state = stChunkSize
	case stTrailers:
		if len(line) == 0 {
			r.state = stLine
		}
	}
}

func (r *recorder) endHeaders() {
	if len(r.queue) >= maxQueuedBlocks || r.curBad || r.curTEOther {
		r.stop()
		return
	}
	r.queue = append(r.queue, r.cur)
	switch {
	case r.cur.method == http.MethodConnect || (r.curUpgrade && r.curConnUpg):
		// 之后是隧道 / WebSocket 帧，不再是 HTTP/1 请求。
		r.state = stStopped
		r.line = nil
	case r.curChunked:
		r.state = stChunkSize
	case r.curCLSet && r.curCL > 0:
		r.remain = r.curCL
		r.state = stBody
	default:
		r.state = stLine
	}
}

// next 取出与这个请求对应的那一组头名。http.Server 在一条连接上按顺序一个一个处理请求，
// 所以队首就是当前请求；方法 / 目标对不上说明解析器跟丢了，停掉这条连接上的记录。
func (r *recorder) next(method, target string) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return nil, false
	}
	b := r.queue[0]
	r.queue = r.queue[1:]
	if b.method != method || b.target != target {
		r.stop()
		return nil, false
	}
	return b.names, true
}

func validMethod(m string) bool {
	if m == "" || len(m) > 16 {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// Listener 包住入站 listener，给每条连接挂一个旁路记录器。
type Listener struct{ net.Listener }

func (l Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &recConn{Conn: c, rec: newRecorder()}, nil
}

type recConn struct {
	net.Conn
	rec *recorder
}

func (c *recConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.rec.feed(p[:n])
	}
	return n, err
}

// CloseWrite / ReadFrom 透传，http.Server 的优雅关闭和 sendfile 路径行为不变。
func (c *recConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *recConn) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, src)
}

type recorderKey struct{}
type orderKey struct{}

// ConnContext 挂到 http.Server.ConnContext：把这条连接的记录器放进 context。
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if rc, ok := c.(*recConn); ok {
		return context.WithValue(ctx, recorderKey{}, rc.rec)
	}
	return ctx
}

type headerOrder struct {
	names []string
	known bool
}

// Handler 在每个请求进 gin 之前取出它的原始头名顺序，放进请求 context。
// 每个请求都要取（包括静态页、管理接口），否则同一条长连接上后面的请求会对不上。
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if rec, ok := req.Context().Value(recorderKey{}).(*recorder); ok && req.ProtoMajor == 1 {
			names, known := rec.next(req.Method, req.RequestURI)
			if known {
				req = req.WithContext(context.WithValue(req.Context(), orderKey{}, headerOrder{names: names, known: true}))
			}
		}
		next.ServeHTTP(w, req)
	})
}

// RawHeaderNames 返回这个请求的头名（原始顺序、原始大小写）；拿不到时 ok=false。
func RawHeaderNames(req *http.Request) ([]string, bool) {
	if req == nil {
		return nil, false
	}
	o, ok := req.Context().Value(orderKey{}).(headerOrder)
	if !ok || !o.known {
		return nil, false
	}
	return o.names, true
}
