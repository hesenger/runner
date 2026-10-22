package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, dp *DynamicProxy) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	dp.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hello", nil))
	return rec
}

func TestServesUnavailableBeforeFirstTarget(t *testing.T) {
	rec := get(t, New())

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After header")
	}
	if body := rec.Body.String(); !strings.Contains(body, "no version deployed yet") {
		t.Errorf("body %q, want it to mention no version deployed yet", body)
	}
}

func TestForwardsAfterUpdateTarget(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "backend "+r.URL.Path)
	}))
	defer backend.Close()

	dp := New()
	if err := dp.UpdateTarget(backend.Listener.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	rec := get(t, dp)

	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "backend /hello" {
		t.Errorf("body %q, want %q", body, "backend /hello")
	}
}
