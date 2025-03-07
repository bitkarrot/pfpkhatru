package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// ProfileMetadata represents the structure of a Nostr profile metadata (kind 0)
type ProfileMetadata struct {
	Name        string `json:"name,omitempty"`
	About       string `json:"about,omitempty"`
	Picture     string `json:"picture,omitempty"`
	Banner      string `json:"banner,omitempty"`
	NIP05       string `json:"nip05,omitempty"`
	LUD16       string `json:"lud16,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// Config holds the configuration for the relay
type Config struct {
	ListenAddr         string   `json:"listen_addr"`
	DatabasePath       string   `json:"database_path"`
	MediaCachePath     string   `json:"media_cache_path"`
	UpstreamRelays     []string `json:"upstream_relays"`
	MaxConcurrent      int      `json:"max_concurrent"`
	CacheExpirationDays int      `json:"cache_expiration_days"`
	MaxCacheSize       int64    `json:"max_cache_size_mb"` // Maximum cache size in MB
	LRUCheckInterval   int      `json:"lru_check_interval"` // Interval in minutes to check LRU cache
	ResizeImages       bool     `json:"resize_images"` // Whether to resize profile images
	MaxImageSize       int      `json:"max_image_size"` // Maximum width/height for profile images
	ImageQuality       int      `json:"image_quality"` // JPEG quality (1-100)
}

// LoadConfig loads the configuration from a file
func LoadConfig(configPath string) (*Config, error) {
	// Default configuration
	config := &Config{
		ListenAddr:         ":8080",
		DatabasePath:       "./data/pfpcache.db",
		MediaCachePath:     "./data/media_cache",
		UpstreamRelays:     []string{"wss://damus.io", "wss://primal.net", "wss://nos.lol", "wss://purplepag.es"},
		MaxConcurrent:      20,
		CacheExpirationDays: 30,
		MaxCacheSize:       1024, // Default to 1GB
		LRUCheckInterval:   60,   // Default to 1 hour
		ResizeImages:       true,  // Default to resizing images
		MaxImageSize:       200,   // Default to 200x200
		ImageQuality:       85,    // Default to 85% JPEG quality
	}

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Printf("Config file not found at %s, using defaults", configPath)
		// Save the default config for future use
		configJSON, _ := json.MarshalIndent(config, "", "  ")
		if err := os.WriteFile(configPath, configJSON, 0644); err != nil {
			log.Printf("Failed to write default config: %v", err)
		}
		return config, nil
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	// Parse config
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	log.Printf("Loaded configuration from %s", configPath)
	return config, nil
}

// FilesystemStorage is a simple filesystem-based storage implementation
type FilesystemStorage struct {
	baseDir string
	// LRU cache management
	accessTimes     map[string]time.Time
	accessTimesMutex sync.RWMutex
	maxCacheSize    int64 // in bytes
	checkInterval   time.Duration
}

// NewFilesystemStorage creates a new filesystem storage
func NewFilesystemStorage(basePath string, maxCacheSizeMB int64, checkIntervalMinutes int) *FilesystemStorage {
	fs := &FilesystemStorage{
		baseDir:       basePath,
		accessTimes:   make(map[string]time.Time),
		maxCacheSize:  maxCacheSizeMB * 1024 * 1024, // Convert MB to bytes
		checkInterval: time.Duration(checkIntervalMinutes) * time.Minute,
	}
	
	// Start the LRU cleaner in a background goroutine
	go fs.startLRUCleaner()
	
	return fs
}

// startLRUCleaner starts a ticker to periodically check and clean the LRU cache
func (fs *FilesystemStorage) startLRUCleaner() {
	ticker := time.NewTicker(fs.checkInterval)
	defer ticker.Stop()
	
	for range ticker.C {
		fs.cleanLRUCache()
	}
}

// cleanLRUCache checks the current cache size and removes least recently used files if needed
func (fs *FilesystemStorage) cleanLRUCache() {
	log.Printf("Running LRU cache cleanup check")
	
	// Get current cache size
	currentSize, err := fs.getCacheSize()
	if err != nil {
		log.Printf("Error getting cache size: %v", err)
		return
	}
	
	// If we're under the limit, nothing to do
	if currentSize <= fs.maxCacheSize {
		log.Printf("Cache size (%.2f MB) is under limit (%.2f MB), no cleanup needed", 
			float64(currentSize)/(1024*1024), float64(fs.maxCacheSize)/(1024*1024))
		return
	}
	
	// We need to clean up
	log.Printf("Cache size (%.2f MB) exceeds limit (%.2f MB), cleaning up", 
		float64(currentSize)/(1024*1024), float64(fs.maxCacheSize)/(1024*1024))
	
	// Get all files with their access times
	type fileInfo struct {
		path      string
		accessTime time.Time
		size      int64
	}
	
	var files []fileInfo
	
	// Lock the map while we read from it
	fs.accessTimesMutex.RLock()
	for path, accessTime := range fs.accessTimes {
		fullPath := filepath.Join(fs.baseDir, path)
		info, err := os.Stat(fullPath)
		if err != nil {
			// File might have been deleted, remove from access times
			delete(fs.accessTimes, path)
			continue
		}
		files = append(files, fileInfo{
			path:       path,
			accessTime: accessTime,
			size:       info.Size(),
		})
	}
	fs.accessTimesMutex.RUnlock()
	
	// Sort files by access time (oldest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].accessTime.Before(files[j].accessTime)
	})
	
	// Remove files until we're under the limit
	bytesToRemove := currentSize - fs.maxCacheSize
	bytesRemoved := int64(0)
	
	for _, file := range files {
		if bytesRemoved >= bytesToRemove {
			break
		}
		
		fullPath := filepath.Join(fs.baseDir, file.path)
		log.Printf("LRU cache: removing %s (last accessed: %s, size: %.2f KB)", 
			file.path, file.accessTime.Format(time.RFC3339), float64(file.size)/1024)
		
		err := os.Remove(fullPath)
		if err != nil {
			log.Printf("Error removing file %s: %v", fullPath, err)
			continue
		}
		
		// Update the removed bytes count
		bytesRemoved += file.size
		
		// Remove from access times
		fs.accessTimesMutex.Lock()
		delete(fs.accessTimes, file.path)
		fs.accessTimesMutex.Unlock()
	}
	
	log.Printf("LRU cache cleanup complete: removed %.2f MB", float64(bytesRemoved)/(1024*1024))
}

// getCacheSize calculates the total size of all files in the cache
func (fs *FilesystemStorage) getCacheSize() (int64, error) {
	var totalSize int64
	
	err := filepath.Walk(fs.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	
	return totalSize, err
}

// recordAccess records the access time for a file
func (fs *FilesystemStorage) recordAccess(key string) {
	fs.accessTimesMutex.Lock()
	defer fs.accessTimesMutex.Unlock()
	fs.accessTimes[key] = time.Now()
}

// Has checks if a key exists in the storage
func (fs *FilesystemStorage) Has(key string) bool {
	path := filepath.Join(fs.baseDir, key)
	_, err := os.Stat(path)
	if err == nil {
		// Record access time
		fs.recordAccess(key)
		return true
	}
	return false
}

// Get retrieves a value from the storage
func (fs *FilesystemStorage) Get(key string) ([]byte, error) {
	path := filepath.Join(fs.baseDir, key)
	
	// Read the file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	
	// Record access time
	fs.recordAccess(key)
	
	return data, nil
}

// GetReader retrieves a value from the storage as an io.ReadCloser
func (fs *FilesystemStorage) GetReader(key string) (io.ReadCloser, error) {
	path := filepath.Join(fs.baseDir, key)
	
	// Open the file
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	
	// Record access time
	fs.recordAccess(key)
	
	return file, nil
}

// Store stores a value in the storage
func (fs *FilesystemStorage) Store(key string, data interface{}) error {
	path := filepath.Join(fs.baseDir, key)
	
	// Ensure the directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	
	// Create the file
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	
	// Write the data to the file
	var writeErr error
	switch d := data.(type) {
	case []byte:
		_, writeErr = file.Write(d)
	case io.Reader:
		_, writeErr = io.Copy(file, d)
	default:
		return fmt.Errorf("unsupported data type: %T", data)
	}
	
	if writeErr != nil {
		return writeErr
	}
	
	// Record access time
	fs.recordAccess(key)
	
	return nil
}

// Delete removes a key from storage
func (fs *FilesystemStorage) Delete(key string) error {
	path := filepath.Join(fs.baseDir, key)
	
	// Remove from access times
	fs.accessTimesMutex.Lock()
	delete(fs.accessTimes, key)
	fs.accessTimesMutex.Unlock()
	
	return os.Remove(path)
}

// PurgeDirectory removes all files in a directory
func (fs *FilesystemStorage) PurgeDirectory(dirPath string) error {
	fullPath := filepath.Join(fs.baseDir, dirPath)
	
	// Check if directory exists
	_, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		// Directory doesn't exist, nothing to purge
		return nil
	}
	
	// Remove all files in the directory
	err = os.RemoveAll(fullPath)
	if err != nil {
		return err
	}
	
	// Remove all entries with this prefix from access times
	fs.accessTimesMutex.Lock()
	prefix := dirPath + "/"
	for key := range fs.accessTimes {
		if strings.HasPrefix(key, prefix) || key == dirPath {
			delete(fs.accessTimes, key)
		}
	}
	fs.accessTimesMutex.Unlock()
	
	// Recreate the empty directory
	return os.MkdirAll(fullPath, 0755)
}

// List returns a list of keys in the storage
func (fs *FilesystemStorage) List(pattern string) ([]string, error) {
	var keys []string
	
	// If pattern contains wildcards, use filepath.Glob
	if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") || strings.Contains(pattern, "[") {
		// Get the directory part of the pattern
		dir := filepath.Dir(pattern)
		if dir == "." {
			dir = fs.baseDir
		} else {
			dir = filepath.Join(fs.baseDir, dir)
		}
		
		// Get the base part of the pattern
		base := filepath.Base(pattern)
		
		// Create the full pattern
		fullPattern := filepath.Join(dir, base)
		
		// Use filepath.Glob to find matching files
		matches, err := filepath.Glob(fullPattern)
		if err != nil {
			return nil, err
		}
		
		// Convert full paths to keys
		for _, match := range matches {
			// Remove the base directory prefix to get the key
			key := strings.TrimPrefix(match, fs.baseDir+"/")
			keys = append(keys, key)
		}
		
		return keys, nil
	}
	
	// If no wildcards, just check if the file exists
	path := filepath.Join(fs.baseDir, pattern)
	if _, err := os.Stat(path); err == nil {
		keys = append(keys, pattern)
	}
	
	return keys, nil
}

// MediaHandler handles HTTP requests for media files
type MediaHandler struct {
	storage *FilesystemStorage
	config  *Config
}

// NewMediaHandler creates a new media handler
func NewMediaHandler(storage *FilesystemStorage, config *Config) *MediaHandler {
	return &MediaHandler{
		storage: storage,
		config:  config,
	}
}

// ServeHTTP implements the http.Handler interface
func (h *MediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Extract the key from the URL
	key := strings.TrimPrefix(r.URL.Path, "/media/")
	
	// Check if the file exists
	if !h.storage.Has(key) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	
	// Get the file
	data, err := h.storage.Get(key)
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusInternalServerError)
		return
	}
	
	// Determine content type based on file extension
	ext := filepath.Ext(key)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		// Default to octet-stream if we can't determine the content type
		contentType = "application/octet-stream"
	}
	
	// Set content type
	w.Header().Set("Content-Type", contentType)
	
	// Cache for the number of days specified in config
	cacheSeconds := h.config.CacheExpirationDays * 24 * 60 * 60
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
	w.Write(data)
}

// BatchCacheRequest represents a request to cache multiple profile pictures
type BatchCacheRequest struct {
	Pubkeys []string `json:"pubkeys"`
}

// BatchCacheResponse represents the response to a batch cache request
type BatchCacheResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

func main() {
	// Load configuration
	configPath := "./config.json"
	config, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Ensure directories exist
	os.MkdirAll(filepath.Dir(config.DatabasePath), 0755)
	os.MkdirAll(config.MediaCachePath, 0755)

	// Initialize Khatru relay
	relay := khatru.NewRelay()
	relay.Info.Name = "Profile Picture Cache Relay"
	relay.Info.Description = "A Nostr relay that caches profile pictures"
	relay.Info.SupportedNIPs = []any{1, 11, 40, 42}

	// Set up SQLite database for event storage
	db := sqlite3.SQLite3Backend{DatabaseURL: config.DatabasePath}
	if err := db.Init(); err != nil {
		log.Fatalf("Failed to initialize SQLite database: %v", err)
	}

	// Connect relay to database
	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.CountEvents = append(relay.CountEvents, db.CountEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, db.ReplaceEvent)

	// Initialize filesystem storage for media
	storage := NewFilesystemStorage(config.MediaCachePath, config.MaxCacheSize, config.LRUCheckInterval)
	mediaHandler := NewMediaHandler(storage, config)

	// Create a mux for our HTTP handlers
	mux := http.NewServeMux()

	// Serve static files (including client.html)
	mux.Handle("/", http.FileServer(http.Dir(".")))

	// Register the media handler for media serving
	mux.Handle("/media/", http.StripPrefix("/media/", mediaHandler))

	// Set up batch profile caching endpoint
	mux.HandleFunc("/cache-profiles", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body for pubkeys
		var request BatchCacheRequest

		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Start fetching and caching in the background
		go fetchAndCacheProfiles(relay, storage, request.Pubkeys, *config)

		// Return immediately to the client
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(BatchCacheResponse{
			Status:  "accepted",
			Message: "Profile caching initiated",
			Count:   len(request.Pubkeys),
		})
	})

	// Set up profile picture endpoint
	mux.HandleFunc("/profile-pic/", func(w http.ResponseWriter, r *http.Request) {
		// Extract the pubkey from the URL
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		pubkey := parts[2]

		// Check if we have this profile in our database
		events, err := queryEvents(relay, nostr.Filter{
			Kinds:   []int{0},
			Authors: []string{pubkey},
			Limit:   1,
		})

		var event *nostr.Event
		if err != nil || len(events) == 0 {
			// Profile not found locally, try public relays
			log.Printf("Profile not found locally for %s, trying public relays", pubkey)
			metadata, err := fetchProfileFromPublicRelays(pubkey)
			if err != nil {
				http.Error(w, fmt.Sprintf("Profile not found: %v", err), http.StatusNotFound)
				return
			}

			// Check if we already have this image cached
			// Try with different possible extensions
			extensions := []string{".jpg", ".png", ".gif", ".webp"}
			var reader io.ReadCloser
			
			for _, ext := range extensions {
				cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
				if storage.Has(cacheKey) {
					log.Printf("Found cached profile picture for %s with extension %s", pubkey, ext)
					var err error
					reader, err = storage.GetReader(cacheKey)
					if err != nil {
						continue
					}
					break
				}
			}
			
			if reader != nil {
				log.Printf("Serving cached profile picture for %s", pubkey)
				defer reader.Close()
				
				// Try to determine content type based on the file extension we found
				contentType := "image/jpeg" // Default
				
				// Check all possible extensions
				for _, ext := range extensions {
					cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
					if storage.Has(cacheKey) {
						if ext == ".png" {
							contentType = "image/png"
						} else if ext == ".gif" {
							contentType = "image/gif"
						} else if ext == ".webp" {
							contentType = "image/webp"
						}
						break
					}
				}
				
				w.Header().Set("Content-Type", contentType)
				// Cache for the number of days specified in config
				cacheSeconds := config.CacheExpirationDays * 24 * 60 * 60
				w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
				io.Copy(w, reader)
				return
			}

			// If we didn't find a cached image, cache it now
			log.Printf("Fetching profile picture for %s from %s", pubkey, metadata.Picture)
			resp, err := http.Get(metadata.Picture)
			if err != nil {
				log.Printf("Error fetching profile picture: %v", err)
				// If we can't fetch the image, redirect to the original URL
				http.Redirect(w, r, metadata.Picture, http.StatusTemporaryRedirect)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Printf("Error fetching profile picture, status: %d", resp.StatusCode)
				// If we can't fetch the image, redirect to the original URL
				http.Redirect(w, r, metadata.Picture, http.StatusTemporaryRedirect)
				return
			}

			// Cache the image
			var buf bytes.Buffer
			tee := io.TeeReader(resp.Body, &buf)
			if err := storage.Store(fmt.Sprintf("profile-pic/%s.jpg", pubkey), tee); err != nil {
				log.Printf("Error caching profile picture: %v", err)
				// If we can't cache the image, redirect to the original URL
				http.Redirect(w, r, metadata.Picture, http.StatusTemporaryRedirect)
				return
			}

			// Determine content type based on file extension
			ext := filepath.Ext(metadata.Picture)
			contentType := "image/jpeg" // Default
			if ext == ".png" {
				contentType = "image/png"
			} else if ext == ".gif" {
				contentType = "image/gif"
			} else if ext == ".webp" {
				contentType = "image/webp"
			}

			w.Header().Set("Content-Type", contentType)
			// Cache for the number of days specified in config
			cacheSeconds := config.CacheExpirationDays * 24 * 60 * 60
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
			io.Copy(w, &buf)
			return
		} else {
			event = events[0]
		}

		// Parse profile metadata
		var metadata ProfileMetadata
		if err := json.Unmarshal([]byte(event.Content), &metadata); err != nil {
			http.Error(w, "Invalid profile data", http.StatusInternalServerError)
			return
		}

		pictureURL := metadata.Picture
		if pictureURL == "" {
			http.Error(w, "No profile picture found", http.StatusNotFound)
			return
		}

		// Check if we already have this image cached
		// Try with different possible extensions
		extensions := []string{".jpg", ".png", ".gif", ".webp"}
		var reader io.ReadCloser
		
		for _, ext := range extensions {
			cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
			if storage.Has(cacheKey) {
				log.Printf("Found cached profile picture for %s with extension %s", pubkey, ext)
				var err error
				reader, err = storage.GetReader(cacheKey)
				if err != nil {
					continue
				}
				break
			}
		}
		
		if reader != nil {
			log.Printf("Serving cached profile picture for %s", pubkey)
			defer reader.Close()
			
			// Try to determine content type based on the file extension we found
			contentType := "image/jpeg" // Default
			
			// Check all possible extensions
			for _, ext := range extensions {
				cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
				if storage.Has(cacheKey) {
					if ext == ".png" {
						contentType = "image/png"
					} else if ext == ".gif" {
						contentType = "image/gif"
					} else if ext == ".webp" {
						contentType = "image/webp"
					}
					break
				}
			}
			
			w.Header().Set("Content-Type", contentType)
			// Cache for the number of days specified in config
			cacheSeconds := config.CacheExpirationDays * 24 * 60 * 60
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
			io.Copy(w, reader)
			return
		}

		// Fetch and cache the image
		log.Printf("Fetching profile picture for %s from %s", pubkey, pictureURL)
		resp, err := http.Get(pictureURL)
		if err != nil {
			log.Printf("Error fetching profile picture: %v", err)
			// If we can't fetch the image, redirect to the original URL
			http.Redirect(w, r, pictureURL, http.StatusTemporaryRedirect)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Error fetching profile picture, status: %d", resp.StatusCode)
			// If we can't fetch the image, redirect to the original URL
			http.Redirect(w, r, pictureURL, http.StatusTemporaryRedirect)
			return
		}

		// Cache the image
		var buf bytes.Buffer
		tee := io.TeeReader(resp.Body, &buf)
		if err := storage.Store(fmt.Sprintf("profile-pic/%s.jpg", pubkey), tee); err != nil {
			log.Printf("Error caching profile picture: %v", err)
			// If we can't cache the image, redirect to the original URL
			http.Redirect(w, r, pictureURL, http.StatusTemporaryRedirect)
			return
		}

		// Determine content type based on file extension
		ext := filepath.Ext(pictureURL)
		contentType := "image/jpeg" // Default
		if ext == ".png" {
			contentType = "image/png"
		} else if ext == ".gif" {
			contentType = "image/gif"
		} else if ext == ".webp" {
			contentType = "image/webp"
		}

		w.Header().Set("Content-Type", contentType)
		// Cache for the number of days specified in config
		cacheSeconds := config.CacheExpirationDays * 24 * 60 * 60
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
		io.Copy(w, &buf)
	})

	// Handle batch caching of profile pictures from follows
	mux.HandleFunc("/cache-follows/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract the pubkey from the URL
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		pubkey := parts[2]

		// Limit parameter (default to 500)
		limitStr := r.URL.Query().Get("limit")
		limit := 500
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
				if limit > 1000 {
					limit = 1000 // Cap at 1000 to prevent abuse
				}
			}
		}

		// Start a goroutine to fetch follows and cache their profile pictures
		go func() {
			// Get the user's follows
			follows, err := fetchFollows(pubkey, limit, config, storage)
			if err != nil {
				log.Printf("Error fetching follows for %s: %v", pubkey, err)
				return
			}

			log.Printf("Fetched %d follows for %s, caching profile pictures", len(follows), pubkey)
			
			// Cache profile pictures for all follows
			fetchAndCacheProfiles(relay, storage, follows, *config)
		}()

		// Return immediately to the client
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(BatchCacheResponse{
			Status:  "accepted",
			Message: fmt.Sprintf("Caching profile pictures for follows of %s", pubkey),
			Count:   limit,
		})
	})

	// Handle batch caching of profile pictures
	mux.HandleFunc("/batch-cache", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		var request BatchCacheRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		
		if len(request.Pubkeys) == 0 {
			http.Error(w, "No pubkeys provided", http.StatusBadRequest)
			return
		}
		
		// Limit the number of pubkeys to process
		maxPubkeys := 1000
		if len(request.Pubkeys) > maxPubkeys {
			request.Pubkeys = request.Pubkeys[:maxPubkeys]
		}
		
		// Get limit parameter (default to 50)
		limitStr := r.URL.Query().Get("limit")
		limit := 50
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
				if limit > 1000 {
					limit = 1000 // Cap at 1000 to prevent abuse
				}
			}
		}
		
		// If we have a single pubkey and a limit, try to fetch follows
		if len(request.Pubkeys) == 1 && limit > 0 {
			pubkey := request.Pubkeys[0]
			go func() {
				follows, err := fetchFollows(pubkey, limit, config, storage)
				if err != nil {
					log.Printf("Error fetching follows for %s: %v", pubkey, err)
					return
				}
				log.Printf("Successfully fetched %d follows for %s", len(follows), pubkey)
			}()
			
			response := BatchCacheResponse{
				Status:  "accepted",
				Message: fmt.Sprintf("Fetching and caching up to %d profile pictures for follows of %s", limit, pubkey),
				Count:   limit,
			}
			
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}
		
		// Otherwise, just cache the provided pubkeys
		go func() {
			fetchAndCacheProfiles(relay, storage, request.Pubkeys, *config)
		}()
		
		response := BatchCacheResponse{
			Status:  "accepted",
			Message: fmt.Sprintf("Started caching %d profile pictures", len(request.Pubkeys)),
			Count:   len(request.Pubkeys),
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Handle cache purging
	mux.HandleFunc("/purge-cache/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			http.Error(w, "Method not allowed. Use DELETE or POST.", http.StatusMethodNotAllowed)
			return
		}
		
		// Extract the cache type from the URL
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 {
			http.Error(w, "Invalid URL. Use /purge-cache/all or /purge-cache/profile-pics", http.StatusBadRequest)
			return
		}
		
		cacheType := parts[2]
		var message string
		var err error
		
		switch cacheType {
		case "all":
			// Purge all cached media
			err = storage.PurgeDirectory("")
			message = "All cached media purged successfully"
		case "profile-pics":
			// Purge only profile pictures
			err = storage.PurgeDirectory("profile-pic")
			message = "Profile picture cache purged successfully"
		default:
			http.Error(w, "Invalid cache type. Use 'all' or 'profile-pics'", http.StatusBadRequest)
			return
		}
		
		if err != nil {
			log.Printf("Error purging cache: %v", err)
			http.Error(w, fmt.Sprintf("Error purging cache: %v", err), http.StatusInternalServerError)
			return
		}
		
		// Return success response
		response := map[string]string{
			"status": "success",
			"message": message,
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Handle single profile picture purging
	mux.HandleFunc("/purge-profile-pic/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			http.Error(w, "Method not allowed. Use DELETE or POST.", http.StatusMethodNotAllowed)
			return
		}
		
		// Extract the pubkey from the URL
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 3 {
			http.Error(w, "Invalid URL. Use /purge-profile-pic/{pubkey}", http.StatusBadRequest)
			return
		}
		
		pubkey := parts[2]
		if pubkey == "" {
			http.Error(w, "Invalid pubkey", http.StatusBadRequest)
			return
		}
		
		// Try to delete profile pictures with different extensions
		extensions := []string{".jpg", ".png", ".gif", ".webp", ""} // Empty string for legacy files without extension
		deleted := false
		
		for _, ext := range extensions {
			cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
			if storage.Has(cacheKey) {
				err := storage.Delete(cacheKey)
				if err != nil {
					log.Printf("Error deleting profile picture for %s: %v", pubkey, err)
				} else {
					deleted = true
					log.Printf("Deleted profile picture for %s with extension %s", pubkey, ext)
				}
			}
		}
		
		if !deleted {
			http.Error(w, fmt.Sprintf("No cached profile picture found for %s", pubkey), http.StatusNotFound)
			return
		}
		
		// Return success response
		response := map[string]string{
			"status": "success",
			"message": fmt.Sprintf("Profile picture for %s purged successfully", pubkey),
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Set up the Khatru relay to handle profile metadata events
	relay.OnEventSaved = append(relay.OnEventSaved, func(ctx context.Context, event *nostr.Event) {
		if event.Kind != 0 {
			return // Only process profile metadata events
		}

		// Parse profile metadata
		var metadata ProfileMetadata
		if err := json.Unmarshal([]byte(event.Content), &metadata); err != nil {
			return
		}

		if metadata.Picture == "" {
			return
		}

		// Create a key for this profile picture
		cacheKey := fmt.Sprintf("profile-pic/%s", event.PubKey)

		// Check if we already have this image cached
		if storage.Has(cacheKey) {
			return // Already cached
		}

		// Cache the image in the background
		go cacheProfileImage(storage, event.PubKey, metadata.Picture, config)
	})

	// Combine our handlers with the relay's handlers
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			relay.HandleWebsocket(w, r)
		} else if r.Header.Get("Accept") == "application/nostr+json" {
			relay.HandleNIP11(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	})

	// Set the global relay instance
	setRelayInstance(relay)

	// Start the server
	log.Printf("Starting Profile Picture Cache Relay on %s", config.ListenAddr)
	log.Fatal(http.ListenAndServe(config.ListenAddr, nil))
}

func (h *MediaHandler) CacheProfilePicture(w http.ResponseWriter, r *http.Request) {
	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		http.Error(w, "Missing pubkey parameter", http.StatusBadRequest)
		return
	}
	
	// Check if we have this profile in our database
	relay, err := getRelayInstance()
	if err != nil {
		http.Error(w, "Error getting relay instance", http.StatusInternalServerError)
		return
	}
	
	// Query for the profile metadata
	filter := nostr.Filter{Kinds: []int{0}, Authors: []string{pubkey}}
	events, err := queryEvents(relay, filter)
	if err != nil || len(events) == 0 {
		// Not found in our database, try public relays
		metadata, err := fetchProfileFromPublicRelays(pubkey)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error fetching profile: %v", err), http.StatusInternalServerError)
			return
		}
		
		// Cache the profile picture
		err = cacheProfileImage(h.storage, pubkey, metadata.Picture, h.config)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error caching profile picture: %v", err), http.StatusInternalServerError)
			return
		}
		
		// Resize the image if needed
		if h.config.ResizeImages {
			err = resizeProfileImage(h.storage, pubkey, h.config)
			if err != nil {
				log.Printf("Error resizing profile picture: %v", err)
			}
		}
		
		// Try with different possible extensions
		extensions := []string{".jpg", ".png", ".gif", ".webp"}
		
		for _, ext := range extensions {
			cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
			if h.storage.Has(cacheKey) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"success":true,"path":"/media/` + cacheKey + `"}`))
				return
			}
		}
		
		http.Error(w, "Failed to cache profile picture", http.StatusInternalServerError)
		return
	}
	
	// Parse the profile metadata
	var metadata ProfileMetadata
	if err := json.Unmarshal([]byte(events[0].Content), &metadata); err != nil {
		http.Error(w, fmt.Sprintf("Error parsing profile: %v", err), http.StatusInternalServerError)
		return
	}
	
	// Check if the profile has a picture URL
	if metadata.Picture == "" {
		http.Error(w, "Profile has no picture URL", http.StatusNotFound)
		return
	}
	
	// Cache the profile picture
	err = cacheProfileImage(h.storage, pubkey, metadata.Picture, h.config)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error caching profile picture: %v", err), http.StatusInternalServerError)
		return
	}
	
	// Resize the image if needed
	if h.config.ResizeImages {
		err = resizeProfileImage(h.storage, pubkey, h.config)
		if err != nil {
			log.Printf("Error resizing profile picture: %v", err)
		}
	}
	
	// Try with different possible extensions
	extensions := []string{".jpg", ".png", ".gif", ".webp"}
	
	for _, ext := range extensions {
		cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
		if h.storage.Has(cacheKey) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":true,"path":"/media/` + cacheKey + `"}`))
			return
		}
	}
	
	http.Error(w, "Failed to cache profile picture", http.StatusInternalServerError)
}

