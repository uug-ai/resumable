package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storage is a production-ready S3 implementation
type S3Storage struct {
	client *s3.Client
	bucket string

	// Track multipart upload IDs
	uploadsLock sync.RWMutex
	uploads     map[string]*s3UploadInfo
}

type s3UploadInfo struct {
	uploadID      string
	key           string // S3 object key
	parts         []types.CompletedPart
	partNum       int
	buffer        *bytes.Buffer // Buffer for accumulating data until min part size
	totalSize     int64         // Total expected size of upload
	uploadedBytes int64         // Total bytes uploaded to S3 (excluding buffer)
}

const (
	// MinPartSize is the minimum size for S3 multipart upload parts (except the last part)
	MinPartSize = 5 * 1024 * 1024 // 5 MB
)

// NewS3Storage creates a new S3 remote storage
func NewS3Storage(bucket, region, endpoint, accessKey, secretKey string) (*S3Storage, error) {
	log.Printf("S3: [ENTER NewS3Storage] bucket=%s region=%s endpoint=%s", bucket, region, endpoint)

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

	store := &S3Storage{
		client:  client,
		bucket:  bucket,
		uploads: make(map[string]*s3UploadInfo),
	}

	// Validate that the bucket is reachable
	log.Printf("S3: [NewS3Storage] validating bucket accessibility")
	ctx := context.Background()
	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		log.Printf("S3: [ERROR NewS3Storage] bucket validation failed: %v", err)
		return nil, fmt.Errorf("failed to access S3 bucket '%s': %w (check bucket exists and credentials are valid)", bucket, err)
	}

	log.Printf("S3: [NewS3Storage] bucket validation successful")

	log.Printf("S3: [NewS3Storage] created new instance, initial mapSize=%d", len(store.uploads))

	// Restore in-progress uploads from S3 to allow resuming after restart
	log.Printf("S3: [NewS3Storage] restoring in-progress uploads")
	if err := store.RestoreInProgressUploads(ctx); err != nil {
		log.Printf("S3: [WARN NewS3Storage] failed to restore in-progress uploads: %v", err)
		// Don't fail initialization if restore fails, just log the warning
	}

	return store, nil
}

