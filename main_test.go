package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

func setupTestServer(t *testing.T) *Server {
	// Create temporary database
	tmpDB, err := bolt.Open(":memory:", 0600, nil)
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}

	// Initialize buckets
	err = tmpDB.Update(func(tx *bolt.Tx) error {
		for _, bucket := range []string{"events", "users", "webhook_keys", "client_keys"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(bucket)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to initialize test database: %v", err)
	}

	s := &Server{
		db:        tmpDB,
		clients:   make(map[string]chan *WebhookEvent),
		authToken: "test-token",
	}

	// Create mock templates
	s.templates = template.Must(template.New("mock").Parse(`
		{{define "login.html"}}Mock login{{end}}
		{{define "dashboard.html"}}Mock dashboard{{end}}
	`))

	// Add test user and keys
	s.db.Update(func(tx *bolt.Tx) error {
		webhookKeys := tx.Bucket([]byte("webhook_keys"))
		clientKeys := tx.Bucket([]byte("client_keys"))
		users := tx.Bucket([]byte("users"))
		
		webhookKeys.Put([]byte("test-webhook-key"), []byte("test-user"))
		clientKeys.Put([]byte("test-client-key"), []byte("test-user"))
		users.Put([]byte("test-user:webhook"), []byte("test-webhook-key"))
		users.Put([]byte("test-user:client"), []byte("test-client-key"))
		
		return nil
	})

	return s
}

func TestWebhookHandler(t *testing.T) {
	s := setupTestServer(t)
	defer s.db.Close()

	tests := []struct {
		name           string
		key            string
		path           string
		body           string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "Valid webhook",
			key:            "test-webhook-key",
			path:           "notes/test.md",
			body:           "# Test Note\nContent here",
			expectedStatus: http.StatusOK,
			expectedBody:   "ok",
		},
		{
			name:           "Invalid key",
			key:            "invalid-key",
			path:           "notes/test.md",
			body:           "content",
			expectedStatus: http.StatusForbidden,
			expectedBody:   "Invalid webhook key\n",
		},
		{
			name:           "Missing path parameter",
			key:            "test-webhook-key",
			path:           "",
			body:           "content",
			expectedStatus: http.StatusBadRequest,
			expectedBody:   "path parameter required\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := fmt.Sprintf("/webhook/%s", tt.key)
			if tt.path != "" {
				url += fmt.Sprintf("?path=%s", tt.path)
			}

			req, err := http.NewRequest("POST", url, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}

			rr := httptest.NewRecorder()
			
			// Create router to handle URL params
			r := chi.NewRouter()
			r.Post("/webhook/{key}", s.handleWebhook)
			
			r.ServeHTTP(rr, req)

			if status := rr.Code; status != tt.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tt.expectedStatus)
			}

			if rr.Body.String() != tt.expectedBody {
				t.Errorf("handler returned unexpected body: got %v want %v",
					rr.Body.String(), tt.expectedBody)
			}
		})
	}
}

func TestWebhookEventStorage(t *testing.T) {
	s := setupTestServer(t)
	defer s.db.Close()

	// Clear any existing events first
	s.db.Update(func(tx *bolt.Tx) error {
		events := tx.Bucket([]byte("events"))
		if userBucket := events.Bucket([]byte("test-user")); userBucket != nil {
			events.DeleteBucket([]byte("test-user"))
			events.CreateBucket([]byte("test-user"))
		}
		return nil
	})

	// Send a webhook
	url := "/webhook/test-webhook-key?path=notes/storage-test.md"
	body := "# Storage Test\nThis tests event storage"
	
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	rr := httptest.NewRecorder()
	
	r := chi.NewRouter()
	r.Post("/webhook/{key}", s.handleWebhook)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected webhook to succeed, got %d", rr.Code)
	}

	// Check if event was stored
	var events []*WebhookEvent
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		userBucket := b.Bucket([]byte("test-user"))
		if userBucket == nil {
			t.Error("User bucket not created")
			return nil
		}

		userBucket.ForEach(func(k, v []byte) error {
			var event WebhookEvent
			if err := json.Unmarshal(v, &event); err == nil {
				events = append(events, &event)
			}
			return nil
		})
		return nil
	})

	if len(events) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(events))
	}

	event := events[0]
	if event.UserID != "test-user" {
		t.Errorf("Expected UserID 'test-user', got '%s'", event.UserID)
	}
	if event.Path != "notes/storage-test.md" {
		t.Errorf("Expected path 'notes/storage-test.md', got '%s'", event.Path)
	}
	if event.Data != body {
		t.Errorf("Expected data '%s', got '%s'", body, event.Data)
	}
}

