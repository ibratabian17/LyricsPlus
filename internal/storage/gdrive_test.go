package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
)

type mockDriveTransport struct {
	mu           sync.Mutex
	files        map[string]FileItem
	fileContents map[string][]byte
	patchCalls   int
	postCalls    int
	deleteCalls  int
}

func newMockDriveTransport() *mockDriveTransport {
	return &mockDriveTransport{
		files:        make(map[string]FileItem),
		fileContents: make(map[string][]byte),
	}
}

func (m *mockDriveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Handle OAuth token exchange
	if strings.Contains(req.URL.Host, "oauth2.googleapis.com") {
		respBody := `{"access_token":"mock-access-token","expires_in":3600,"token_type":"Bearer"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(respBody)),
			Header:     make(http.Header),
		}, nil
	}

	// Handle search: GET https://www.googleapis.com/drive/v3/files?...
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/drive/v3/files") && !strings.Contains(req.URL.RawQuery, "alt=media") {
		q := req.URL.Query().Get("q")
		var matched []FileItem
		for _, f := range m.files {
			if strings.Contains(q, "name = '"+escapeQueryLiteral(f.Name)+"'") || strings.Contains(q, f.Name) {
				matched = append(matched, f)
			}
		}
		respData, _ := json.Marshal(FileListResponse{Files: matched})
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(respData))),
			Header:     make(http.Header),
		}, nil
	}

	// Handle update (PATCH): https://www.googleapis.com/upload/drive/v3/files/{fileId}?uploadType=multipart
	if req.Method == http.MethodPatch && strings.Contains(req.URL.Path, "/upload/drive/v3/files/") {
		m.patchCalls++
		parts := strings.Split(req.URL.Path, "/")
		fileID := parts[len(parts)-1]

		bodyBytes, _ := io.ReadAll(req.Body)
		m.fileContents[fileID] = bodyBytes
		item := FileItem{
			ID:           fileID,
			Name:         "updated.json",
			MimeType:     "application/json",
			ModifiedTime: time.Now(),
		}
		m.files[fileID] = item
		respData, _ := json.Marshal(item)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(respData))),
			Header:     make(http.Header),
		}, nil
	}

	// Handle create (POST): https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart
	if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/upload/drive/v3/files") {
		m.postCalls++
		fileID := "new-file-id"
		bodyBytes, _ := io.ReadAll(req.Body)
		m.fileContents[fileID] = bodyBytes
		item := FileItem{
			ID:           fileID,
			Name:         "new.json",
			MimeType:     "application/json",
			CreatedTime:  time.Now(),
			ModifiedTime: time.Now(),
		}
		m.files[fileID] = item
		respData, _ := json.Marshal(item)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(respData))),
			Header:     make(http.Header),
		}, nil
	}

	// Handle delete (DELETE): https://www.googleapis.com/drive/v3/files/{fileId}
	if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/drive/v3/files/") {
		m.deleteCalls++
		parts := strings.Split(req.URL.Path, "/")
		fileID := parts[len(parts)-1]
		delete(m.files, fileID)
		delete(m.fileContents, fileID)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	}

	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
		Header:     make(http.Header),
	}, nil
}

func TestUploadUserLyrics_OverwritesAndDeletesDuplicates(t *testing.T) {
	mock := newMockDriveTransport()
	fileName := "Artist - Title [Album] (180.00) <US123::plat456>.json"

	// Pre-populate with two duplicate files with the same name
	mock.files["dup-1"] = FileItem{ID: "dup-1", Name: fileName, MimeType: "application/json"}
	mock.files["dup-2"] = FileItem{ID: "dup-2", Name: fileName, MimeType: "application/json"}

	httpClient := &http.Client{Transport: mock}
	cfg := config.GDrive{
		Enabled: true,
		Accounts: []config.GDriveAccount{
			{ClientID: "cid", RefreshToken: "rt"},
		},
		FolderUserTML: []string{"folder-user-tml"},
	}

	client := NewGDriveClient(cfg, httpClient)

	// Upload user lyrics (should OVERWRITE dup-1, and DELETE dup-2)
	updatedItem, err := client.UploadUserLyrics(context.Background(), fileName, []byte(`{"lyrics":"updated"}`))
	if err != nil {
		t.Fatalf("UploadUserLyrics error: %v", err)
	}
	if updatedItem == nil || updatedItem.ID != "dup-1" {
		t.Fatalf("expected updatedItem ID to be dup-1, got %+v", updatedItem)
	}

	// Verify PATCH was called, POST was NOT called (no duplicates created!)
	if mock.patchCalls != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", mock.patchCalls)
	}
	if mock.postCalls != 0 {
		t.Fatalf("expected 0 POST calls, got %d", mock.postCalls)
	}
	// Verify dup-2 was deleted
	if mock.deleteCalls != 1 {
		t.Fatalf("expected 1 DELETE call for duplicate, got %d", mock.deleteCalls)
	}
	if _, exists := mock.files["dup-2"]; exists {
		t.Fatalf("expected dup-2 to be removed from mock files")
	}
}

func TestUploadUserLyrics_CreatesNewWhenNotExisting(t *testing.T) {
	mock := newMockDriveTransport()
	fileName := "NewArtist - NewTitle (120.00) <null::null>.json"

	httpClient := &http.Client{Transport: mock}
	cfg := config.GDrive{
		Enabled: true,
		Accounts: []config.GDriveAccount{
			{ClientID: "cid", RefreshToken: "rt"},
		},
		FolderUserTML: []string{"folder-user-tml"},
	}

	client := NewGDriveClient(cfg, httpClient)

	newItem, err := client.UploadUserLyrics(context.Background(), fileName, []byte(`{"lyrics":"new"}`))
	if err != nil {
		t.Fatalf("UploadUserLyrics error: %v", err)
	}
	if newItem == nil || newItem.ID != "new-file-id" {
		t.Fatalf("expected newItem ID to be new-file-id, got %+v", newItem)
	}

	if mock.postCalls != 1 {
		t.Fatalf("expected 1 POST call for new file, got %d", mock.postCalls)
	}
	if mock.patchCalls != 0 {
		t.Fatalf("expected 0 PATCH calls, got %d", mock.patchCalls)
	}
}
