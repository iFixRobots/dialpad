package dialgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestSanitizeMMSFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "upload"},
		{"image.jpg", "image.jpg"},
		{"Beeper Nightly 2026-05-15 21.33.12.png", "Beeper_Nightly_2026-05-15_21.33.12.png"},
		{"my photo (1).png", "my_photo__1_.png"},
		{"weird/path\\name.jpg", "weird_path_name.jpg"},
		{"emoji_🎉.png", "emoji__.png"},
		{"___", "upload"},
		{".hidden.jpg", "hidden.jpg"},
		{"plain", "plain"},
	}
	for _, tc := range cases {
		got := sanitizeMMSFilename(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeMMSFilename(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestUploadFile_MultipartPayload locks in the exact shape of the request
// Dialpad's /api/upload_file/ endpoint receives: HTTP method, Authorization
// header, X-Requested-With marker, multipart fields "file" and "file_type",
// and sanitized filename in the file field's Content-Disposition. If a future
// edit accidentally drops one of these (as the original UploadFile shipped
// without X-Requested-With), this test fails.
func TestUploadFile_MultipartPayload(t *testing.T) {
	var captured struct {
		method     string
		path       string
		auth       string
		xrw        string
		fileType   string
		fileName   string
		fileBody   []byte
		filePartCT string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.xrw = r.Header.Get("X-Requested-With")

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("expected multipart content-type, got %q", r.Header.Get("Content-Type"))
			http.Error(w, "bad content-type", http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read multipart: %v", err)
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "file":
				captured.fileName = part.FileName()
				captured.fileBody = body
				captured.filePartCT = part.Header.Get("Content-Type")
			case "file_type":
				captured.fileType = string(body)
			}
		}
		_ = json.NewEncoder(w).Encode(UploadFileResponse{
			ContentType: "image/jpeg",
			Filename:    captured.fileName,
			GCSFilename: "/test-bucket/2026-05-15/abc",
			UUID:        "abc-uuid",
		})
	}))
	defer server.Close()

	client := NewClient(nil, zerolog.Nop())
	client.InternalAPIBaseURL = server.URL + "/api"
	client.SetBearerToken("test-token")

	data := []byte("fake-image-bytes")
	resp, err := client.UploadFile(context.Background(), data, "My Test File.png", "image/png")
	if err != nil {
		t.Fatalf("UploadFile returned error: %v", err)
	}
	if resp.UUID != "abc-uuid" {
		t.Errorf("UUID = %q; want abc-uuid", resp.UUID)
	}

	if captured.method != "POST" {
		t.Errorf("method = %q; want POST", captured.method)
	}
	if captured.path != "/api/upload_file/" {
		t.Errorf("path = %q; want /api/upload_file/", captured.path)
	}
	if captured.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q; want %q", captured.auth, "Bearer test-token")
	}
	if captured.xrw != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q; want XMLHttpRequest", captured.xrw)
	}
	if captured.fileType != "MMS" {
		t.Errorf("file_type field = %q; want MMS", captured.fileType)
	}
	if !bytes.Equal(captured.fileBody, data) {
		t.Errorf("file body length = %d; want %d", len(captured.fileBody), len(data))
	}
	if captured.fileName != "My_Test_File.png" {
		t.Errorf("filename in form = %q; want My_Test_File.png (sanitized)", captured.fileName)
	}
	// Critical: per-part Content-Type must match the actual MIME type. Dialpad
	// rejects with HTTP 400 body "null" when the file part declares
	// application/octet-stream (which multipart.CreateFormFile would emit by
	// default). This assertion locks that fix in.
	if captured.filePartCT != "image/png" {
		t.Errorf("file part Content-Type = %q; want image/png (NOT octet-stream — Dialpad rejects that)", captured.filePartCT)
	}
}

// TestUploadFile_SurfacesServerErrorBody confirms that when Dialpad returns a
// non-200 status, the APIError carries the response body so logs are useful.
// The original failure mode was an opaque "HTTP 400: null" — this test
// guarantees future code keeps capturing the body string.
func TestUploadFile_SurfacesServerErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()

	client := NewClient(nil, zerolog.Nop())
	client.InternalAPIBaseURL = server.URL + "/api"
	client.SetBearerToken("test-token")

	_, err := client.UploadFile(context.Background(), []byte("x"), "x.jpg", "image/jpeg")
	if err == nil {
		t.Fatal("expected error from 400 response, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", apiErr.StatusCode)
	}
	if apiErr.Body != "null" {
		t.Errorf("body = %q; want %q (no truncation)", apiErr.Body, "null")
	}
}
