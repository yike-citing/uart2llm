package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUsageObserverConcurrentCloseDoesNotMutateReaderBuffers(t *testing.T) {
	b := &usageBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 256*1024))), o: &observation{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p := make([]byte, 1)
		for {
			if _, err := b.Read(p); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 10000; i++ {
		_ = b.Close()
	}
	<-done
}

func TestUsageObserverPreservesFragmentedStreamAndCountsLastUsage(t *testing.T) {
	wire := "data: {\"model\":\"real-model\",\"choices\":[{\"delta\":{\"content\":\"金额€\"}}]}\r\n\r\n" +
		"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: {\n" + "data: \"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	g := &Gateway{}
	o := g.metrics.begin(true)
	b := &usageBody{ReadCloser: io.NopCloser(strings.NewReader(wire)), o: o, sse: true}
	var got bytes.Buffer
	p := make([]byte, 1)
	for {
		n, err := b.Read(p)
		got.Write(p[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != wire {
		t.Fatal("stream changed")
	}
	o.status = 200
	g.metrics.finish(o, false)
	s := g.Statistics()
	if s.TotalTokens != 8 || s.PromptTokens != 5 || s.CompletionTokens != 3 || s.CachedTokens != 2 || s.UsageReported != 1 || s.LastModel != "real-model" {
		t.Fatalf("usage mismatch: %+v", s)
	}
}
func TestUsageObservationBoundsAndUnknownUsage(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		sse        bool
		want       bool
	}{
		{"large-json", `{"content":"` + strings.Repeat("x", 300000) + `","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, false, false},
		{"large-event-recovery", "data: " + strings.Repeat("x", 100000) + "\n\ndata: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n", true, true},
		{"unknown", `{"usage":{"vendor_field":123}}`, false, false},
		{"negative", `{"usage":{"prompt_tokens":1,"completion_tokens":-2,"total_tokens":3}}`, false, false},
		{"null", `{"usage":{"prompt_tokens":null,"completion_tokens":2,"total_tokens":3}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &observation{}
			b := &usageBody{ReadCloser: io.NopCloser(strings.NewReader(tc.wire)), o: o, sse: tc.sse}
			got, e := io.ReadAll(b)
			if e != nil || string(got) != tc.wire {
				t.Fatal("body changed")
			}
			if (o.usage != nil) != tc.want {
				t.Fatal("invalid usage coverage")
			}
			if len(b.buffer) > 256*1024 || len(b.event) > 65536 {
				t.Fatal("unbounded observation")
			}
		})
	}
}
func TestGatewayMetricsAndPauseDoNotInterruptExistingStream(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	g, proxy, _ := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"model\"}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	response, e := http.DefaultClient.Do(request("POST", proxy.URL+"/v1/chat/completions", strings.NewReader("fixture")))
	if e != nil {
		t.Fatal(e)
	}
	<-entered
	g.SetUserPaused(true)
	r, e := http.DefaultClient.Do(request("GET", proxy.URL+"/v1/models", nil))
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatal("paused request admitted")
	}
	close(release)
	data, e := io.ReadAll(response.Body)
	response.Body.Close()
	if e != nil || !strings.Contains(string(data), "[DONE]") {
		t.Fatal("pause interrupted existing stream")
	}
	deadline := time.Now().Add(time.Second)
	for g.Statistics().Completed < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s := g.Statistics()
	if s.Succeeded != 1 || s.Rejected != 1 || s.TotalTokens != 5 || s.RequestBytes != 7 || s.ResponseBytes == 0 {
		t.Fatalf("metrics: %+v", s)
	}
	g.SetUserPaused(false)
	if g.UserPaused() {
		t.Fatal("resume failed")
	}
}
