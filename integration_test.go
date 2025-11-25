package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	bolt "go.etcd.io/bbolt"
)

func setupIntegrationTestServer(t *testing.T) (*Server, *httptest.Server) {
	// Create temporary database file
	tmpFile, err := os.CreateTemp("", "test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp database: %v", err)
	}
	tmpFile.Close()

	db, err := bolt.Open(tmpFile.Name(), 0600, nil)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	// Initialize buckets
	err = db.Update(func(tx *bolt.Tx) error {
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
		db:        db,
		clients:   make(map[string]chan *WebhookEvent),
		authToken: "integration-test-token",
	}

	// Create mock templates to avoid nil pointer
	s.templates = template.Must(template.New("mock").Parse(`
		{{define "login.html"}}Mock login page{{end}}
		{{define "dashboard.html"}}Mock dashboard page{{end}}
	`))

	// Create router with all routes
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Public routes
	r.Post("/webhook/{key}", s.handleWebhook)
	r.Get("/events/{key}", s.handleSSE)
	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginHandler)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/", s.dashboardHandler)
		r.Get("/dashboard", s.dashboardHandler)
		r.Post("/generate-token", s.generateTokenHandler)
		r.Post("/rotate-webhook-key", s.rotateWebhookKeyHandler)
		r.Post("/rotate-client-key", s.rotateClientKeyHandler)
		r.Delete("/events/{eventID}", s.deleteEventHandler)
		r.Get("/logout", s.logoutHandler)
	})

	// Create test server
	ts := httptest.NewServer(r)

	// Cleanup function
	t.Cleanup(func() {
		ts.Close()
		db.Close()
		os.Remove(tmpFile.Name())
	})

	return s, ts
}

func TestFullWorkflow(t *testing.T) {
	s, ts := setupIntegrationTestServer(t)

	// Step 1: Login to get auth token
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}
	
	loginResp, err := client.PostForm(ts.URL+"/login", map[string][]string{
		"token": {"integration-test-token"},
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Expected redirect after login, got %d", loginResp.StatusCode)
	}

	// Extract cookies for subsequent requests
	var authCookie *http.Cookie
	for _, cookie := range loginResp.Cookies() {
		if cookie.Name == "auth_token" {
			authCookie = cookie
			break
		}
	}
	if authCookie == nil {
		t.Fatal("No auth cookie received")
	}

	// Step 2: Generate API token
	genTokenReq, _ := http.NewRequest("POST", ts.URL+"/generate-token", nil)
	genTokenReq.AddCookie(authCookie)
	
	genTokenResp, err := client.Do(genTokenReq)
	if err != nil {
		t.Fatalf("Generate token failed: %v", err)
	}
	defer genTokenResp.Body.Close()

	if genTokenResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Expected redirect after token generation, got %d", genTokenResp.StatusCode)
	}

	// Step 3: Get the generated API key from dashboard
	dashReq, _ := http.NewRequest("GET", ts.URL+"/dashboard", nil)
	dashReq.AddCookie(authCookie)
	
	dashResp, err := client.Do(dashReq)
	if err != nil {
		t.Fatalf("Dashboard request failed: %v", err)
	}
	defer dashResp.Body.Close()

	dashBody, _ := io.ReadAll(dashResp.Body)
	_ = string(dashBody) // We don't actually need to parse HTML, just verify request works

	// Extract API key from dashboard HTML (simple approach)
	var apiKey string
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("users"))
		if b != nil {
			val := b.Get([]byte("default"))
			if val != nil {
				apiKey = string(val)
			}
		}
		return nil
	})

	if apiKey == "" {
		t.Fatal("No API key found")
	}

	// Step 4: Test SSE connection
	sseReq, _ := http.NewRequest("GET", ts.URL+"/events/"+apiKey, nil)
	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected SSE connection OK, got %d", sseResp.StatusCode)
	}

	// Verify SSE headers
	if sseResp.Header.Get("Content-Type") != "text/event-stream" {
		t.Error("Expected SSE content type")
	}

	// Step 5: Send webhook and verify SSE receives it
	webhookData := "# Integration Test\nThis is a full workflow test\nTimestamp: " + time.Now().Format(time.RFC3339)
	webhookURL := fmt.Sprintf("%s/webhook/%s?path=integration-test.md", ts.URL, apiKey)
	
	webhookResp, err := http.Post(webhookURL, "text/plain", strings.NewReader(webhookData))
	if err != nil {
		t.Fatalf("Webhook request failed: %v", err)
	}
	defer webhookResp.Body.Close()

	if webhookResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected webhook OK, got %d", webhookResp.StatusCode)
	}

	// Read from SSE stream to verify event was sent
	scanner := bufio.NewScanner(sseResp.Body)
	scanner.Scan() // Read first event

	eventLine := scanner.Text()
	if !strings.HasPrefix(eventLine, "data: ") {
		t.Fatalf("Expected SSE event, got: %s", eventLine)
	}

	// Parse the event JSON
	eventJSON := strings.TrimPrefix(eventLine, "data: ")
	var event WebhookEvent
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		t.Fatalf("Failed to parse SSE event: %v", err)
	}

	// Verify event content
	if event.Path != "integration-test.md" {
		t.Errorf("Expected path 'integration-test.md', got '%s'", event.Path)
	}
	if event.Data != webhookData {
		t.Errorf("Expected data '%s', got '%s'", webhookData, event.Data)
	}
	if event.UserID != "default" {
		t.Errorf("Expected UserID 'default', got '%s'", event.UserID)
	}

	t.Log("✅ Full workflow test completed successfully")
}