func (s *S3Storage) InitiateUpload(ctx context.Context, uploadID string, size int64, metadata map[string]string) error {
	log.Printf("S3: [ENTER InitiateUpload] uploadID=%s size=%d metadata=%v", uploadID, size, metadata)

	// Determine the S3 key - prefix with uploadID for restoration, use filename if available
	key := uploadID
	if filename, ok := metadata["filename"]; ok && filename != "" {
		// Format: {uploadID}/{filename} - this allows us to extract uploadID during restoration
		key = uploadID + "/" + filename
	}

	log.Printf("S3: [InitiateUpload] uploadID=%s resolved key=%s", uploadID, key)

	// Validate key is not empty
	if key == "" {
		log.Printf("S3: [ERROR InitiateUpload] uploadID=%s - S3 key is empty", uploadID)
		return fmt.Errorf("cannot initiate upload: S3 key is empty (uploadID=%s)", uploadID)
	}

	// Create multipart upload with TUS uploadID in metadata
	log.Printf("S3: [InitiateUpload] uploadID=%s calling CreateMultipartUpload", uploadID)

	// Add TUS uploadID to metadata so we can restore it later
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata["tus-upload-id"] = uploadID

	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Metadata: metadata,
	})
	if err != nil {
		log.Printf("S3: [ERROR InitiateUpload] uploadID=%s CreateMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	log.Printf("S3: [InitiateUpload] uploadID=%q (len=%d) acquiring lock to store upload info", uploadID, len(uploadID))
	s.uploadsLock.Lock()
	log.Printf("S3: [InitiateUpload] storing with key=%q (len=%d)", uploadID, len(uploadID))
	s.uploads[uploadID] = &s3UploadInfo{
		uploadID:      *result.UploadId,
		key:           key,
		parts:         make([]types.CompletedPart, 0),
		partNum:       1,
		buffer:        new(bytes.Buffer),
		totalSize:     size,
		uploadedBytes: 0,
	}
	log.Printf("S3: [InitiateUpload] uploadID=%q stored in map, s3UploadID=%s, mapSize=%d", uploadID, *result.UploadId, len(s.uploads))

	// Debug: verify what's actually in the map
	for mapKey := range s.uploads {
		log.Printf("S3: [InitiateUpload] DEBUG: map contains key=%q (len=%d)", mapKey, len(mapKey))
	}

	s.uploadsLock.Unlock()
	log.Printf("S3: [InitiateUpload] uploadID=%q lock released", uploadID)

	log.Printf("S3: [SUCCESS InitiateUpload] uploadID=%s s3UploadID=%s key=%s", uploadID, *result.UploadId, key)
	return nil
}

func (s *S3Storage) WriteChunk(ctx context.Context, uploadID string, offset int64, data io.Reader, size int64) error {
	log.Printf("S3: [ENTER WriteChunk] uploadID=%s offset=%d size=%d", uploadID, offset, size)

	log.Printf("S3: [WriteChunk] uploadID=%s acquiring lock to lookup upload", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3: [WriteChunk] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		// Log all known upload IDs for debugging
		knownIDs := make([]string, 0, len(s.uploads))
		log.Printf("S3: [WriteChunk] uploadID=%s iterating map to find keys, mapSize=%d", uploadID, len(s.uploads))
		for id := range s.uploads {
			log.Printf("S3: [WriteChunk] found key in map: %q (len=%d)", id, len(id))
			knownIDs = append(knownIDs, id)
		}
		log.Printf("S3: [WriteChunk] collected %d known IDs: %v", len(knownIDs), knownIDs)
		log.Printf("S3: [WriteChunk] looking for uploadID=%q (len=%d)", uploadID, len(uploadID))

		// Check byte-by-byte comparison with first key if any
		if len(knownIDs) > 0 {
			log.Printf("S3: [WriteChunk] comparing bytes: searched=%x first_known=%x", []byte(uploadID), []byte(knownIDs[0]))
		}

		s.uploadsLock.Unlock()
		log.Printf("S3: [ERROR WriteChunk] uploadID=%s NOT FOUND in map, knownUploads=%v, mapSize=%d", uploadID, knownIDs, len(s.uploads))
		return fmt.Errorf("upload %s not found", uploadID)
	}
	s.uploadsLock.Unlock()

	// Validate offset continuity
	expectedOffset := info.uploadedBytes + int64(info.buffer.Len())
	if offset != expectedOffset {
		log.Printf("S3: [ERROR WriteChunk] uploadID=%s offset mismatch: expected=%d got=%d (gap=%d bytes)",
			uploadID, expectedOffset, offset, offset-expectedOffset)
		return fmt.Errorf("offset mismatch: expected %d, got %d (upload may have been corrupted during restart)", expectedOffset, offset)
	}

	// Read chunk data into a temporary buffer
	log.Printf("S3: [WriteChunk] uploadID=%s reading chunk data", uploadID)
	chunkData, err := io.ReadAll(data)
	if err != nil {
		log.Printf("S3: [ERROR WriteChunk] uploadID=%s failed to read chunk data: %v", uploadID, err)
		return fmt.Errorf("failed to read chunk data: %w", err)
	}
	log.Printf("S3: [WriteChunk] uploadID=%s read %d bytes", uploadID, len(chunkData))

	// Write to buffer and flush if needed
	s.uploadsLock.Lock()
	defer s.uploadsLock.Unlock()

	// Write chunk data to buffer
	info.buffer.Write(chunkData)
	bufferSize := int64(info.buffer.Len())
	log.Printf("S3: [WriteChunk] uploadID=%s buffer now contains %d bytes", uploadID, bufferSize)

	// Determine if this is potentially the last chunk
	// We flush if: buffer >= MinPartSize OR (buffer has data AND we're at the total size)
	currentOffset := offset + int64(len(chunkData))
	isLastChunk := info.totalSize > 0 && currentOffset >= info.totalSize
	shouldFlush := bufferSize >= MinPartSize || (isLastChunk && bufferSize > 0)

	log.Printf("S3: [WriteChunk] uploadID=%s currentOffset=%d totalSize=%d isLastChunk=%v shouldFlush=%v",
		uploadID, currentOffset, info.totalSize, isLastChunk, shouldFlush)

	if !shouldFlush {
		log.Printf("S3: [WriteChunk] uploadID=%s buffering data, not flushing yet", uploadID)
		return nil
	}

	// Flush buffer to S3 as a new part
	partNumber := int32(info.partNum)
	info.partNum++
	key := info.key
	s3UploadID := info.uploadID
	bufferData := info.buffer.Bytes()
	info.buffer.Reset() // Clear buffer after reading

	log.Printf("S3: [WriteChunk] uploadID=%s uploading part %d with %d bytes", uploadID, partNumber, len(bufferData))

	// Upload part to S3
	result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		PartNumber: aws.Int32(partNumber),
		UploadId:   aws.String(s3UploadID),
		Body:       bytes.NewReader(bufferData),
	})
	if err != nil {
		log.Printf("S3: [ERROR WriteChunk] uploadID=%s UploadPart failed for part %d: %v", uploadID, partNumber, err)
		// Restore data to buffer on failure so it can be retried
		info.buffer.Write(bufferData)
		info.partNum-- // Rollback part number
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}
	log.Printf("S3: [WriteChunk] uploadID=%s UploadPart succeeded, ETag=%s", uploadID, *result.ETag)

	// Store completed part info
	info.parts = append(info.parts, types.CompletedPart{
		ETag:       result.ETag,
		PartNumber: aws.Int32(partNumber),
	})
	info.uploadedBytes += int64(len(bufferData))
	log.Printf("S3: [WriteChunk] uploadID=%s stored part %d, totalParts=%d, uploadedBytes=%d", uploadID, partNumber, len(info.parts), info.uploadedBytes)

	log.Printf("S3: [SUCCESS WriteChunk] uploadID=%s part=%d bytes=%d", uploadID, partNumber, len(bufferData))
	return nil
}

