package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestEmbeddedIndexPresent(t *testing.T) {
	f, err := distFS.Open("dist/index.html")
	if err != nil {
		t.Fatalf("failed to open embedded dist/index.html: %v", err)
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("failed to read embedded index.html: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("embedded index.html is empty")
	}
	lower := strings.ToLower(string(content))
	if !strings.Contains(lower, "<html") && !strings.Contains(lower, "<!doctype") {
		t.Errorf("embedded index.html does not look like valid HTML: %s", string(content))
	}
}

func TestClientRouteFallback(t *testing.T) {
	r := gin.New()
	r.Use(Handler())

	req := httptest.NewRequest(http.MethodGet, "/dashboard/leases/active", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK for SPA fallback route, got %d", w.Code)
	}

	body := strings.ToLower(w.Body.String())
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!doctype") {
		t.Errorf("expected fallback to return index.html content, got: %s", w.Body.String())
	}
}

func TestDirectoryTraversal_StaysInsideDist(t *testing.T) {
	r := gin.New()
	r.Use(Handler())

	traversalPaths := []string{
		"/../../etc/passwd",
		"/assets/../../go.mod",
		"/..%2F..%2F..%2F.env",
	}

	for _, p := range traversalPaths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// After the cache-header fix, paths containing a dot (e.g. go.mod, .env)
		// that do not exist in the embed return 404 — a strictly better outcome
		// than a 200 index.html fallback. Both 200 and 404 are acceptable here;
		// the invariant is that no repository file content must leak.
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Errorf("path %q: expected 200 or 404, got %d", p, w.Code)
		}
		if w.Code == http.StatusOK {
			body := w.Body.String()
			if strings.Contains(body, "module github.com/pg-cashflow/pg-go") {
				t.Errorf("path %q leaked repository go.mod!", p)
			}
			if strings.Contains(body, "DATABASE_URL=") {
				t.Errorf("path %q leaked .env file!", p)
			}
		}
	}
}

func TestAPIRouteFallback_Returns404JSON(t *testing.T) {
	r := gin.New()
	r.Use(Handler())

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/owner/floors/bulk"},
		{http.MethodGet, "/api/unknown"},
		{http.MethodPost, "/non-api-post"},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: expected 404, got %d", c.method, c.path, w.Code)
		}
		contentType := w.Header().Get("Content-Type")
		if !strings.Contains(contentType, "application/json") {
			t.Errorf("%s %s: expected JSON content-type, got %s", c.method, c.path, contentType)
		}
	}
}