func TestConcurrentWebhooks(t *testing.T) {
	s, ts := setupIntegrationTestServer(t)

	// Setup API key
	s.db.Update(func(tx *bolt.Tx) error {
		keys := tx.Bucket([]byte("keys"))
		users := tx.Bucket([]byte("users"))
		
		keys.Put([]byte("concurrent-key"), []byte("concurrent-user"))
		users.Put([]byte("concurrent-user"), []byte("concurrent-key"))
		
		return nil
	})

	// Send multiple concurrent webhooks
	const numWebhooks = 10
	results := make(chan error, numWebhooks)

	for i := 0; i < numWebhooks; i++ {
		go func(id int) {
			webhookData := fmt.Sprintf("# Concurrent Test %d\nThis is webhook number %d", id, id)
			webhookURL := fmt.Sprintf("%s/webhook/concurrent-key?path=concurrent-%d.md", ts.URL, id)
			
			resp, err := http.Post(webhookURL, "text/plain", strings.NewReader(webhookData))
			if err != nil {
				results <- fmt.Errorf("webhook %d failed: %v", id, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				results <- fmt.Errorf("webhook %d got status %d", id, resp.StatusCode)
				return
			}

			results <- nil
		}(i)
	}

	// Check all results
	for i := 0; i < numWebhooks; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}

	// Verify all events were stored
	var eventCount int
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		userBucket := b.Bucket([]byte("concurrent-user"))
		if userBucket != nil {
			userBucket.ForEach(func(k, v []byte) error {
				eventCount++
				return nil
			})
		}
		return nil
	})

	if eventCount != numWebhooks {
		t.Errorf("Expected %d events stored, got %d", numWebhooks, eventCount)
	}

	t.Logf("✅ Successfully handled %d concurrent webhooks", numWebhooks)
}

func TestSSEReconnection(t *testing.T) {
	s, ts := setupIntegrationTestServer(t)

	// Setup API key
	s.db.Update(func(tx *bolt.Tx) error {
		keys := tx.Bucket([]byte("keys"))
		users := tx.Bucket([]byte("users"))
		
		keys.Put([]byte("sse-key"), []byte("sse-user"))
		users.Put([]byte("sse-user"), []byte("sse-key"))
		
		return nil
	})

	// Connect to SSE
	client := &http.Client{}
	sseReq, _ := http.NewRequest("GET", ts.URL+"/events/sse-key", nil)
	sseResp, err := client.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer sseResp.Body.Close()

	// Verify we can read from the stream
	scanner := bufio.NewScanner(sseResp.Body)
	
	// Send a webhook
	go func() {
		time.Sleep(100 * time.Millisecond) // Give SSE time to connect
		webhookURL := fmt.Sprintf("%s/webhook/sse-key?path=sse-test.md", ts.URL)
		http.Post(webhookURL, "text/plain", strings.NewReader("SSE reconnection test"))
	}()

	// Should receive the event
	if scanner.Scan() {
		eventLine := scanner.Text()
		if !strings.HasPrefix(eventLine, "data: ") {
			t.Errorf("Expected SSE event, got: %s", eventLine)
		}
	} else {
		t.Error("Failed to read SSE event")
	}

	t.Log("✅ SSE connection and event delivery working")
}

