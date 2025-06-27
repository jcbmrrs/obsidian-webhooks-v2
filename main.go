package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fvbock/endless"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	bolt "go.etcd.io/bbolt"
)

var jwtSecret = []byte(os.Getenv("JWT_SECRET"))

func init() {
	if len(jwtSecret) == 0 {
		b := make([]byte, 32)
		rand.Read(b)
		jwtSecret = b
	}
}

type Server struct {
	db         *bolt.DB
	clients    map[string]chan *WebhookEvent
	clientsMux sync.RWMutex
	templates  *template.Template
	authToken  string
}

type WebhookEvent struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Path      string    `json:"path"`
	Data      string    `json:"data"`
	Timestamp time.Time `json:"timestamp"`
}

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "webhooks.db"
	}
	
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

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
		log.Fatal(err)
	}

	s := &Server{
		db:        db,
		clients:   make(map[string]chan *WebhookEvent),
		authToken: os.Getenv("AUTH_TOKEN"),
	}

	// Setup graceful shutdown
	setupGracefulShutdown(s)

	// Load templates
	s.templates = template.Must(template.ParseGlob("templates/*.html"))

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
		r.Get("/logout", s.logoutHandler)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on port %s with hot reload support", port)
	log.Printf("Send SIGUSR2 to reload: kill -USR2 %d", os.Getpid())
	log.Fatal(endless.ListenAndServe(":"+port, r))
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")

	var userID string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("webhook_keys"))
		if b != nil {
			val := b.Get([]byte(key))
			if val != nil {
				userID = string(val)
			}
		}
		return nil
	})

	if err != nil || userID == "" {
		http.Error(w, "Invalid webhook key", http.StatusForbidden)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path parameter required", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	event := &WebhookEvent{
		ID:        generateID(),
		UserID:    userID,
		Path:      path,
		Data:      string(body),
		Timestamp: time.Now(),
	}

	// Save event
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		userBucket, err := b.CreateBucketIfNotExists([]byte(userID))
		if err != nil {
			return err
		}

		data, err := json.Marshal(event)
		if err != nil {
			return err
		}

		key := []byte(event.Timestamp.Format(time.RFC3339Nano) + "-" + event.ID)
		return userBucket.Put(key, data)
	})

	if err != nil {
		http.Error(w, "Failed to save event", http.StatusInternalServerError)
		return
	}

	// Notify SSE clients
	s.notifyClients(userID, event)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")

	var userID string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("client_keys"))
		if b != nil {
			val := b.Get([]byte(key))
			if val != nil {
				userID = string(val)
			}
		}
		return nil
	})

	if err != nil || userID == "" {
		http.Error(w, "Invalid client key", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientChan := make(chan *WebhookEvent, 10)
	clientKey := userID + "-" + generateID()

	s.clientsMux.Lock()
	s.clients[clientKey] = clientChan
	s.clientsMux.Unlock()

	defer func() {
		s.clientsMux.Lock()
		delete(s.clients, clientKey)
		s.clientsMux.Unlock()
		close(clientChan)
	}()

	// Send existing events
	var events []*WebhookEvent
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		if b != nil {
			userBucket := b.Bucket([]byte(userID))
			if userBucket != nil {
				userBucket.ForEach(func(k, v []byte) error {
					var event WebhookEvent
					if err := json.Unmarshal(v, &event); err == nil {
						events = append(events, &event)
					}
					return nil
				})
			}
		}
		return nil
	})

	for _, event := range events {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Keep connection open
	for {
		select {
		case event := <-clientChan:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) notifyClients(userID string, event *WebhookEvent) {
	s.clientsMux.RLock()
	defer s.clientsMux.RUnlock()

	for key, client := range s.clients {
		if len(key) > len(userID) && key[:len(userID)] == userID {
			select {
			case client <- event:
			default:
			}
		}
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		IsEnvAuth bool
	}{
		IsEnvAuth: s.authToken != "",
	}
	s.templates.ExecuteTemplate(w, "login.html", data)
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	if token != s.authToken {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Create JWT
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "default",
		"exp":     time.Now().Add(24 * time.Hour * 30).Unix(),
	})

	tokenString, err := jwtToken.SignedString(jwtSecret)
	if err != nil {
		http.Error(w, "Failed to create token", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "auth_token",
		Value:    tokenString,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		MaxAge:   86400 * 30,
	})

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "auth_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("auth_token")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		token, err := jwt.Parse(cookie.Value, func(token *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})

		if err != nil || !token.Valid {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		claims := token.Claims.(jwt.MapClaims)
		userID := claims["user_id"].(string)

		ctx := context.WithValue(r.Context(), "userID", userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)

	// Get user's keys
	var webhookKey, clientKey string
	s.db.View(func(tx *bolt.Tx) error {
		users := tx.Bucket([]byte("users"))
		if users != nil {
			if val := users.Get([]byte(userID + ":webhook")); val != nil {
				webhookKey = string(val)
			}
			if val := users.Get([]byte(userID + ":client")); val != nil {
				clientKey = string(val)
			}
		}
		return nil
	})

	// Get events
	var events []*WebhookEvent
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		if b != nil {
			userBucket := b.Bucket([]byte(userID))
			if userBucket != nil {
				userBucket.ForEach(func(k, v []byte) error {
					var event WebhookEvent
					if err := json.Unmarshal(v, &event); err == nil {
						events = append(events, &event)
					}
					return nil
				})
			}
		}
		return nil
	})

	data := struct {
		UserID      string
		WebhookKey  string
		ClientKey   string
		Events      []*WebhookEvent
		WebhookURL  string
	}{
		UserID:     userID,
		WebhookKey: webhookKey,
		ClientKey:  clientKey,
		Events:     events,
		WebhookURL: r.Host + "/webhook/" + webhookKey,
	}

	s.templates.ExecuteTemplate(w, "dashboard.html", data)
}