func (h *MediaHandler) PurgeProfilePicture(w http.ResponseWriter, r *http.Request) {
	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		http.Error(w, "Missing pubkey parameter", http.StatusBadRequest)
		return
	}
	
	// Try with different possible extensions
	extensions := []string{".jpg", ".png", ".gif", ".webp"}
	var deleted bool
	
	for _, ext := range extensions {
		cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
		if h.storage.Has(cacheKey) {
			err := h.storage.Delete(cacheKey)
			if err != nil {
				http.Error(w, fmt.Sprintf("Error deleting profile picture: %v", err), http.StatusInternalServerError)
				return
			}
			deleted = true
		}
	}
	
	if deleted {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}`))
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":false,"error":"Profile picture not found"}`))
	}
}

// cacheProfileImage caches a profile image
func cacheProfileImage(storage *FilesystemStorage, pubkey, pictureURL string, config *Config) error {
	log.Printf("Caching profile picture for %s: %s", pubkey, pictureURL)
	
	// Extract file extension from the URL
	ext := filepath.Ext(pictureURL)
	if ext == "" {
		// Try to get extension from the last part of the URL path
		parts := strings.Split(pictureURL, "/")
		if len(parts) > 0 {
			lastPart := parts[len(parts)-1]
			if strings.Contains(lastPart, ".") {
				ext = "." + strings.Split(lastPart, ".")[1]
			}
		}
	}
	
	// Create a client with a timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	
	// Fetch the image
	resp, err := client.Get(pictureURL)
	if err != nil {
		return fmt.Errorf("error fetching profile picture: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error fetching profile picture: status code %d", resp.StatusCode)
	}
	
	// If we still don't have an extension, try to get it from the Content-Type header
	if ext == "" {
		contentType := resp.Header.Get("Content-Type")
		extensions, _ := mime.ExtensionsByType(contentType)
		if len(extensions) > 0 {
			ext = extensions[0]
		} else {
			// Default to .jpg if we can't determine the extension
			ext = ".jpg"
		}
	}

	// Normalize extensions
	ext = strings.ToLower(ext)
	if ext == ".jpe" || ext == ".jfif" || ext == ".jpeg" {
		ext = ".jpg"
	}
	
	// Ensure the extension starts with a dot
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	// Create the key for storage
	key := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
	
	// Save the original image first
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading image data: %w", err)
	}
	
	// Store the original image
	if err := storage.Store(key, imageData); err != nil {
		return fmt.Errorf("error storing profile picture: %w", err)
	}
	
	log.Printf("Successfully cached profile picture for %s with extension %s", pubkey, ext)
	return nil
}

