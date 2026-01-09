package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3RemoteStorageReal is a production-ready S3 implementation
type S3RemoteStorageReal struct {
	client *s3.Client
	bucket string

	// Track multipart upload IDs
	uploadsLock sync.RWMutex
	uploads     map[string]*s3UploadInfo
}

type s3UploadInfo struct {
	uploadID string
	key      string // S3 object key
	parts    []types.CompletedPart
	partNum  int
}

// NewS3RemoteStorageReal creates a new S3 remote storage
func NewS3RemoteStorageReal(bucket, region, endpoint, accessKey, secretKey string) (*S3RemoteStorageReal, error) {
	log.Printf("S3Real: [ENTER NewS3RemoteStorageReal] bucket=%s region=%s endpoint=%s", bucket, region, endpoint)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // Required for MinIO
		}
	})

	store := &S3RemoteStorageReal{
		client:  client,
		bucket:  bucket,
		uploads: make(map[string]*s3UploadInfo),
	}

	log.Printf("S3Real: [NewS3RemoteStorageReal] created new instance, initial mapSize=%d", len(store.uploads))
	return store, nil
}

func (s *S3RemoteStorageReal) InitiateUpload(ctx context.Context, uploadID string, size int64, metadata map[string]string) error {
	log.Printf("S3Real: [ENTER InitiateUpload] uploadID=%s size=%d metadata=%v", uploadID, size, metadata)

	// Determine the S3 key - use filename from metadata if available, otherwise use uploadID
	key := uploadID
	if filename, ok := metadata["filename"]; ok && filename != "" {
		key = filename
	}

	log.Printf("S3Real: [InitiateUpload] uploadID=%s resolved key=%s", uploadID, key)

	// Validate key is not empty
	if key == "" {
		log.Printf("S3Real: [ERROR InitiateUpload] uploadID=%s - S3 key is empty", uploadID)
		return fmt.Errorf("cannot initiate upload: S3 key is empty (uploadID=%s)", uploadID)
	}

	// Create multipart upload
	log.Printf("S3Real: [InitiateUpload] uploadID=%s calling CreateMultipartUpload", uploadID)
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Metadata: metadata,
	})
	if err != nil {
		log.Printf("S3Real: [ERROR InitiateUpload] uploadID=%s CreateMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	log.Printf("S3Real: [InitiateUpload] uploadID=%q (len=%d) acquiring lock to store upload info", uploadID, len(uploadID))
	s.uploadsLock.Lock()
	log.Printf("S3Real: [InitiateUpload] storing with key=%q (len=%d)", uploadID, len(uploadID))
	s.uploads[uploadID] = &s3UploadInfo{
		uploadID: *result.UploadId,
		key:      key,
		parts:    make([]types.CompletedPart, 0),
		partNum:  1,
	}
	log.Printf("S3Real: [InitiateUpload] uploadID=%q stored in map, s3UploadID=%s, mapSize=%d", uploadID, *result.UploadId, len(s.uploads))

	// Debug: verify what's actually in the map
	for mapKey := range s.uploads {
		log.Printf("S3Real: [InitiateUpload] DEBUG: map contains key=%q (len=%d)", mapKey, len(mapKey))
	}

	s.uploadsLock.Unlock()
	log.Printf("S3Real: [InitiateUpload] uploadID=%q lock released", uploadID)

	log.Printf("S3Real: [SUCCESS InitiateUpload] uploadID=%s s3UploadID=%s key=%s", uploadID, *result.UploadId, key)
	return nil
}

func (s *S3RemoteStorageReal) WriteChunk(ctx context.Context, uploadID string, offset int64, data io.Reader, size int64) error {
	log.Printf("S3Real: [ENTER WriteChunk] uploadID=%s offset=%d size=%d", uploadID, offset, size)

	log.Printf("S3Real: [WriteChunk] uploadID=%s acquiring lock to lookup upload", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3Real: [WriteChunk] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		// Log all known upload IDs for debugging
		knownIDs := make([]string, 0, len(s.uploads))
		log.Printf("S3Real: [WriteChunk] uploadID=%s iterating map to find keys, mapSize=%d", uploadID, len(s.uploads))
		for id := range s.uploads {
			log.Printf("S3Real: [WriteChunk] found key in map: %q (len=%d)", id, len(id))
			knownIDs = append(knownIDs, id)
		}
		log.Printf("S3Real: [WriteChunk] collected %d known IDs: %v", len(knownIDs), knownIDs)
		log.Printf("S3Real: [WriteChunk] looking for uploadID=%q (len=%d)", uploadID, len(uploadID))

		// Check byte-by-byte comparison with first key if any
		if len(knownIDs) > 0 {
			log.Printf("S3Real: [WriteChunk] comparing bytes: searched=%x first_known=%x", []byte(uploadID), []byte(knownIDs[0]))
		}

		s.uploadsLock.Unlock()
		log.Printf("S3Real: [ERROR WriteChunk] uploadID=%s NOT FOUND in map, knownUploads=%v, mapSize=%d", uploadID, knownIDs, len(s.uploads))
		return fmt.Errorf("upload %s not found", uploadID)
	}
	partNumber := int32(info.partNum)
	info.partNum++
	key := info.key
	s3UploadID := info.uploadID
	log.Printf("S3Real: [WriteChunk] uploadID=%s found in map, s3UploadID=%s key=%s partNumber=%d", uploadID, s3UploadID, key, partNumber)
	s.uploadsLock.Unlock()
	log.Printf("S3Real: [WriteChunk] uploadID=%s lock released", uploadID)

	// Read all data into buffer (required for S3 SDK)
	log.Printf("S3Real: [WriteChunk] uploadID=%s reading chunk data into buffer", uploadID)
	buf := new(bytes.Buffer)
	written, err := io.Copy(buf, data)
	if err != nil {
		log.Printf("S3Real: [ERROR WriteChunk] uploadID=%s failed to read chunk data: %v", uploadID, err)
		return fmt.Errorf("failed to read chunk data: %w", err)
	}
	log.Printf("S3Real: [WriteChunk] uploadID=%s read %d bytes into buffer", uploadID, written)

	// Upload part to S3
	log.Printf("S3Real: [WriteChunk] uploadID=%s calling UploadPart s3UploadID=%s partNumber=%d", uploadID, s3UploadID, partNumber)
	result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		PartNumber: aws.Int32(partNumber),
		UploadId:   aws.String(s3UploadID),
		Body:       bytes.NewReader(buf.Bytes()),
	})
	if err != nil {
		log.Printf("S3Real: [ERROR WriteChunk] uploadID=%s UploadPart failed for part %d: %v", uploadID, partNumber, err)
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}
	log.Printf("S3Real: [WriteChunk] uploadID=%s UploadPart succeeded, ETag=%s", uploadID, *result.ETag)

	// Store completed part info
	log.Printf("S3Real: [WriteChunk] uploadID=%s acquiring lock to store completed part", uploadID)
	s.uploadsLock.Lock()
	info.parts = append(info.parts, types.CompletedPart{
		ETag:       result.ETag,
		PartNumber: aws.Int32(partNumber),
	})
	log.Printf("S3Real: [WriteChunk] uploadID=%s stored part %d, totalParts=%d", uploadID, partNumber, len(info.parts))
	s.uploadsLock.Unlock()
	log.Printf("S3Real: [WriteChunk] uploadID=%s lock released", uploadID)

	log.Printf("S3Real: [SUCCESS WriteChunk] uploadID=%s part=%d bytes=%d", uploadID, partNumber, written)
	return nil
}

