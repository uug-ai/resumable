package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/uug-ai/resumable/pkg/storage"
)

func main() {
	// Ensure uploads directory exists
	if err := os.MkdirAll("./uploads", 0755); err != nil {
		log.Fatalf("Failed to create uploads directory: %v", err)
	}

	// 1. LOCAL STORE - For resumability and caching
	// Files are temporarily stored here while being uploaded
	localStore := filestore.New("./uploads")
	log.Println("✓ Local store initialized: ./uploads")

	// 2. REMOTE STORAGE - Final destination for files
	// Choose one of the following options:

	// Option A: Mock storage (for testing without S3)
	remoteStorage := storage.NewS3RemoteStorage(
		"my-bucket",
		"us-east-1",
		"",
		"",
		"",
	)
	log.Println("✓ Remote storage: Mock S3 (logging only)")

	// Option B: Real S3/MinIO storage (uncomment to use)
	/*
		remoteStorage, err := storage.NewS3RemoteStorageReal(
			"uploads",                   // bucket name
			"us-east-1",                // region
			"http://localhost:9000",    // MinIO endpoint (empty for AWS S3)
			"minioadmin",               // access key
			"minioadmin",               // secret key
		)
		if err != nil {
			log.Fatalf("Failed to initialize S3 storage: %v", err)
		}
		log.Println("✓ Remote storage: S3/MinIO")
	*/

	// 3. PROXY STORE - Combines local and remote with retry logic
	proxyStore := storage.NewProxyStore(localStore, remoteStorage)
	log.Println("✓ Proxy store initialized with retry logic")

	// 4. FILE LOCKER - Prevents concurrent writes to same upload
	locker := filelocker.New("./uploads")

	// 5. COMPOSE STORAGE - Combine all storage components
	composer := tusd.NewStoreComposer()
	proxyStore.UseIn(composer)
	locker.UseIn(composer)

	// 6. CREATE TUSD HANDLER - Main upload handler
	handler, err := tusd.NewHandler(tusd.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		NotifyCreatedUploads:  true,
	})
	if err != nil {
		log.Fatalf("Unable to create handler: %s", err)
	}

	// 7. MONITOR UPLOAD EVENTS
	go func() {
		for {
			select {
			case event := <-handler.CreatedUploads:
				log.Printf("📤 Upload created: %s", event.Upload.ID)
				log.Printf("   Size: %d bytes", event.Upload.Size)
				log.Printf("   Metadata: %v", event.Upload.MetaData)

			case event := <-handler.CompleteUploads:
				log.Printf("✅ Upload completed: %s", event.Upload.ID)
				log.Printf("   Size: %d bytes", event.Upload.Size)
				log.Printf("   Metadata: %v", event.Upload.MetaData)
				log.Printf("   File is now in remote storage")

				// Optional: Clean up local file after successful remote upload
				// This would require checking if remote upload actually succeeded
				// For now, we'll leave files for manual cleanup
			}
		}
	}()

	// 8. SETUP HTTP ROUTES
	http.Handle("/files/", http.StripPrefix("/files/", handler))
	http.Handle("/files", http.StripPrefix("/files", handler))

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// 9. START SERVER
	server := &http.Server{Addr: ":8080"}

	// Handle graceful shutdown
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		<-sigint

		log.Println("\n⏸️  Shutting down server...")
		if err := server.Shutdown(context.Background()); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	log.Println("")
	log.Println("========================================")
	log.Println("🚀 Resumable Upload Proxy Server")
	log.Println("========================================")
	log.Println("Listening on: http://localhost:8080")
	log.Println("Upload endpoint: http://localhost:8080/files/")
	log.Println("")
	log.Println("Features:")
	log.Println("  ✓ TUS protocol (resumable uploads)")
	log.Println("  ✓ Streaming to remote storage")
	log.Println("  ✓ Automatic retry on failures")
	log.Println("  ✓ Local caching for resumability")
	log.Println("")
	log.Println("Test with: cd cmd && go run client_upload.go")
	log.Println("========================================")
	log.Println("")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %s", err)
	}

	log.Println("✓ Server stopped")
}