// Resize a previously cached image
func resizeProfileImage(storage *FilesystemStorage, pubkey string, config *Config) error {
	// Find the image in storage
	files, err := storage.List("profile-pic/" + pubkey + ".*")
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no profile picture found for %s", pubkey)
	}
	
	// Get the first matching file
	key := files[0]
	
	// Log the file extension for debugging
	log.Printf("Resizing image with extension: %s", filepath.Ext(key))
	
	// Get the image data
	data, err := storage.Get(key)
	if err != nil {
		return fmt.Errorf("error reading profile picture: %w", err)
	}
	
	// Decode the image
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("error decoding profile picture: %w", err)
	}
	
	// Resize the image
	resized := imaging.Resize(img, config.MaxImageSize, 0, imaging.Lanczos)
	
	// Create a buffer to store the resized image
	buf := new(bytes.Buffer)
	
	// Encode the resized image
	switch format {
	case "jpeg":
		err = jpeg.Encode(buf, resized, &jpeg.Options{Quality: config.ImageQuality})
	case "png":
		err = png.Encode(buf, resized)
	case "gif":
		err = gif.Encode(buf, resized, nil)
	default:
		// Default to JPEG
		err = jpeg.Encode(buf, resized, &jpeg.Options{Quality: config.ImageQuality})
	}
	
	if err != nil {
		return fmt.Errorf("error encoding resized profile picture: %w", err)
	}
	
	// Store the resized image
	if err := storage.Store(key, buf.Bytes()); err != nil {
		return fmt.Errorf("error storing resized profile picture: %w", err)
	}
	
	log.Printf("Successfully resized profile picture for %s", pubkey)
	return nil
}