func (s *S3RemoteStorageReal) FinalizeUpload(ctx context.Context, uploadID string) error {
	log.Printf("S3Real: [ENTER FinalizeUpload] uploadID=%s", uploadID)

	log.Printf("S3Real: [FinalizeUpload] uploadID=%s acquiring lock", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		// Log all known upload IDs for debugging
		knownIDs := make([]string, 0, len(s.uploads))
		log.Printf("S3Real: [FinalizeUpload] uploadID=%s iterating map to find keys, mapSize=%d", uploadID, len(s.uploads))
		for id := range s.uploads {
			log.Printf("S3Real: [FinalizeUpload] found key in map: %q (len=%d)", id, len(id))
			knownIDs = append(knownIDs, id)
		}
		log.Printf("S3Real: [FinalizeUpload] collected %d known IDs: %v", len(knownIDs), knownIDs)
		log.Printf("S3Real: [FinalizeUpload] looking for uploadID=%q (len=%d)", uploadID, len(uploadID))

		// Check byte-by-byte comparison with first key if any
		if len(knownIDs) > 0 {
			log.Printf("S3Real: [FinalizeUpload] comparing bytes: searched=%x first_known=%x", []byte(uploadID), []byte(knownIDs[0]))
		}

		s.uploadsLock.Unlock()
		log.Printf("S3Real: [ERROR FinalizeUpload] uploadID=%s NOT FOUND in map, knownUploads=%v, mapSize=%d", uploadID, knownIDs, len(s.uploads))
		return fmt.Errorf("upload %s not found", uploadID)
	}

	log.Printf("S3Real: [FinalizeUpload] uploadID=%s found in map, s3UploadID=%s key=%s totalParts=%d", uploadID, info.uploadID, info.key, len(info.parts))

	// Sort parts by part number (required by S3)
	sort.Slice(info.parts, func(i, j int) bool {
		return *info.parts[i].PartNumber < *info.parts[j].PartNumber
	})
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s sorted %d parts", uploadID, len(info.parts))

	parts := make([]types.CompletedPart, len(info.parts))
	copy(parts, info.parts)
	uploadIDStr := info.uploadID
	key := info.key
	s.uploadsLock.Unlock()
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s lock released", uploadID)

	// Complete multipart upload
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s calling CompleteMultipartUpload with %d parts", uploadID, len(parts))
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		log.Printf("S3Real: [ERROR FinalizeUpload] uploadID=%s CompleteMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s CompleteMultipartUpload succeeded", uploadID)

	// Clean up tracking
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s acquiring lock to delete from map", uploadID)
	s.uploadsLock.Lock()
	delete(s.uploads, uploadID)
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s deleted from map, newMapSize=%d", uploadID, len(s.uploads))
	s.uploadsLock.Unlock()
	log.Printf("S3Real: [FinalizeUpload] uploadID=%s lock released", uploadID)

	log.Printf("S3Real: [SUCCESS FinalizeUpload] uploadID=%s", uploadID)
	return nil
}

