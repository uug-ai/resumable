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
	parts    []types.CompletedPart
	partNum  int
}

// NewS3RemoteStorageReal creates a new S3 remote storage
func NewS3RemoteStorageReal(bucket, region, endpoint, accessKey, secretKey string) (*S3RemoteStorageReal, error) {
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

	return &S3RemoteStorageReal{
		client: client,
	}, nil
}

func (s *S3RemoteStorageReal) InitiateUpload(ctx context.Context, uploadID string, size int64, metadata map[string]string) error {
	// Create multipart upload
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(uploadID),
		Metadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("failed to initiate multipart upload: %w", err)
	}

	s.uploadsLock.Lock()
	s.uploads[uploadID] = &s3UploadInfo{
		uploadID: *result.UploadId,
		parts:    make([]types.CompletedPart, 0),
		partNum:  1,
	}
	s.uploadsLock.Unlock()

	log.Printf("S3: Initiated multipart upload %s for key %s", *result.UploadId, uploadID)
	return nil
}

func (s *S3RemoteStorageReal) WriteChunk(ctx context.Context, uploadID string, offset int64, data io.Reader, size int64) error {
	s.uploadsLock.Lock()
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.Unlock()
		return fmt.Errorf("upload %s not found", uploadID)
	}
	partNumber := int32(info.partNum)
	info.partNum++
	s.uploadsLock.Unlock()

	// Read all data into buffer (required for S3 SDK)
	buf := new(bytes.Buffer)
	written, err := io.Copy(buf, data)
	if err != nil {
		return fmt.Errorf("failed to read chunk data: %w", err)
	}

	// Upload part to S3
	result, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
		UploadId:   aws.String(info.uploadID),
		Body:       bytes.NewReader(buf.Bytes()),
	})
	if err != nil {
		return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
	}

	// Store completed part info
	s.uploadsLock.Lock()
	info.parts = append(info.parts, types.CompletedPart{
		ETag:       result.ETag,
		PartNumber: aws.Int32(partNumber),
	})
	s.uploadsLock.Unlock()

	log.Printf("S3: Uploaded part %d (%d bytes) for %s", partNumber, written, uploadID)
	return nil
}

func (s *S3RemoteStorageReal) FinalizeUpload(ctx context.Context, uploadID string) error {
	s.uploadsLock.Lock()
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.Unlock()
		return fmt.Errorf("upload %s not found", uploadID)
	}

	// Sort parts by part number (required by S3)
	sort.Slice(info.parts, func(i, j int) bool {
		return *info.parts[i].PartNumber < *info.parts[j].PartNumber
	})

	parts := make([]types.CompletedPart, len(info.parts))
	copy(parts, info.parts)
	uploadIDStr := info.uploadID
	s.uploadsLock.Unlock()

	// Complete multipart upload
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(uploadID),
		UploadId: aws.String(uploadIDStr),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	// Clean up tracking
	s.uploadsLock.Lock()
	delete(s.uploads, uploadID)
	s.uploadsLock.Unlock()

	log.Printf("S3: Completed multipart upload for %s", uploadID)
	return nil
}

func (s *S3RemoteStorageReal) AbortUpload(ctx context.Context, uploadID string) error {
	s.uploadsLock.Lock()
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.Unlock()
		return nil // Already cleaned up
	}
	uploadIDStr := info.uploadID
	delete(s.uploads, uploadID)
	s.uploadsLock.Unlock()

	// Abort multipart upload on S3
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(uploadID),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	log.Printf("S3: Aborted multipart upload for %s", uploadID)
	return nil
}

func (s *S3RemoteStorageReal) GetUploadProgress(ctx context.Context, uploadID string) (int64, error) {
	s.uploadsLock.RLock()
	info, exists := s.uploads[uploadID]
	if !exists {
		s.uploadsLock.RUnlock()
		return 0, nil
	}
	uploadIDStr := info.uploadID
	s.uploadsLock.RUnlock()

	// List uploaded parts
	result, err := s.client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(uploadID),
		UploadId: aws.String(uploadIDStr),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list parts: %w", err)
	}

	// Calculate total uploaded size
	var totalSize int64
	for _, part := range result.Parts {
		totalSize += *part.Size
	}

	return totalSize, nil
}