func (s *Server) generateTokenHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)

	// Generate both keys
	webhookKey := "wh_" + generateID()
	clientKey := "cl_" + generateID()

	// Save mappings
	err := s.db.Update(func(tx *bolt.Tx) error {
		// Save webhook key mapping
		webhookKeys := tx.Bucket([]byte("webhook_keys"))
		if err := webhookKeys.Put([]byte(webhookKey), []byte(userID)); err != nil {
			return err
		}

		// Save client key mapping
		clientKeys := tx.Bucket([]byte("client_keys"))
		if err := clientKeys.Put([]byte(clientKey), []byte(userID)); err != nil {
			return err
		}

		// Save user -> keys mapping
		users := tx.Bucket([]byte("users"))
		if err := users.Put([]byte(userID+":webhook"), []byte(webhookKey)); err != nil {
			return err
		}
		return users.Put([]byte(userID+":client"), []byte(clientKey))
	})

	if err != nil {
		http.Error(w, "Failed to generate tokens", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) rotateWebhookKeyHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)

	// Get old key to delete
	var oldKey string
	s.db.View(func(tx *bolt.Tx) error {
		users := tx.Bucket([]byte("users"))
		if users != nil {
			if val := users.Get([]byte(userID + ":webhook")); val != nil {
				oldKey = string(val)
			}
		}
		return nil
	})

	// Generate new key
	newKey := "wh_" + generateID()

	// Update mappings
	err := s.db.Update(func(tx *bolt.Tx) error {
		// Remove old key
		if oldKey != "" {
			webhookKeys := tx.Bucket([]byte("webhook_keys"))
			webhookKeys.Delete([]byte(oldKey))
		}

		// Add new key
		webhookKeys := tx.Bucket([]byte("webhook_keys"))
		if err := webhookKeys.Put([]byte(newKey), []byte(userID)); err != nil {
			return err
		}

		// Update user mapping
		users := tx.Bucket([]byte("users"))
		return users.Put([]byte(userID+":webhook"), []byte(newKey))
	})

	if err != nil {
		http.Error(w, "Failed to rotate webhook key", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) rotateClientKeyHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value("userID").(string)

	// Get old key to delete
	var oldKey string
	s.db.View(func(tx *bolt.Tx) error {
		users := tx.Bucket([]byte("users"))
		if users != nil {
			if val := users.Get([]byte(userID + ":client")); val != nil {
				oldKey = string(val)
			}
		}
		return nil
	})

	// Generate new key
	newKey := "cl_" + generateID()

	// Update mappings
	err := s.db.Update(func(tx *bolt.Tx) error {
		// Remove old key
		if oldKey != "" {
			clientKeys := tx.Bucket([]byte("client_keys"))
			clientKeys.Delete([]byte(oldKey))
		}

		// Add new key
		clientKeys := tx.Bucket([]byte("client_keys"))
		if err := clientKeys.Put([]byte(newKey), []byte(userID)); err != nil {
			return err
		}

		// Update user mapping
		users := tx.Bucket([]byte("users"))
		return users.Put([]byte(userID+":client"), []byte(newKey))
	})

	if err != nil {
		http.Error(w, "Failed to rotate client key", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func setupGracefulShutdown(s *Server) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGUSR1)

	go func() {
		sig := <-c
		log.Printf("Received signal %v, shutting down gracefully...", sig)
		
		// Close all SSE clients
		s.clientsMux.Lock()
		for key, client := range s.clients {
			close(client)
			delete(s.clients, key)
		}
		s.clientsMux.Unlock()
		
		// Close database
		if err := s.db.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
		
		log.Println("Graceful shutdown complete")
		os.Exit(0)
	}()
}