package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordlessSessionOriginAndContentTypeBoundaries(t *testing.T) {
	s := testAdmin(t)
	for _, tc := range []struct {
		name, host, origin, site, contentType, body string
		status                                      int
	}{
		{"localhost", "localhost:8766", "http://localhost:8766", "same-origin", "application/json", "{}", 200},
		{"IPv4", "127.0.0.1:8766", "http://127.0.0.1:8766", "same-origin", "application/json; charset=utf-8", "{}", 200},
		{"IPv6", "[::1]:8766", "http://[::1]:8766", "same-origin", "application/json", "{}", 200},
		{"form", "localhost:8766", "", "", "application/x-www-form-urlencoded", "", 415},
		{"simple text", "localhost:8766", "", "", "text/plain", "{}", 415},
		{"missing type", "localhost:8766", "", "", "", "{}", 415},
		{"cross origin", "localhost:8766", "https://example.org", "", "application/json", "{}", 403},
		{"null origin", "localhost:8766", "null", "", "application/json", "{}", 403},
		{"metadata no origin", "localhost:8766", "", "cross-site", "application/json", "{}", 403},
		{"other local port", "localhost:8766", "", "same-site", "application/json", "{}", 403},
		{"rebound host", "example.org:8766", "", "", "application/json", "{}", 403},
		{"wrong port", "localhost:8765", "", "", "application/json", "{}", 403},
		{"invalid JSON", "localhost:8766", "", "", "application/json", "{", 400},
		{"extra JSON", "localhost:8766", "", "", "application/json", "{} {}", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+"/admin/v1/session", strings.NewReader(tc.body))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if tc.status != 200 && len(w.Result().Cookies()) != 0 {
				t.Fatal("rejected request created a session")
			}
		})
	}
}

func TestPasswordlessCookieCanManageButNotCrossSite(t *testing.T) {
	s := testAdmin(t)
	w := callAdmin(s, "POST", "/session", "{}", "", "http://localhost:8766")
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != "/admin/v1" || cookies[0].MaxAge != 28800 {
		t.Fatal("missing restricted automatic session")
	}
	for _, site := range []string{"cross-site", "same-origin"} {
		r := httptest.NewRequest("POST", "http://localhost:8766/admin/v1/proxy/pause", strings.NewReader(`{"paused":true}`))
		r.AddCookie(cookies[0])
		r.Header.Set("Sec-Fetch-Site", site)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if site == "cross-site" {
			if w.Code != http.StatusForbidden || s.Gateway.UserPaused() {
				t.Fatal("cross-site cookie request performed management")
			}
		} else if w.Code != http.StatusOK || !s.Gateway.UserPaused() {
			t.Fatal("automatic cookie cannot manage device")
		}
	}
}