func (s *S3RemoteStorageReal) AbortUpload(ctx context.Context, uploadID string) error {
	log.Printf("S3Real: [ENTER AbortUpload] uploadID=%s", uploadID)

	log.Printf("S3Real: [AbortUpload] uploadID=%s acquiring lock", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3Real: [AbortUpload] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.Unlock()
		log.Printf("S3Real: [AbortUpload] uploadID=%s not found in map (already cleaned up)", uploadID)
		return nil // Already cleaned up
	}
	uploadIDStr := info.uploadID
	key := info.key
	log.Printf("S3Real: [AbortUpload] uploadID=%s found, s3UploadID=%s key=%s, deleting from map", uploadID, uploadIDStr, key)
	delete(s.uploads, uploadID)
	log.Printf("S3Real: [AbortUpload] uploadID=%s deleted from map, newMapSize=%d", uploadID, len(s.uploads))
	s.uploadsLock.Unlock()
	log.Printf("S3Real: [AbortUpload] uploadID=%s lock released", uploadID)

	// Abort multipart upload on S3
	log.Printf("S3Real: [AbortUpload] uploadID=%s calling AbortMultipartUpload", uploadID)
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		log.Printf("S3Real: [ERROR AbortUpload] uploadID=%s AbortMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	log.Printf("S3Real: [SUCCESS AbortUpload] uploadID=%s", uploadID)
	return nil
}

func (s *S3RemoteStorageReal) GetUploadProgress(ctx context.Context, uploadID string) (int64, error) {
	log.Printf("S3Real: [ENTER GetUploadProgress] uploadID=%s", uploadID)

	log.Printf("S3Real: [GetUploadProgress] uploadID=%s acquiring read lock", uploadID)
	s.uploadsLock.RLock()
	log.Printf("S3Real: [GetUploadProgress] uploadID=%s read lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.RUnlock()
		log.Printf("S3Real: [GetUploadProgress] uploadID=%s not found in map, returning 0", uploadID)
		return 0, nil
	}
	uploadIDStr := info.uploadID
	key := info.key
	log.Printf("S3Real: [GetUploadProgress] uploadID=%s found, s3UploadID=%s key=%s", uploadID, uploadIDStr, key)
	s.uploadsLock.RUnlock()
	log.Printf("S3Real: [GetUploadProgress] uploadID=%s read lock released", uploadID)

	// List uploaded parts
	log.Printf("S3Real: [GetUploadProgress] uploadID=%s calling ListParts", uploadID)
	result, err := s.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		log.Printf("S3Real: [ERROR GetUploadProgress] uploadID=%s ListParts failed: %v", uploadID, err)
		return 0, fmt.Errorf("failed to list parts: %w", err)
	}

	// Calculate total uploaded size
	var totalSize int64
	for _, part := range result.Parts {
		totalSize += *part.Size
	}
	log.Printf("S3Real: [GetUploadProgress] uploadID=%s calculated totalSize=%d from %d parts", uploadID, totalSize, len(result.Parts))

	log.Printf("S3Real: [SUCCESS GetUploadProgress] uploadID=%s totalSize=%d", uploadID, totalSize)
	return totalSize, nil
}
