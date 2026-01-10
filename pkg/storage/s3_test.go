package storage

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// getS3TestConfig reads S3 configuration from environment variables
func getS3TestConfig() (bucket, region, endpoint, accessKey, secretKey string) {
	bucket = os.Getenv("AWS_S3_BUCKET")
	region = os.Getenv("AWS_S3_REGION")
	endpoint = os.Getenv("AWS_S3_ENDPOINT")
	accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	return
}

// getMinioTestConfig reads MinIO configuration from environment variables
func getMinioTestConfig() (bucket, region, endpoint, accessKey, secretKey string) {
	bucket = "test-bucket"
	region = "us-east-1"
	endpoint = os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:9000"
	}
	accessKey = os.Getenv("MINIO_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey = os.Getenv("MINIO_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	return
}

func TestNewS3Storage_BucketValidation(t *testing.T) {
	tests := []struct {
		name        string
		bucket      string
		region      string
		endpoint    string
		accessKey   string
		secretKey   string
		expectError bool
	}{
		{
			name:        "Invalid credentials",
			bucket:      "test-bucket",
			region:      "us-east-1",
			endpoint:    "http://localhost:9000",
			accessKey:   "invalid",
			secretKey:   "invalid",
			expectError: true,
		},
		{
			name:        "Empty bucket name",
			bucket:      "",
			region:      "us-east-1",
			endpoint:    "http://localhost:9000",
			accessKey:   "minioadmin",
			secretKey:   "minioadmin",
			expectError: true,
		},
		{
			name:      "Valid configuration",
			bucket:    "test-bucket",
			region:    "us-east-1",
			endpoint:  "http://localhost:9000",
			accessKey: "minioadmin",
			secretKey: "minioadmin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage, err := NewS3Storage(
				tt.bucket,
				tt.region,
				tt.endpoint,
				tt.accessKey,
				tt.secretKey,
			)

			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				}
				if storage != nil {
					t.Error("expected nil storage on error")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if storage == nil {
					t.Error("expected storage but got nil")
				}
			}
		})
	}
}

func TestS3RemoteStorageReal_InitiateUpload(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// This test requires a running MinIO/S3 instance
	bucket, region, endpoint, accessKey, secretKey := getS3TestConfig()
	if bucket == "" || accessKey == "" {
		t.Skip("skipping test, S3 credentials not configured")
	}

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()
	uploadID := "test-upload-123"
	metadata := map[string]string{
		"filename": "test-file.txt",
	}

	err = storage.InitiateUpload(ctx, uploadID, 1024, metadata)
	if err != nil {
		t.Errorf("InitiateUpload failed: %v", err)
	}

	// Verify upload is tracked
	storage.uploadsLock.RLock()
	info, exists := storage.uploads[uploadID]
	storage.uploadsLock.RUnlock()

	if !exists {
		t.Error("upload not found in tracking map")
	}
	if info.key != "test-file.txt" {
		t.Errorf("expected key 'test-file.txt', got '%s'", info.key)
	}

	// Clean up - abort since we didn't upload any chunks
	_ = storage.AbortUpload(ctx, uploadID)
}

func TestS3RemoteStorageReal_UploadWithParts(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bucket, region, endpoint, accessKey, secretKey := getS3TestConfig()
	if bucket == "" || accessKey == "" {
		t.Skip("skipping test, S3 credentials not configured")
	}

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()
	uploadID := "test-multipart-upload-456"

	// Create test data - S3 requires parts to be at least 5MB (except the last part)
	// Part 1: 5MB
	part1 := bytes.Repeat([]byte("A"), 5*1024*1024)
	// Part 2: 5MB
	part2 := bytes.Repeat([]byte("B"), 5*1024*1024)
	// Part 3: smaller final part (1MB)
	part3 := bytes.Repeat([]byte("C"), 1024*1024)
	totalSize := int64(len(part1) + len(part2) + len(part3))

	metadata := map[string]string{
		"filename":     "multipart-test.txt",
		"content-type": "text/plain",
	}

	t.Log("Step 1: Initiate upload")
	err = storage.InitiateUpload(ctx, uploadID, totalSize, metadata)
	if err != nil {
		t.Fatalf("InitiateUpload failed: %v", err)
	}

	t.Log("Step 2: Upload part 1")
	err = storage.WriteChunk(ctx, uploadID, 0, bytes.NewReader(part1), int64(len(part1)))
	if err != nil {
		t.Fatalf("WriteChunk part 1 failed: %v", err)
	}

	t.Log("Step 3: Upload part 2")
	err = storage.WriteChunk(ctx, uploadID, int64(len(part1)), bytes.NewReader(part2), int64(len(part2)))
	if err != nil {
		t.Fatalf("WriteChunk part 2 failed: %v", err)
	}

	t.Log("Step 4: Upload part 3")
	err = storage.WriteChunk(ctx, uploadID, int64(len(part1)+len(part2)), bytes.NewReader(part3), int64(len(part3)))
	if err != nil {
		t.Fatalf("WriteChunk part 3 failed: %v", err)
	}

	t.Log("Step 5: Check progress")
	progress, err := storage.GetUploadProgress(ctx, uploadID)
	if err != nil {
		t.Errorf("GetUploadProgress failed: %v", err)
	}
	t.Logf("Upload progress: %d / %d bytes", progress, totalSize)

	if progress != totalSize {
		t.Errorf("expected progress %d, got %d", totalSize, progress)
	}

	t.Log("Step 6: Finalize upload")
	err = storage.FinalizeUpload(ctx, uploadID)
	if err != nil {
		t.Fatalf("FinalizeUpload failed: %v", err)
	}

	// Verify upload is no longer tracked
	storage.uploadsLock.RLock()
	_, exists := storage.uploads[uploadID]
	storage.uploadsLock.RUnlock()

	if exists {
		t.Error("upload should be removed from tracking after finalization")
	}

	t.Log("Multipart upload completed successfully")
}

