package dialgo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// Builds a Client wired against an httptest server with a fresh bearer token.
// Returns the client and a teardown.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	c := NewClient(nil, zerolog.Nop())
	c.InternalAPIBaseURL = server.URL + "/api"
	c.SetBearerToken("test-token")
	return c, server.Close
}

type sampleResponse struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestGetJSON_DecodesStruct(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q; want GET", r.Method)
		}
		if r.URL.Path != "/api/widget/42" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(sampleResponse{Name: "ada", Age: 36})
	})
	defer teardown()

	got, err := getJSON[*sampleResponse](context.Background(), c, "/widget/42", nil, "get widget")
	if err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if got == nil || got.Name != "ada" || got.Age != 36 {
		t.Errorf("got = %+v; want {ada 36}", got)
	}
}

func TestGetJSON_DecodesSlice(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"a","age":1},{"name":"b","age":2}]`))
	})
	defer teardown()

	got, err := getJSON[[]sampleResponse](context.Background(), c, "/things", nil, "get things")
	if err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Age != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestGetJSON_AppendsQueryParams(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "messages" {
			t.Errorf("filter = %q", r.URL.Query().Get("filter"))
		}
		if r.URL.Query().Get("limit") != "100" {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		_, _ = w.Write([]byte(`{}`))
	})
	defer teardown()

	_, err := getJSON[*sampleResponse](context.Background(), c, "/contact/", url.Values{
		"filter": {"messages"},
		"limit":  {"100"},
	}, "get conversations")
	if err != nil {
		t.Fatalf("getJSON returned error: %v", err)
	}
}

func TestPostJSON_RoundTripsBody(t *testing.T) {
	var captured sampleResponse
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q; want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(sampleResponse{Name: "echo:" + captured.Name, Age: captured.Age + 1})
	})
	defer teardown()

	got, err := postJSON[*sampleResponse](context.Background(), c, "/thing/", nil, sampleResponse{Name: "ada", Age: 1}, "create thing")
	if err != nil {
		t.Fatalf("postJSON returned error: %v", err)
	}
	if captured.Name != "ada" || captured.Age != 1 {
		t.Errorf("server saw body = %+v; want {ada 1}", captured)
	}
	if got.Name != "echo:ada" || got.Age != 2 {
		t.Errorf("response = %+v; want {echo:ada 2}", got)
	}
}

func TestPatchJSON_RoundTripsBody(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %q; want PATCH", r.Method)
		}
		w.WriteHeader(http.StatusOK) // empty body — exercise the "no response" path
	})
	defer teardown()

	_, err := patchJSON[json.RawMessage](context.Background(), c, "/thing/abc", nil, map[string]any{"k": "v"}, "patch thing")
	if err != nil {
		t.Fatalf("patchJSON returned error: %v", err)
	}
}

// 401 must propagate as *APIError AND fire OnAuthError. The bridge's silent
// refresh path depends on this firing — losing it would mean tokens never
// rotate automatically.
func TestDoJSON_Surfaces401AndFiresOnAuthError(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	defer teardown()

	var fired atomic.Bool
	c.OnAuthError = func(_ *APIError) { fired.Store(true) }

	_, err := getJSON[*sampleResponse](context.Background(), c, "/whatever", nil, "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", apiErr.StatusCode)
	}
	if !fired.Load() {
		t.Error("OnAuthError did not fire on 401")
	}
}

func TestDoJSON_NotLoggedIn(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached when not logged in")
	})
	defer teardown()

	c.SetBearerToken("") // simulate logged out

	_, err := getJSON[*sampleResponse](context.Background(), c, "/x", nil, "x")
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v; want ErrNotLoggedIn", err)
	}
}

// Decode failure includes the response body preview in the error message so
// debugging is possible without re-running with a packet sniffer.
func TestDoJSON_DecodeFailureCarriesBodyPreview(t *testing.T) {
	c, teardown := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{this is not json`))
	})
	defer teardown()

	_, err := getJSON[*sampleResponse](context.Background(), c, "/x", nil, "fetch x")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "fetch x") {
		t.Errorf("error message missing opName: %v", err)
	}
	if !strings.Contains(err.Error(), "this is not json") {
		t.Errorf("error message missing body preview: %v", err)
	}
}