func (s *S3Storage) FinalizeUpload(ctx context.Context, uploadID string) error {
	log.Printf("S3: [ENTER FinalizeUpload] uploadID=%s", uploadID)

	log.Printf("S3: [FinalizeUpload] uploadID=%s acquiring lock", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3: [FinalizeUpload] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		// Log all known upload IDs for debugging
		knownIDs := make([]string, 0, len(s.uploads))
		log.Printf("S3: [FinalizeUpload] uploadID=%s iterating map to find keys, mapSize=%d", uploadID, len(s.uploads))
		for id := range s.uploads {
			log.Printf("S3: [FinalizeUpload] found key in map: %q (len=%d)", id, len(id))
			knownIDs = append(knownIDs, id)
		}
		log.Printf("S3: [FinalizeUpload] collected %d known IDs: %v", len(knownIDs), knownIDs)
		log.Printf("S3: [FinalizeUpload] looking for uploadID=%q (len=%d)", uploadID, len(uploadID))

		// Check byte-by-byte comparison with first key if any
		if len(knownIDs) > 0 {
			log.Printf("S3: [FinalizeUpload] comparing bytes: searched=%x first_known=%x", []byte(uploadID), []byte(knownIDs[0]))
		}

		s.uploadsLock.Unlock()
		log.Printf("S3: [ERROR FinalizeUpload] uploadID=%s NOT FOUND in map, knownUploads=%v, mapSize=%d", uploadID, knownIDs, len(s.uploads))
		return fmt.Errorf("upload %s not found", uploadID)
	}

	log.Printf("S3: [FinalizeUpload] uploadID=%s found in map, s3UploadID=%s key=%s totalParts=%d bufferSize=%d",
		uploadID, info.uploadID, info.key, len(info.parts), info.buffer.Len())

	// Flush any remaining buffered data as the final part
	if info.buffer.Len() > 0 {
		log.Printf("S3: [FinalizeUpload] uploadID=%s flushing final %d bytes from buffer", uploadID, info.buffer.Len())
		partNumber := int32(info.partNum)
		info.partNum++
		key := info.key
		s3UploadID := info.uploadID
		bufferData := info.buffer.Bytes()
		info.buffer.Reset()

		// Upload final part to S3
		log.Printf("S3: [FinalizeUpload] uploadID=%s uploading final part %d with %d bytes", uploadID, partNumber, len(bufferData))
		result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			PartNumber: aws.Int32(partNumber),
			UploadId:   aws.String(s3UploadID),
			Body:       bytes.NewReader(bufferData),
		})
		if err != nil {
			s.uploadsLock.Unlock()
			log.Printf("S3: [ERROR FinalizeUpload] uploadID=%s failed to upload final part: %v", uploadID, err)
			return fmt.Errorf("failed to upload final part: %w", err)
		}
		log.Printf("S3: [FinalizeUpload] uploadID=%s uploaded final part %d with %d bytes, ETag=%s",
			uploadID, partNumber, len(bufferData), *result.ETag)

		// Store completed part info
		info.parts = append(info.parts, types.CompletedPart{
			ETag:       result.ETag,
			PartNumber: aws.Int32(partNumber),
		})
		info.uploadedBytes += int64(len(bufferData))
		log.Printf("S3: [FinalizeUpload] uploadID=%s total uploadedBytes=%d", uploadID, info.uploadedBytes)
	}

	// Sort parts by part number (required by S3)
	sort.Slice(info.parts, func(i, j int) bool {
		return *info.parts[i].PartNumber < *info.parts[j].PartNumber
	})
	log.Printf("S3: [FinalizeUpload] uploadID=%s sorted %d parts", uploadID, len(info.parts))

	parts := make([]types.CompletedPart, len(info.parts))
	copy(parts, info.parts)
	uploadIDStr := info.uploadID
	key := info.key
	s.uploadsLock.Unlock()
	log.Printf("S3: [FinalizeUpload] uploadID=%s lock released", uploadID)

	// Complete multipart upload
	log.Printf("S3: [FinalizeUpload] uploadID=%s calling CompleteMultipartUpload with %d parts", uploadID, len(parts))
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		log.Printf("S3: [ERROR FinalizeUpload] uploadID=%s CompleteMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}
	log.Printf("S3: [FinalizeUpload] uploadID=%s CompleteMultipartUpload succeeded", uploadID)

	// Clean up tracking
	log.Printf("S3: [FinalizeUpload] uploadID=%s acquiring lock to delete from map", uploadID)
	s.uploadsLock.Lock()
	delete(s.uploads, uploadID)
	log.Printf("S3: [FinalizeUpload] uploadID=%s deleted from map, newMapSize=%d", uploadID, len(s.uploads))
	s.uploadsLock.Unlock()
	log.Printf("S3: [FinalizeUpload] uploadID=%s lock released", uploadID)

	log.Printf("S3: [SUCCESS FinalizeUpload] uploadID=%s", uploadID)
	return nil
}

