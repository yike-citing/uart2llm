// test-upstream is a controlled HTTPS fixture for uart2llm acceptance testing.
// Run it on another LAN computer. It never contacts OpenAI or any other service.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const fixtureModel = "uart2llm-acceptance-fixture-v1"
const maxFileBytes = int64(1024 * 1024 * 1024)

type fixture struct {
	token      string
	eventDelay time.Duration
}

func main() {
	listen := flag.String("listen", ":9443", "HTTPS listen address on the networked LAN computer")
	cert := flag.String("cert", "", "PEM certificate chain; SAN must match the configured upstream hostname")
	key := flag.String("key", "", "PEM private key")
	tokenEnv := flag.String("token-env", "UART2LLM_FIXTURE_TOKEN", "environment variable containing the fixture bearer token")
	delay := flag.Duration("event-delay", 40*time.Millisecond, "pause between deterministic SSE events")
	flag.Parse()
	token := os.Getenv(*tokenEnv)
	if *cert == "" || *key == "" || token == "" || *delay < 0 || *delay > time.Minute {
		log.Fatal("provide --cert, --key, a nonempty fixture token environment variable, and event-delay between 0 and 1m")
	}
	server := &http.Server{Addr: *listen, Handler: &fixture{token: token, eventDelay: *delay}, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	log.Printf("controlled acceptance fixture listening on HTTPS %s; no external service calls", *listen)
	log.Fatal(server.ListenAndServeTLS(*cert, *key))
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": "fixture_error"}})
}
func consumeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid or oversized JSON")
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "one JSON value required")
		return false
	}
	return true
}
func model() map[string]any {
	return map[string]any{"id": fixtureModel, "object": "model", "created": 0, "owned_by": "uart2llm-controlled-fixture"}
}
func job(status string) map[string]any {
	return map[string]any{"id": "ftjob-fixture", "object": "fine_tuning.job", "model": fixtureModel, "status": status, "training_file": "file-fixture", "created_at": 0}
}
func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+f.token)) != 1 {
		fail(w, 401, "fixture bearer token required")
		return
	}
	w.Header().Set("X-Uart2llm-Fixture", "1")
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/v1/models":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{model()}, "uart2llm_fixture": map[string]any{"version": 1, "pattern": "offset-mod-251", "no_external_calls": true}})
	case r.Method == "GET" && path == "/v1/models/"+fixtureModel:
		writeJSON(w, 200, model())
	case r.Method == "POST" && path == "/v1/chat/completions":
		f.chat(w, r)
	case r.Method == "POST" && path == "/v1/files":
		f.upload(w, r)
	case r.Method == "GET" && path == "/v1/files/fixture-pattern/content":
		f.download(w, r)
	case r.Method == "GET" && path == "/v1/files":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "fixture-pattern", "object": "file", "bytes": 65536, "filename": "pattern.bin", "purpose": "fine-tune"}}, "has_more": false})
	case r.Method == "GET" && path == "/v1/files/fixture-pattern":
		writeJSON(w, 200, map[string]any{"id": "fixture-pattern", "object": "file", "bytes": 65536, "filename": "pattern.bin", "purpose": "fine-tune"})
	case r.Method == "DELETE" && strings.HasPrefix(path, "/v1/files/"):
		writeJSON(w, 200, map[string]any{"id": strings.TrimPrefix(path, "/v1/files/"), "object": "file", "deleted": true})
	case path == "/v1/fine_tuning/jobs" && r.Method == "POST":
		var input map[string]any
		if consumeJSON(w, r, &input) {
			writeJSON(w, 200, job("succeeded"))
		}
	case path == "/v1/fine_tuning/jobs" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{job("succeeded")}, "has_more": false})
	case path == "/v1/fine_tuning/jobs/ftjob-fixture" && r.Method == "GET":
		writeJSON(w, 200, job("succeeded"))
	case path == "/v1/fine_tuning/jobs/ftjob-fixture/events" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "event-fixture", "object": "fine_tuning.job.event", "level": "info", "message": "Controlled fixture event"}}, "has_more": false})
	case path == "/v1/fine_tuning/jobs/ftjob-fixture/checkpoints" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{}, "has_more": false})
	case r.Method == "POST" && (path == "/v1/fine_tuning/jobs/ftjob-fixture/cancel" || path == "/v1/fine_tuning/jobs/ftjob-fixture/pause" || path == "/v1/fine_tuning/jobs/ftjob-fixture/resume"):
		status := "running"
		if strings.HasSuffix(path, "/cancel") {
			status = "cancelled"
		}
		if strings.HasSuffix(path, "/pause") {
			status = "paused"
		}
		writeJSON(w, 200, job(status))
	default:
		fail(w, 404, "unknown controlled fixture endpoint")
	}
}
func (f *fixture) chat(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model         string          `json:"model"`
		Stream        bool            `json:"stream"`
		Messages      json.RawMessage `json:"messages"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	if !consumeJSON(w, r, &input) {
		return
	}
	if input.Model != fixtureModel {
		fail(w, 400, "use the controlled fixture model")
		return
	}
	if !input.Stream {
		writeJSON(w, 200, map[string]any{"id": "chat-fixture", "object": "chat.completion", "model": fixtureModel, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "你好，串口"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 5, "total_tokens": 9}})
		return
	}
	events := []string{
		`{"id":"chat-fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"你好，"}}]}`,
		`{"id":"chat-fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"串口","tool_calls":[{"index":0,"id":"call-fixture","type":"function","function":{"name":"echo","arguments":"{\"value\":\""}}]}}]}`,
		`{"id":"chat-fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"中文\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"chat-fixture","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9},"fixture_extension":{"preserved":true}}`,
		`[DONE]`,
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Request-ID", "fixture-stream")
	rc := http.NewResponseController(w)
	for _, event := range events {
		// Small chunks deliberately cross UTF-8 and JSON boundaries.
		data := []byte("data: " + event + "\n\n")
		for len(data) > 0 {
			n := 7
			if n > len(data) {
				n = len(data)
			}
			_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
			if _, err := w.Write(data[:n]); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			data = data[n:]
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(f.eventDelay):
		}
	}
}
func (f *fixture) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes+128*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "multipart/form-data required")
		return
	}
	hash := sha256.New()
	var count int64
	files := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, 400, "multipart input interrupted")
			return
		}
		if part.FormName() == "file" {
			files++
			n, err := io.Copy(hash, io.LimitReader(part, maxFileBytes+1))
			count += n
			if err != nil || count > maxFileBytes {
				fail(w, 400, "file too large or interrupted")
				return
			}
		} else {
			n, err := io.Copy(io.Discard, io.LimitReader(part, 65537))
			if err != nil || n > 65536 {
				fail(w, 400, "form field too large")
				return
			}
		}
		part.Close()
	}
	if files != 1 {
		fail(w, 400, "exactly one file part required")
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expected := r.Header.Get("X-Expected-Sha256"); expected != "" && expected != digest {
		fail(w, 422, "upload SHA-256 mismatch")
		return
	}
	writeJSON(w, 200, map[string]any{"id": "file-fixture", "object": "file", "bytes": count, "purpose": "fine-tune", "sha256": digest, "uart2llm_fixture": 1})
}
func (f *fixture) download(w http.ResponseWriter, r *http.Request) {
	size := int64(65536)
	if raw := r.URL.Query().Get("size"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 || n > maxFileBytes {
			fail(w, 400, "size must be 0..1073741824")
			return
		}
		size = n
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Fixture-Pattern", "offset-mod-251")
	buffer := make([]byte, 32768)
	offset := int64(0)
	rc := http.NewResponseController(w)
	for offset < size {
		n := int64(len(buffer))
		if size-offset < n {
			n = size - offset
		}
		for i := int64(0); i < n; i++ {
			buffer[i] = byte((offset + i) % 251)
		}
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))
		if _, err := w.Write(buffer[:n]); err != nil {
			return
		}
		offset += n
	}
}
