package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Statistics are in-memory counters for this daemon run. Usage is reported by
// the provider; missing/oversize/compressed usage is never estimated as zero.
type Statistics struct {
	Since              time.Time `json:"since"`
	Requests           uint64    `json:"requests"`
	Completed          uint64    `json:"completed"`
	Succeeded          uint64    `json:"succeeded"`
	Failed             uint64    `json:"failed"`
	Cancelled          uint64    `json:"cancelled"`
	Rejected           uint64    `json:"rejected"`
	ChatRequests       uint64    `json:"chat_requests"`
	UsageReported      uint64    `json:"usage_reported"`
	UsageMissing       uint64    `json:"usage_missing"`
	PromptTokens       uint64    `json:"prompt_tokens"`
	CompletionTokens   uint64    `json:"completion_tokens"`
	TotalTokens        uint64    `json:"total_tokens"`
	CachedTokens       uint64    `json:"cached_tokens"`
	RequestBytes       uint64    `json:"request_bytes"`
	ResponseBytes      uint64    `json:"response_bytes"`
	AverageDurationMS  float64   `json:"average_duration_ms"`
	AverageFirstByteMS float64   `json:"average_first_byte_ms"`
	RecentRequests     uint64    `json:"requests_last_60s"`
	LastStatus         int       `json:"last_status"`
	LastModel          string    `json:"last_model"`
	Active             int32     `json:"active"`
	Paused             bool      `json:"paused"`
}

type statistics struct {
	mu                  sync.Mutex
	value               Statistics
	duration, firstByte time.Duration
	firstByteCount      uint64
	buckets             [60]struct {
		second int64
		count  uint64
	}
}

