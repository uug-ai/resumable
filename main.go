package main

import (
	"log"
	"net/http"
	"os"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"

	"github.com/uug-ai/resumable/pkg/storage"
)

func main() {
	// Create a local FileStore for temporary storage and resumability
	// This ensures uploads can be resumed if network fails
	localStore := filestore.New("./uploads")

	// Create remote storage backend (S3, Azure, GCS, etc.)
	// This is where files will ultimately be stored
	// Load configuration from environment variables
	bucket := os.Getenv("AWS_S3_BUCKET")
	region := os.Getenv("AWS_S3_REGION")
	endpoint := os.Getenv("AWS_S3_ENDPOINT")
	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if bucket == "" || accessKey == "" {
		log.Fatal("AWS_S3_BUCKET and AWS_ACCESS_KEY_ID environment variables are required")
	}

	remoteStorage, err := storage.NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		log.Fatalf("Failed to initialize S3 remote storage: %v", err)
	}

	// Create ProxyStore that streams to remote storage while receiving
	proxyStore := storage.NewProxyStore(localStore, remoteStorage)

	// File locker for preventing concurrent writes to same upload
	locker := filelocker.New("./uploads")

	// Compose the storage backend
	composer := tusd.NewStoreComposer()
	proxyStore.UseIn(composer)
	locker.UseIn(composer)

	// Create the tusd handler
	handler, err := tusd.NewHandler(tusd.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
	})
	if err != nil {
		log.Fatalf("unable to create handler: %s", err)
	}

	// Listen for completed uploads
	go func() {
		for {
			event := <-handler.CompleteUploads
			log.Printf("Upload %s finished", event.Upload.ID)
			log.Printf("  Size: %d bytes", event.Upload.Size)
			log.Printf("  Metadata: %v", event.Upload.MetaData)
			log.Printf("  File is now in remote storage and can be cleaned from local")

			// Optional: Clean up local file after successful remote upload
			// if err := handler.Composer.Core.AsTerminatableUpload(event.Upload).Terminate(context.Background()); err != nil {
			//     log.Printf("Failed to cleanup local file: %v", err)
			// }
		}
	}()

	// Setup HTTP routes
	http.Handle("/files/", http.StripPrefix("/files/", handler))
	http.Handle("/files", http.StripPrefix("/files", handler))

	log.Println("Starting resumable upload server on :8080")
	log.Println("Features:")
	log.Println("  - Receives uploads via TUS protocol")
	log.Println("  - Streams data to remote storage in real-time")
	log.Println("  - Automatic retry on remote storage failures")
	log.Println("  - Local storage for resumability")
	log.Println("Endpoint: http://localhost:8080/files/")

	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("unable to listen: %s", err)
	}
}