// fetchAndCacheProfiles fetches and caches profile pictures for a list of pubkeys
func fetchAndCacheProfiles(relay *khatru.Relay, storage *FilesystemStorage, pubkeys []string, config Config) {
	// Limit concurrent fetches
	sem := make(chan struct{}, config.MaxConcurrent)
	
	// Count successful and failed fetches
	var successCount, failCount int
	var mu sync.Mutex // Mutex to protect the counters
	
	// Process each pubkey
	for _, pubkey := range pubkeys {
		sem <- struct{}{} // Acquire semaphore
		
		go func(pk string) {
			defer func() { <-sem }() // Release semaphore when done
			
			// Check if we already have this profile picture cached
			// Try with different possible extensions
			extensions := []string{".jpg", ".png", ".gif", ".webp"}
			
			for _, ext := range extensions {
				cacheKey := fmt.Sprintf("profile-pic/%s%s", pk, ext)
				if storage.Has(cacheKey) {
					mu.Lock()
					successCount++
					mu.Unlock()
					log.Printf("Profile picture for %s already cached (%d/%d)", pk, successCount+failCount, len(pubkeys))
					return
				}
			}
			
			// Try to get the profile from local database first
			events, err := queryEvents(relay, nostr.Filter{
				Kinds:   []int{0},
				Authors: []string{pk},
				Limit:   1,
			})
			
			var metadata ProfileMetadata
			
			if err != nil || len(events) == 0 {
				// Not found locally, try public relays
				meta, err := fetchProfileFromPublicRelays(pk)
				if err != nil {
					mu.Lock()
					failCount++
					mu.Unlock()
					log.Printf("No profile found for %s (%d/%d)", pk, successCount+failCount, len(pubkeys))
					return
				}
				metadata = *meta
			} else {
				// Parse the profile metadata
				if err := json.Unmarshal([]byte(events[0].Content), &metadata); err != nil {
					mu.Lock()
					failCount++
					mu.Unlock()
					log.Printf("Invalid profile data for %s: %v (%d/%d)", pk, err, successCount+failCount, len(pubkeys))
					return
				}
			}
			
			// Check if the profile has a picture URL
			if metadata.Picture == "" {
				mu.Lock()
				failCount++
				mu.Unlock()
				log.Printf("No picture URL in profile for %s (%d/%d)", pk, successCount+failCount, len(pubkeys))
				return
			}
			
			// Cache the image
			if err := cacheProfileImage(storage, pk, metadata.Picture, &config); err != nil {
				mu.Lock()
				failCount++
				mu.Unlock()
				log.Printf("Error caching profile picture for %s: %v (%d/%d)", pk, err, successCount+failCount, len(pubkeys))
			} else {
				mu.Lock()
				successCount++
				mu.Unlock()
				log.Printf("Successfully cached profile picture for %s (%d/%d)", pk, successCount+failCount, len(pubkeys))
			}
		}(pubkey)
	}
	
	// Wait for all goroutines to finish
	for i := 0; i < config.MaxConcurrent; i++ {
		sem <- struct{}{}
	}
	
	log.Printf("Batch caching completed: %d successful, %d failed out of %d total", 
		successCount, failCount, len(pubkeys))
}

