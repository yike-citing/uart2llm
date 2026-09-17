package admin

import (
	"strings"
	"testing"
)

func TestMetricsAndPauseAuthenticationAndValidation(t *testing.T) {
	s := testAdmin(t)
	if w := callAdmin(s, "GET", "/metrics", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{}`, `{"paused":null}`, `{"paused":"true"}`, `{"paused":true,"extra":1}`} {
		if w := callAdmin(s, "POST", "/proxy/pause", body, "admin-secret", ""); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w := callAdmin(s, "POST", "/proxy/pause", `{"paused":true}`, "admin-secret", ""); w.Code != 200 || !s.Gateway.UserPaused() {
		t.Fatal(w.Body.String())
	}
	if w := callAdmin(s, "GET", "/metrics", "", "admin-secret", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"paused":true`) {
		t.Fatal(w.Body.String())
	}
	if w := callAdmin(s, "POST", "/proxy/pause", `{"paused":false}`, "admin-secret", ""); w.Code != 200 || s.Gateway.UserPaused() {
		t.Fatal(w.Body.String())
	}
}