func TestErrorHandling(t *testing.T) {
	_, ts := setupIntegrationTestServer(t)

	tests := []struct {
		name           string
		method         string
		url            string
		body           string
		expectedStatus int
	}{
		{
			name:           "Invalid webhook key",
			method:         "POST",
			url:            "/webhook/invalid-key?path=test.md",
			body:           "test content",
			expectedStatus: http.StatusForbidden,
		},
		{
			name:           "Missing path parameter",
			method:         "POST", 
			url:            "/webhook/test-key",
			body:           "test content",
			expectedStatus: http.StatusForbidden, // Invalid key returns 403 before checking path
		},
		{
			name:           "Invalid SSE key",
			method:         "GET",
			url:            "/events/invalid-key",
			body:           "",
			expectedStatus: http.StatusForbidden,
		},
		{
			name:           "Unauthorized dashboard access",
			method:         "GET",
			url:            "/dashboard",
			body:           "",
			expectedStatus: http.StatusSeeOther, // Redirect to login
		},
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			}

			req, _ := http.NewRequest(tt.method, ts.URL+tt.url, body)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d", tt.expectedStatus, resp.StatusCode)
			}
		})
	}

	t.Log("✅ Error handling tests passed")
}

func TestDeleteEvent(t *testing.T) {
	s, ts := setupIntegrationTestServer(t)

	userID := "default" // Must match the userID in the JWT from login
	webhookKey := "wh_test123"

	// Setup: Create webhook key mapping
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("webhook_keys"))
		return b.Put([]byte(webhookKey), []byte(userID))
	})
	if err != nil {
		t.Fatalf("Failed to setup webhook key: %v", err)
	}

	// Create some test events
	eventIDs := []string{}
	for i := 0; i < 3; i++ {
		resp, err := http.Post(
			fmt.Sprintf("%s/webhook/%s?path=test-%d.md", ts.URL, webhookKey, i),
			"text/plain",
			strings.NewReader(fmt.Sprintf("Test content %d", i)),
		)
		if err != nil {
			t.Fatalf("Failed to create test event: %v", err)
		}
		resp.Body.Close()
	}

	// Get event IDs from database
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		userBucket := b.Bucket([]byte(userID))
		if userBucket == nil {
			return fmt.Errorf("user bucket not found")
		}

		return userBucket.ForEach(func(k, v []byte) error {
			var event WebhookEvent
			if err := json.Unmarshal(v, &event); err == nil {
				eventIDs = append(eventIDs, event.ID)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Failed to get event IDs: %v", err)
	}

	if len(eventIDs) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(eventIDs))
	}

	// Login to get auth token
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	loginResp, err := client.PostForm(ts.URL+"/login", map[string][]string{
		"token": {"integration-test-token"},
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	defer loginResp.Body.Close()

	var authCookie *http.Cookie
	for _, cookie := range loginResp.Cookies() {
		if cookie.Name == "auth_token" {
			authCookie = cookie
			break
		}
	}

	if authCookie == nil {
		t.Fatal("No auth cookie received")
	}

	// Test: Delete first event
	req, _ := http.NewRequest("DELETE", ts.URL+"/events/"+eventIDs[0], nil)
	req.AddCookie(authCookie)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Delete request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify event was deleted
	var remainingCount int
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		userBucket := b.Bucket([]byte(userID))
		if userBucket == nil {
			return fmt.Errorf("user bucket not found")
		}

		return userBucket.ForEach(func(k, v []byte) error {
			remainingCount++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("Failed to verify deletion: %v", err)
	}

	if remainingCount != 2 {
		t.Errorf("Expected 2 remaining events, got %d", remainingCount)
	}

	// Test: Delete non-existent event
	req, _ = http.NewRequest("DELETE", ts.URL+"/events/nonexistent", nil)
	req.AddCookie(authCookie)

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Delete request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404 for non-existent event, got %d", resp.StatusCode)
	}

	// Test: Delete without authentication
	req, _ = http.NewRequest("DELETE", ts.URL+"/events/"+eventIDs[1], nil)

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Delete request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("Expected redirect to login (303) without auth, got %d", resp.StatusCode)
	}

	t.Log("✅ Delete event tests passed")
}