func (s *S3Storage) AbortUpload(ctx context.Context, uploadID string) error {
	log.Printf("S3: [ENTER AbortUpload] uploadID=%s", uploadID)

	log.Printf("S3: [AbortUpload] uploadID=%s acquiring lock", uploadID)
	s.uploadsLock.Lock()
	log.Printf("S3: [AbortUpload] uploadID=%s lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.Unlock()
		log.Printf("S3: [AbortUpload] uploadID=%s not found in map (already cleaned up)", uploadID)
		return nil // Already cleaned up
	}
	uploadIDStr := info.uploadID
	key := info.key
	log.Printf("S3: [AbortUpload] uploadID=%s found, s3UploadID=%s key=%s, deleting from map", uploadID, uploadIDStr, key)
	delete(s.uploads, uploadID)
	log.Printf("S3: [AbortUpload] uploadID=%s deleted from map, newMapSize=%d", uploadID, len(s.uploads))
	s.uploadsLock.Unlock()
	log.Printf("S3: [AbortUpload] uploadID=%s lock released", uploadID)

	// Abort multipart upload on S3
	log.Printf("S3: [AbortUpload] uploadID=%s calling AbortMultipartUpload", uploadID)
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		log.Printf("S3: [ERROR AbortUpload] uploadID=%s AbortMultipartUpload failed: %v", uploadID, err)
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	log.Printf("S3: [SUCCESS AbortUpload] uploadID=%s", uploadID)
	return nil
}

