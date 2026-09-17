package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixtureSentinelRequiresAuthentication(t *testing.T) {
	f := &fixture{token: "test"}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r.Header.Set("Authorization", "Bearer test")
	w = httptest.NewRecorder()
	f.ServeHTTP(w, r)
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || v["uart2llm_fixture"].(map[string]any)["no_external_calls"] != true {
		t.Fatal("missing sentinel")
	}
}
func TestFixtureStreamingEventsAreValidAndComplete(t *testing.T) {
	f := &fixture{token: "test"}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"uart2llm-acceptance-fixture-v1","stream":true}`))
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	scan := bufio.NewScanner(w.Body)
	events := 0
	done := false
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatal(err)
		}
		events++
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if events != 4 || !done {
		t.Fatalf("events=%d done=%v", events, done)
	}
}
func TestFixturePatternDownloadAndMultipartHash(t *testing.T) {
	f := &fixture{token: "test"}
	r := httptest.NewRequest("GET", "/v1/files/fixture-pattern/content?size=65539", nil)
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	payload := w.Body.Bytes()
	if len(payload) != 65539 {
		t.Fatal(len(payload))
	}
	for i, b := range payload {
		if b != byte(i%251) {
			t.Fatalf("pattern mismatch at %d", i)
		}
	}
	expected := sha256.Sum256(payload)
	var upload bytes.Buffer
	mw := multipart.NewWriter(&upload)
	part, err := mw.CreateFormFile("file", "test.bin")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(payload)
	mw.WriteField("purpose", "fine-tune")
	mw.Close()
	r = httptest.NewRequest("POST", "/v1/files", &upload)
	r.Header.Set("Authorization", "Bearer test")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w = httptest.NewRecorder()
	f.ServeHTTP(w, r)
	var v struct {
		Bytes int    `json:"bytes"`
		Hash  string `json:"sha256"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || v.Bytes != len(payload) || v.Hash != hex.EncodeToString(expected[:]) {
		t.Fatal(w.Code, w.Body.String())
	}
}