type observation struct {
	start         time.Time
	chat          bool
	status        int
	firstByte     time.Duration
	requestBytes  atomic.Uint64
	responseBytes uint64
	rejected      bool
	model         string
	usage         *usage
}
type usage struct {
	Prompt     uint64 `json:"prompt_tokens"`
	Completion uint64 `json:"completion_tokens"`
	Total      uint64 `json:"total_tokens"`
	CacheHit   uint64 `json:"prompt_cache_hit_tokens"`
	Details    struct {
		Cached uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (s *statistics) begin(chat bool) *observation {
	now := time.Now()
	s.mu.Lock()
	if s.value.Since.IsZero() {
		s.value.Since = now
	}
	s.value.Requests++
	if chat {
		s.value.ChatRequests++
	}
	b := &s.buckets[now.Unix()%60]
	if b.second != now.Unix() {
		b.second, b.count = now.Unix(), 0
	}
	b.count++
	s.mu.Unlock()
	return &observation{start: now, chat: chat}
}

func (s *statistics) finish(o *observation, aborted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := &s.value
	v.Completed++
	if o.status >= 200 && o.status < 400 && !aborted {
		v.Succeeded++
	} else {
		v.Failed++
	}
	if aborted {
		v.Cancelled++
	}
	if o.rejected {
		v.Rejected++
	}
	v.RequestBytes += o.requestBytes.Load()
	v.ResponseBytes += o.responseBytes
	v.LastStatus = o.status
	if o.model != "" {
		v.LastModel = o.model
	}
	s.duration += time.Since(o.start)
	if o.firstByte > 0 {
		s.firstByte += o.firstByte
		s.firstByteCount++
	}
	if o.chat {
		if o.usage == nil {
			v.UsageMissing++
		} else {
			v.UsageReported++
			v.PromptTokens += o.usage.Prompt
			v.CompletionTokens += o.usage.Completion
			v.TotalTokens += o.usage.Total
			cached := o.usage.CacheHit
			if cached == 0 {
				cached = o.usage.Details.Cached
			}
			v.CachedTokens += cached
		}
	}
}

func (g *Gateway) Statistics() Statistics {
	s := &g.metrics
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.value
	if v.Completed > 0 {
		v.AverageDurationMS = float64(s.duration) / float64(time.Millisecond) / float64(v.Completed)
	}
	if s.firstByteCount > 0 {
		v.AverageFirstByteMS = float64(s.firstByte) / float64(time.Millisecond) / float64(s.firstByteCount)
	}
	for _, b := range s.buckets {
		if age := time.Now().Unix() - b.second; age >= 0 && age < 60 {
			v.RecentRequests += b.count
		}
	}
	v.Active, v.Paused = g.Active(), g.UserPaused()
	return v
}

func StatisticsSchema() []map[string]any {
	t := reflect.TypeOf(Statistics{})
	out := make([]map[string]any, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("json")
		unit := "count"
		if strings.HasSuffix(name, "_ms") {
			unit = "ms"
		}
		if strings.HasSuffix(name, "_bytes") {
			unit = "bytes"
		}
		if strings.Contains(name, "tokens") {
			unit = "provider tokens"
		}
		if name == "since" {
			unit = "UTC"
		}
		if name == "paused" {
			unit = "boolean"
		}
		if name == "last_model" {
			unit = "text"
		}
		out = append(out, map[string]any{"name": "llm." + name, "source": "daemon", "unit": unit, "sample_time_field": "sampled_at", "valid_when": "daemon_running", "unavailable_reason": "usage absent, compressed or beyond observation limits; no estimate", "scope": "this daemon run; completed request counters; usage is provider-reported"})
	}
	return out
}

type observedWriter struct {
	http.ResponseWriter
	o *observation
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if status >= 200 && w.o.status == 0 {
		w.o.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(p []byte) (int, error) {
	if w.o.status == 0 {
		w.o.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.o.firstByte == 0 {
		w.o.firstByte = time.Since(w.o.start)
	}
	w.o.responseBytes += uint64(n)
	return n, err
}

type countedBody struct {
	io.ReadCloser
	o *observation
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.o.requestBytes.Add(uint64(n))
	return n, e
}

// Only metadata is decoded. Buffers have hard bounds and never affect bytes
// delivered to the client. Large JSON or SSE events simply lack usage metrics.
type usageBody struct {
	io.ReadCloser
	o                                *observation
	sse                              bool
	buffer                           []byte
	event                            []byte
	discard, eventDiscard, exhausted bool
}

func (b *usageBody) metadata(data []byte) {
	var m struct {
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	if len(m.Model) > 0 && len(m.Model) <= 128 && !strings.ContainsAny(m.Model, "\r\n\x00") {
		b.o.model = m.Model
	}
	var fields map[string]json.RawMessage
	var value usage
	if json.Unmarshal(m.Usage, &fields) == nil && fields["prompt_tokens"] != nil && fields["completion_tokens"] != nil && fields["total_tokens"] != nil &&
		string(fields["prompt_tokens"]) != "null" && string(fields["completion_tokens"]) != "null" && string(fields["total_tokens"]) != "null" && json.Unmarshal(m.Usage, &value) == nil {
		b.o.usage = &value // last cumulative usage, not sum of chunks
	}
}
func (b *usageBody) line(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		if !b.eventDiscard && len(b.event) > 0 {
			b.metadata(bytes.TrimSuffix(b.event, []byte{'\n'}))
		}
		b.event = b.event[:0]
		b.eventDiscard = false
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) && !b.eventDiscard {
		data := line[5:]
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		if len(b.event)+len(data)+1 > 65536 {
			b.eventDiscard = true
			b.event = b.event[:0]
			return
		}
		b.event = append(b.event, data...)
		b.event = append(b.event, '\n')
	}
}
func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.exhausted {
		if !b.sse {
			if len(b.buffer)+n <= 256*1024 {
				b.buffer = append(b.buffer, p[:n]...)
			} else {
				b.buffer = nil
				b.exhausted = true
			}
		} else {
			for _, v := range p[:n] {
				if v == '\n' {
					if b.discard {
						b.eventDiscard = true
					} else {
						b.line(b.buffer)
					}
					b.buffer = b.buffer[:0]
					b.discard = false
				} else if !b.discard {
					if len(b.buffer) == 65536 {
						b.buffer = b.buffer[:0]
						b.discard = true
					} else {
						b.buffer = append(b.buffer, v)
					}
				}
			}
		}
		if err == io.EOF && !b.sse {
			b.metadata(b.buffer)
		}
	}
	return n, err
}

// Close may run concurrently with Read during cancellation. Only the reader
// owns the bounded metadata buffers; let them be collected with this body.
func (b *usageBody) Close() error { return b.ReadCloser.Close() }