func TestSSEHandler(t *testing.T) {
	s := setupTestServer(t)
	defer s.db.Close()

	// Create a request to the SSE endpoint
	req, _ := http.NewRequest("GET", "/events/test-client-key", nil)
	rr := httptest.NewRecorder()

	r := chi.NewRouter()
	r.Get("/events/{key}", s.handleSSE)
	
	// Start SSE handler in goroutine
	go r.ServeHTTP(rr, req)
	
	// Give SSE handler time to set up
	time.Sleep(100 * time.Millisecond)

	// Check SSE headers
	headers := rr.Header()
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Error("Expected Content-Type: text/event-stream")
	}
	if headers.Get("Cache-Control") != "no-cache" {
		t.Error("Expected Cache-Control: no-cache")
	}
	if headers.Get("Connection") != "keep-alive" {
		t.Error("Expected Connection: keep-alive")
	}

	// Send a webhook event to trigger SSE
	event := &WebhookEvent{
		ID:        generateID(),
		UserID:    "test-user",
		Path:      "test.md",
		Data:      "test content",
		Timestamp: time.Now(),
	}

	s.notifyClients("test-user", event)
	
	// Give time for event to be sent
	time.Sleep(100 * time.Millisecond)
}

func TestAuthMiddleware(t *testing.T) {
	s := setupTestServer(t)
	defer s.db.Close()

	// Test handler that requires auth
	protectedHandler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Context().Value("userID").(string)
		w.Write([]byte("authenticated: " + userID))
	}))

	tests := []struct {
		name           string
		token          string
		expectedStatus int
		expectRedirect bool
	}{
		{
			name:           "No token",
			token:          "",
			expectedStatus: http.StatusSeeOther,
			expectRedirect: true,
		},
		{
			name:           "Invalid token",
			token:          "invalid.token.here",
			expectedStatus: http.StatusSeeOther,
			expectRedirect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/protected", nil)
			if tt.token != "" {
				req.AddCookie(&http.Cookie{
					Name:  "auth_token",
					Value: tt.token,
				})
			}

			rr := httptest.NewRecorder()
			protectedHandler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}

			if tt.expectRedirect {
				location := rr.Header().Get("Location")
				if location != "/login" {
					t.Errorf("Expected redirect to /login, got %s", location)
				}
			}
		})
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()

	if id1 == id2 {
		t.Error("generateID should return unique IDs")
	}

	if len(id1) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("Expected ID length 32, got %d", len(id1))
	}
}

func TestLoginHandler(t *testing.T) {
	s := setupTestServer(t)
	defer s.db.Close()

	tests := []struct {
		name           string
		token          string
		expectedStatus int
		expectCookie   bool
	}{
		{
			name:           "Valid token",
			token:          "test-token",
			expectedStatus: http.StatusSeeOther,
			expectCookie:   true,
		},
		{
			name:           "Invalid token",
			token:          "wrong-token",
			expectedStatus: http.StatusUnauthorized,
			expectCookie:   false,
		},
		{
			name:           "Empty token",
			token:          "",
			expectedStatus: http.StatusUnauthorized,
			expectCookie:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := fmt.Sprintf("token=%s", tt.token)
			req, _ := http.NewRequest("POST", "/login", strings.NewReader(form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			rr := httptest.NewRecorder()
			s.loginHandler(rr, req)

			if rr.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, rr.Code)
			}

			cookies := rr.Header()["Set-Cookie"]
			hasCookie := len(cookies) > 0 && strings.Contains(cookies[0], "auth_token=")
			
			if tt.expectCookie && !hasCookie {
				t.Error("Expected auth cookie to be set")
			}
			if !tt.expectCookie && hasCookie {
				t.Error("Expected no auth cookie to be set")
			}
		})
	}
}

// Benchmark tests
func BenchmarkWebhookHandler(b *testing.B) {
	s := setupTestServer(&testing.T{})
	defer s.db.Close()

	r := chi.NewRouter()
	r.Post("/webhook/{key}", s.handleWebhook)

	body := strings.NewReader("# Benchmark Test\nThis is a benchmark test")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", "/webhook/test-key?path=bench.md", body)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		
		// Reset body reader
		body.Seek(0, 0)
	}
}

func BenchmarkGenerateID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		generateID()
	}
}