// queryEvents is a helper function to query events from the relay
func queryEvents(relay *khatru.Relay, filter nostr.Filter) ([]*nostr.Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eventsChan, err := relay.QueryEvents[0](ctx, filter)
	if err != nil {
		return nil, err
	}

	var events []*nostr.Event
	for event := range eventsChan {
		events = append(events, event)
	}

	return events, nil
}

// fetchProfileFromPublicRelays fetches a profile from public relays
func fetchProfileFromPublicRelays(pubkey string) (*ProfileMetadata, error) {
	// Get the configuration
	config, err := LoadConfig("./config.json")
	if err != nil {
		log.Printf("Error loading config, using default relays: %v", err)
		// Use default relays if config can't be loaded
		config = &Config{
			UpstreamRelays: []string{"wss://damus.io", "wss://primal.net", "wss://nos.lol", "wss://purplepag.es"},
		}
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Query for the profile
	filter := nostr.Filter{
		Kinds:   []int{0},
		Authors: []string{pubkey},
		Limit:   1,
	}

	log.Printf("Querying public relays for profile of %s", pubkey)
	
	// Try each relay individually to avoid WaitGroup issues
	var profileEvent *nostr.Event
	
	// Create a channel to receive events
	eventChan := make(chan *nostr.Event, 1)
	errChan := make(chan error, 1)
	
	// Try each relay with a timeout
	for _, relayURL := range config.UpstreamRelays {
		log.Printf("Checking relay %s for profile of %s", relayURL, pubkey)
		
		// Create a context with a shorter timeout for each relay
		relayCtx, relayCancel := context.WithTimeout(ctx, 5*time.Second)
		
		go func(relayURL string) {
			// Connect to the relay
			relay, err := nostr.RelayConnect(relayCtx, relayURL)
			if err != nil {
				errChan <- fmt.Errorf("failed to connect to %s: %v", relayURL, err)
				relayCancel()
				return
			}
			
			// Subscribe to events
			sub, err := relay.Subscribe(relayCtx, []nostr.Filter{filter})
			if err != nil {
				errChan <- fmt.Errorf("failed to subscribe to %s: %v", relayURL, err)
				relay.Close()
				relayCancel()
				return
			}
			
			// Process events
			for {
				select {
				case ev := <-sub.Events:
					if ev != nil && ev.Kind == 0 && ev.PubKey == pubkey {
						eventChan <- ev
						relay.Close()
						relayCancel()
						return
					}
				case <-sub.EndOfStoredEvents:
					errChan <- fmt.Errorf("no profile found on %s", relayURL)
					relay.Close()
					relayCancel()
					return
				case <-relayCtx.Done():
					errChan <- fmt.Errorf("timeout querying %s", relayURL)
					relay.Close()
					return
				}
			}
		}(relayURL)
		
		// Wait for either an event or an error
		select {
		case profileEvent = <-eventChan:
			log.Printf("Found profile for %s on relay %s", pubkey, relayURL)
			relayCancel()
			break
		case err := <-errChan:
			log.Printf("Error from relay %s: %v", relayURL, err)
			relayCancel()
			continue
		case <-time.After(5 * time.Second):
			log.Printf("Timeout waiting for relay %s", relayURL)
			relayCancel()
			continue
		}
		
		if profileEvent != nil {
			break
		}
	}
	
	if profileEvent == nil {
		return nil, fmt.Errorf("profile not found on any configured relay")
	}

	// Store the profile in our local database
	relay, err := getRelayInstance()
	if err == nil {
		// Create a separate context for database operations to ensure they complete
		dbCtx := context.Background()
		for _, storeFunc := range relay.StoreEvent {
			if err := storeFunc(dbCtx, profileEvent); err != nil {
				log.Printf("Error storing event: %v", err)
			} else {
				log.Printf("Successfully stored profile event in database for %s", pubkey)
			}
		}
	}

	// Parse the profile metadata
	var metadata ProfileMetadata
	if err := json.Unmarshal([]byte(profileEvent.Content), &metadata); err != nil {
		return nil, fmt.Errorf("error parsing profile: %v", err)
	}

	// Check if the profile has a picture URL
	if metadata.Picture == "" {
		return nil, fmt.Errorf("profile has no picture URL")
	}

	return &metadata, nil
}

// fetchFollows fetches a user's follows from public relays and caches their profile pictures
func fetchFollows(pubkey string, limit int, config *Config, storage *FilesystemStorage) ([]string, error) {
	log.Printf("Fetching follows for %s with limit %d", pubkey, limit)
	
	// Query for the contact list
	filter := nostr.Filter{Kinds: []int{3}, Authors: []string{pubkey}}
	
	// Connect to relays and fetch the contact list
	var follows []string
	var foundEvent bool
	var totalRelaysAttempted int
	var successfulRelays int
	
	for _, url := range config.UpstreamRelays {
		totalRelaysAttempted++
		relay, err := nostr.RelayConnect(context.Background(), url)
		if err != nil {
			log.Printf("Error connecting to relay %s: %v", url, err)
			continue
		}
		
		sub, err := relay.Subscribe(context.Background(), []nostr.Filter{filter})
		if err != nil {
			log.Printf("Error subscribing to relay %s: %v", url, err)
			relay.Close()
			continue
		}
		
		ev := <-sub.Events
		if ev != nil {
			foundEvent = true
			successfulRelays++
			
			// Extract contacts from the "p" tags
			for _, tag := range ev.Tags {
				if len(tag) >= 2 && tag[0] == "p" {
					follows = append(follows, tag[1])
				}
			}
			
			log.Printf("Found %d contacts for %s on relay %s", len(follows), pubkey, url)
			break
		} else {
			log.Printf("No contact list found for %s on relay %s", pubkey, url)
		}
		
		relay.Close()
	}
	
	if !foundEvent {
		log.Printf("Failed to fetch contact list for %s after trying %d relays (successful: %d)", pubkey, totalRelaysAttempted, successfulRelays)
		return nil, fmt.Errorf("failed to fetch contact list for %s", pubkey)
	}
	
	// Limit the number of follows to cache
	if limit > 0 && len(follows) > limit {
		follows = follows[:limit]
	}
	
	log.Printf("Processing %d follows for %s", len(follows), pubkey)
	
	// Step 1: Cache profile pictures for each follow (without resizing)
	var cached []string
	var errors int
	var noPicture int
	
	for _, follow := range follows {
		// Get profile metadata for the follow
		metadata, err := getProfileMetadata(follow, config)
		if err != nil {
			log.Printf("Error getting profile metadata for %s: %v", follow, err)
			errors++
			continue
		}
		
		// Check if the profile has a picture URL
		if metadata.Picture == "" {
			log.Printf("No profile picture found for %s", follow)
			noPicture++
			continue
		}
		
		// Cache the profile picture (without resizing)
		err = cacheProfileImage(storage, follow, metadata.Picture, config)
		if err != nil {
			log.Printf("Error caching profile picture for %s: %v", follow, err)
			errors++
			continue
		}
		
		cached = append(cached, follow)
	}
	
	log.Printf("Cached %d/%d profile pictures for follows of %s (errors: %d, no picture: %d)", 
		len(cached), len(follows), pubkey, errors, noPicture)
	
	// Step 2: Resize all cached images
	if config.ResizeImages {
		log.Printf("Starting batch resize of %d images", len(cached))
		var resizeErrors int
		
		for _, follow := range cached {
			err := resizeProfileImage(storage, follow, config)
			if err != nil {
				log.Printf("Error resizing profile picture for %s: %v", follow, err)
				resizeErrors++
			}
		}
		
		log.Printf("Completed batch resize with %d errors", resizeErrors)
	}
	
	return cached, nil
}

// getProfileMetadata fetches a profile's metadata
func getProfileMetadata(pubkey string, config *Config) (*ProfileMetadata, error) {
	// Query for the profile metadata
	filter := nostr.Filter{Kinds: []int{0}, Authors: []string{pubkey}}
	
	// Try each relay individually to avoid WaitGroup issues
	var profileEvent *nostr.Event
	
	// Create a channel to receive events
	eventChan := make(chan *nostr.Event, 1)
	errChan := make(chan error, 1)
	
	// Try each relay with a timeout
	for _, relayURL := range config.UpstreamRelays {
		log.Printf("Checking relay %s for profile of %s", relayURL, pubkey)
		
		// Create a context with a shorter timeout for each relay
		relayCtx, relayCancel := context.WithTimeout(context.Background(), 5*time.Second)
		
		go func(relayURL string) {
			// Connect to the relay
			relay, err := nostr.RelayConnect(relayCtx, relayURL)
			if err != nil {
				errChan <- fmt.Errorf("failed to connect to %s: %v", relayURL, err)
				relayCancel()
				return
			}
			
			// Subscribe to events
			sub, err := relay.Subscribe(relayCtx, []nostr.Filter{filter})
			if err != nil {
				errChan <- fmt.Errorf("failed to subscribe to %s: %v", relayURL, err)
				relay.Close()
				relayCancel()
				return
			}
			
			// Process events
			for {
				select {
				case ev := <-sub.Events:
					if ev != nil && ev.Kind == 0 && ev.PubKey == pubkey {
						eventChan <- ev
						relay.Close()
						relayCancel()
						return
					}
				case <-sub.EndOfStoredEvents:
					errChan <- fmt.Errorf("no profile found on %s", relayURL)
					relay.Close()
					relayCancel()
					return
				case <-relayCtx.Done():
					errChan <- fmt.Errorf("timeout querying %s", relayURL)
					relay.Close()
					return
				}
			}
		}(relayURL)
		
		// Wait for either an event or an error
		select {
		case profileEvent = <-eventChan:
			log.Printf("Found profile for %s on relay %s", pubkey, relayURL)
			relayCancel()
			break
		case err := <-errChan:
			log.Printf("Error from relay %s: %v", relayURL, err)
			relayCancel()
			continue
		case <-time.After(5 * time.Second):
			log.Printf("Timeout waiting for relay %s", relayURL)
			relayCancel()
			continue
		}
		
		if profileEvent != nil {
			break
		}
	}
	
	if profileEvent == nil {
		return nil, fmt.Errorf("profile not found on any configured relay")
	}

	// Store the profile in our local database
	relay, err := getRelayInstance()
	if err == nil {
		// Create a separate context for database operations to ensure they complete
		dbCtx := context.Background()
		for _, storeFunc := range relay.StoreEvent {
			if err := storeFunc(dbCtx, profileEvent); err != nil {
				log.Printf("Error storing event: %v", err)
			} else {
				log.Printf("Successfully stored profile event in database for %s", pubkey)
			}
		}
	}

	// Parse the profile metadata
	var metadata ProfileMetadata
	if err := json.Unmarshal([]byte(profileEvent.Content), &metadata); err != nil {
		return nil, fmt.Errorf("error parsing profile: %v", err)
	}

	return &metadata, nil
}

// Global relay instance for access from any function
var globalRelay *khatru.Relay

// setRelayInstance sets the global relay instance
func setRelayInstance(relay *khatru.Relay) {
	globalRelay = relay
}

// getRelayInstance gets the global relay instance
func getRelayInstance() (*khatru.Relay, error) {
	if globalRelay == nil {
		return nil, fmt.Errorf("relay instance not set")
	}
	return globalRelay, nil
}

func (h *MediaHandler) HasProfilePicture(w http.ResponseWriter, r *http.Request) {
	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		http.Error(w, "Missing pubkey parameter", http.StatusBadRequest)
		return
	}
	
	// Try with different possible extensions
	extensions := []string{".jpg", ".png", ".gif", ".webp"}
	
	for _, ext := range extensions {
		cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
		if h.storage.Has(cacheKey) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"has_picture":true,"path":"/media/` + cacheKey + `"}`))
			return
		}
	}
	
	// No profile picture found
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"has_picture":false}`))
}

func (h *MediaHandler) GetProfilePicture(w http.ResponseWriter, r *http.Request) {
	pubkey := r.URL.Query().Get("pubkey")
	if pubkey == "" {
		http.Error(w, "Missing pubkey parameter", http.StatusBadRequest)
		return
	}
	
	// Try with different possible extensions
	extensions := []string{".jpg", ".png", ".gif", ".webp"}
	
	for _, ext := range extensions {
		cacheKey := fmt.Sprintf("profile-pic/%s%s", pubkey, ext)
		if h.storage.Has(cacheKey) {
			// Redirect to the media endpoint
			http.Redirect(w, r, "/media/"+cacheKey, http.StatusFound)
			return
		}
	}
	
	// No profile picture found
	http.Error(w, "Profile picture not found", http.StatusNotFound)
}