func TestS3RemoteStorageReal_UploadNotFound(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bucket, region, endpoint, accessKey, secretKey := getMinioTestConfig()

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()
	nonExistentID := "non-existent-upload"

	// Try to write chunk to non-existent upload
	err = storage.WriteChunk(ctx, nonExistentID, 0, nil, 0)
	if err == nil {
		t.Error("expected error for non-existent upload")
	}

	// Try to finalize non-existent upload
	err = storage.FinalizeUpload(ctx, nonExistentID)
	if err == nil {
		t.Error("expected error for non-existent upload")
	}
}

func TestS3RemoteStorageReal_GetUploadProgress(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bucket, region, endpoint, accessKey, secretKey := getMinioTestConfig()

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()

	// Test progress for non-existent upload
	progress, err := storage.GetUploadProgress(ctx, "non-existent")
	if err != nil {
		t.Errorf("GetUploadProgress should not error for non-existent upload: %v", err)
	}
	if progress != 0 {
		t.Errorf("expected progress 0 for non-existent upload, got %d", progress)
	}
}

func TestS3RemoteStorageReal_FullUploadCycle(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bucket, region, endpoint, accessKey, secretKey := getMinioTestConfig()

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()
	uploadID := "test-full-upload-123"
	testData := []byte("This is test data for upload validation")

	metadata := map[string]string{
		"filename":     "test-upload.txt",
		"content-type": "text/plain",
	}

	t.Log("Step 1: Initiate upload")
	err = storage.InitiateUpload(ctx, uploadID, int64(len(testData)), metadata)
	if err != nil {
		t.Fatalf("InitiateUpload failed: %v", err)
	}

	t.Log("Step 2: Write chunk")
	err = storage.WriteChunk(ctx, uploadID, 0, bytes.NewReader(testData), int64(len(testData)))
	if err != nil {
		t.Fatalf("WriteChunk failed: %v", err)
	}

	t.Log("Step 3: Check upload progress")
	progress, err := storage.GetUploadProgress(ctx, uploadID)
	if err != nil {
		t.Errorf("GetUploadProgress failed: %v", err)
	}
	t.Logf("Upload progress: %d bytes", progress)

	t.Log("Step 4: Finalize upload")
	err = storage.FinalizeUpload(ctx, uploadID)
	if err != nil {
		t.Fatalf("FinalizeUpload failed: %v", err)
	}

	// Verify upload is no longer tracked
	storage.uploadsLock.RLock()
	_, exists := storage.uploads[uploadID]
	storage.uploadsLock.RUnlock()

	if exists {
		t.Error("upload should be removed from tracking after finalization")
	}

	t.Log("Full upload cycle completed successfully")
}

func TestS3RemoteStorageReal_AbortUpload(t *testing.T) {
	// Skip if no S3 endpoint is available
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	bucket, region, endpoint, accessKey, secretKey := getMinioTestConfig()

	storage, err := NewS3Storage(
		bucket,
		region,
		endpoint,
		accessKey,
		secretKey,
	)
	if err != nil {
		t.Skipf("skipping test, S3 not available: %v", err)
	}

	ctx := context.Background()
	uploadID := "test-abort-upload-456"

	metadata := map[string]string{
		"filename": "test-abort.txt",
	}

	t.Log("Initiate upload for abort test")
	err = storage.InitiateUpload(ctx, uploadID, 1024, metadata)
	if err != nil {
		t.Fatalf("InitiateUpload failed: %v", err)
	}

	// Verify upload is tracked
	storage.uploadsLock.RLock()
	_, exists := storage.uploads[uploadID]
	storage.uploadsLock.RUnlock()

	if !exists {
		t.Fatal("upload should be tracked after initiation")
	}

	t.Log("Abort upload")
	err = storage.AbortUpload(ctx, uploadID)
	if err != nil {
		t.Fatalf("AbortUpload failed: %v", err)
	}

	// Verify upload is removed from tracking
	storage.uploadsLock.RLock()
	_, exists = storage.uploads[uploadID]
	storage.uploadsLock.RUnlock()

	if exists {
		t.Error("upload should be removed from tracking after abort")
	}

	t.Log("Upload aborted successfully")
}