func (s *S3Storage) GetUploadProgress(ctx context.Context, uploadID string) (int64, error) {
	log.Printf("S3: [ENTER GetUploadProgress] uploadID=%s", uploadID)

	log.Printf("S3: [GetUploadProgress] uploadID=%s acquiring read lock", uploadID)
	s.uploadsLock.RLock()
	log.Printf("S3: [GetUploadProgress] uploadID=%s read lock acquired, mapSize=%d", uploadID, len(s.uploads))
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.RUnlock()
		log.Printf("S3: [GetUploadProgress] uploadID=%s not found in map, returning 0", uploadID)
		return 0, nil
	}
	uploadIDStr := info.uploadID
	key := info.key
	log.Printf("S3: [GetUploadProgress] uploadID=%s found, s3UploadID=%s key=%s", uploadID, uploadIDStr, key)
	s.uploadsLock.RUnlock()
	log.Printf("S3: [GetUploadProgress] uploadID=%s read lock released", uploadID)

	// List uploaded parts
	log.Printf("S3: [GetUploadProgress] uploadID=%s calling ListParts", uploadID)
	result, err := s.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		log.Printf("S3: [ERROR GetUploadProgress] uploadID=%s ListParts failed: %v", uploadID, err)
		return 0, fmt.Errorf("failed to list parts: %w", err)
	}

	// Calculate total uploaded size
	var totalSize int64
	for _, part := range result.Parts {
		totalSize += *part.Size
	}
	log.Printf("S3: [GetUploadProgress] uploadID=%s calculated totalSize=%d from %d parts", uploadID, totalSize, len(result.Parts))

	// Add any buffered data that hasn't been uploaded yet
	s.uploadsLock.RLock()
	if info, exists := s.uploads[uploadID]; exists && info.buffer.Len() > 0 {
		bufferSize := int64(info.buffer.Len())
		totalSize += bufferSize
		log.Printf("S3: [GetUploadProgress] uploadID=%s adding %d buffered bytes, totalSize=%d", uploadID, bufferSize, totalSize)
	}
	s.uploadsLock.RUnlock()

	log.Printf("S3: [SUCCESS GetUploadProgress] uploadID=%s totalSize=%d", uploadID, totalSize)
	return totalSize, nil
}

// RestoreInProgressUploads queries S3 for in-progress multipart uploads and restores them to memory
// This allows the server to resume uploads after a restart
func (s *S3Storage) RestoreInProgressUploads(ctx context.Context) error {
	log.Printf("S3: [ENTER RestoreInProgressUploads] querying S3 for in-progress uploads")

	// List all in-progress multipart uploads
	result, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		log.Printf("S3: [ERROR RestoreInProgressUploads] ListMultipartUploads failed: %v", err)
		return fmt.Errorf("failed to list multipart uploads: %w", err)
	}

	log.Printf("S3: [RestoreInProgressUploads] found %d in-progress uploads", len(result.Uploads))

	s.uploadsLock.Lock()
	defer s.uploadsLock.Unlock()

	// Restore each upload to the in-memory map
	for _, upload := range result.Uploads {
		if upload.Key == nil || upload.UploadId == nil {
			continue
		}

		key := *upload.Key
		s3UploadID := *upload.UploadId

		log.Printf("S3: [RestoreInProgressUploads] restoring upload: key=%s s3UploadID=%s", key, s3UploadID)

		// Extract TUS uploadID from the S3 key
		// Key format is: {uploadID} or {uploadID}/{filename}
		uploadID := key
		if idx := strings.IndexByte(key, '/'); idx != -1 {
			uploadID = key[:idx]
			log.Printf("S3: [RestoreInProgressUploads] extracted uploadID=%s from key=%s", uploadID, key)
		} else {
			log.Printf("S3: [RestoreInProgressUploads] using key as uploadID: %s", uploadID)
		}

		// List existing parts to restore part number tracking
		partsResult, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(s3UploadID),
		})
		if err != nil {
			log.Printf("S3: [WARN RestoreInProgressUploads] failed to list parts for key=%s: %v", key, err)
			continue
		}

		// Convert parts to completed parts and calculate uploaded bytes
		completedParts := make([]types.CompletedPart, 0, len(partsResult.Parts))
		maxPartNum := int32(0)
		var uploadedBytes int64
		for _, part := range partsResult.Parts {
			if part.ETag != nil && part.PartNumber != nil {
				completedParts = append(completedParts, types.CompletedPart{
					ETag:       part.ETag,
					PartNumber: part.PartNumber,
				})
				if *part.PartNumber > maxPartNum {
					maxPartNum = *part.PartNumber
				}
				if part.Size != nil {
					uploadedBytes += *part.Size
				}
			}
		}

		s.uploads[uploadID] = &s3UploadInfo{
			uploadID:      s3UploadID,
			key:           key,
			parts:         completedParts,
			partNum:       int(maxPartNum) + 1, // Next part number
			buffer:        new(bytes.Buffer),
			totalSize:     0,             // Unknown when restoring - will be determined during upload
			uploadedBytes: uploadedBytes, // Actual bytes uploaded to S3
		}

		log.Printf("S3: [RestoreInProgressUploads] restored uploadID=%s with %d parts, nextPart=%d, uploadedBytes=%d", uploadID, len(completedParts), maxPartNum+1, uploadedBytes)
	}

	log.Printf("S3: [SUCCESS RestoreInProgressUploads] restored %d uploads, mapSize=%d", len(result.Uploads), len(s.uploads))
	return nil
}
