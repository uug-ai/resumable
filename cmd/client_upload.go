package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/bdragon300/tusgo"
)

func main() {
	// Parse the base URL
	baseURL, err := url.Parse("http://localhost:8080/files/")
	if err != nil {
		log.Fatalf("Failed to parse URL: %v", err)
	}

	// Create a new tus client
	client := tusgo.NewClient(http.DefaultClient, baseURL)

	// Update server capabilities (optional but recommended)
	if _, err = client.UpdateCapabilities(); err != nil {
		log.Printf("Warning: Failed to update capabilities: %v", err)
	}

	// Open the file to upload
	file, err := os.Open("falcon-60min.mp4.mp4")
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}
	defer file.Close()

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		log.Fatalf("Failed to get file info: %v", err)
	}

	// Create the upload on the server
	upload := tusgo.Upload{}
	metadata := map[string]string{
		"filename": fileInfo.Name(),
		"filetype": "video/mp4",
	}

	if _, err = client.CreateUpload(&upload, fileInfo.Size(), false, metadata); err != nil {
		log.Fatalf("Failed to create upload: %v", err)
	}

	fmt.Printf("Upload created at: %s\n", upload.Location)
	fmt.Printf("File: %s\n", fileInfo.Name())
	fmt.Printf("Size: %d bytes\n", fileInfo.Size())

	// Create an upload stream
	stream := tusgo.NewUploadStream(client, &upload)

	// Upload the file with retry logic
	if err = uploadWithRetry(stream, file); err != nil {
		log.Fatalf("Failed to upload file: %v", err)
	}

	fmt.Printf("\nUpload successful!\n")
	fmt.Printf("Final offset: %d/%d bytes\n", upload.RemoteOffset, upload.RemoteSize)
}

// uploadWithRetry implements retry logic for network errors
func uploadWithRetry(dst *tusgo.UploadStream, src *os.File) error {
	// Set stream and file pointer to be equal to the remote pointer
	if _, err := dst.Sync(); err != nil {
		return err
	}
	if _, err := src.Seek(dst.Tell(), io.SeekStart); err != nil {
		return err
	}

	_, err := io.Copy(dst, src)
	attempts := 10

	for err != nil && attempts > 0 {
		// Only retry on network errors or checksum mismatches
		if _, ok := err.(net.Error); !ok && !errors.Is(err, tusgo.ErrChecksumMismatch) {
			return err // Permanent error, no luck
		}

		fmt.Printf("Upload error: %v. Retrying in 5 seconds... (%d attempts left)\n", err, attempts)
		time.Sleep(5 * time.Second)
		attempts--

		// Sync the stream with server and try again
		if _, err = dst.Sync(); err != nil {
			return err
		}
		if _, err = src.Seek(dst.Tell(), io.SeekStart); err != nil {
			return err
		}

		_, err = io.Copy(dst, src)
	}

	if attempts == 0 {
		return errors.New("too many attempts to upload the data")
	}

	return nil
}
