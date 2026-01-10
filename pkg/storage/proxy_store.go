package storage

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

// RemoteStorage is an interface for remote storage providers (S3, Azure, GCS, etc.)
type RemoteStorage interface {
	// InitiateUpload starts a new upload session on the remote storage
	InitiateUpload(ctx context.Context, uploadID string, size int64, metadata map[string]string) error

	// WriteChunk writes a chunk of data to the remote storage
	// offset is the byte position where this chunk starts
	WriteChunk(ctx context.Context, uploadID string, offset int64, data io.Reader, size int64) error

	// FinalizeUpload completes the upload on the remote storage
	FinalizeUpload(ctx context.Context, uploadID string) error

	// AbortUpload cancels the upload on the remote storage
	AbortUpload(ctx context.Context, uploadID string) error

	// GetUploadProgress returns the current offset on the remote storage
	GetUploadProgress(ctx context.Context, uploadID string) (int64, error)
}

// chunkSync tracks the sync status of a chunk
type chunkSync struct {
	offset int64
	size   int64
	done   chan error // nil when successful, error when failed
}

// ProxyStore wraps a local store and streams data to remote storage
type ProxyStore struct {
	localStore    handler.DataStore
	remoteStorage RemoteStorage
	retryAttempts int
	retryDelay    time.Duration

	// Track remote upload progress
	progressLock  sync.RWMutex
	remoteOffsets map[string]int64

	// Track pending chunk uploads
	pendingChunks map[string][]*chunkSync
}

// NewProxyStore creates a new proxy store that forwards data to remote storage
func NewProxyStore(localStore handler.DataStore, remoteStorage RemoteStorage) *ProxyStore {
	return &ProxyStore{
		localStore:    localStore,
		remoteStorage: remoteStorage,
		retryAttempts: 3,
		retryDelay:    time.Second * 2,
		remoteOffsets: make(map[string]int64),
		pendingChunks: make(map[string][]*chunkSync),
	}
}

// UseIn registers the ProxyStore in a StoreComposer
func (ps *ProxyStore) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(ps)
	composer.UseTerminater(ps)
	composer.UseLengthDeferrer(ps)

	// If the underlying store supports locker, expose it
	if locker, ok := ps.localStore.(handler.Locker); ok {
		composer.UseLocker(locker)
	}
}

// NewUpload creates a new upload in both local and remote storage
func (ps *ProxyStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	log.Printf("ProxyStore: [ENTER NewUpload] info.ID=%q (len=%d) info.Size=%d", info.ID, len(info.ID), info.Size)

	// Create upload in local store first
	upload, err := ps.localStore.NewUpload(ctx, info)
	if err != nil {
		log.Printf("ProxyStore: [ERROR NewUpload] local store failed for ID=%q: %v", info.ID, err)
		return nil, err
	}

	// Get the actual upload ID from the created upload (local store generates it)
	uploadInfo, err := upload.GetInfo(ctx)
	if err != nil {
		log.Printf("ProxyStore: [ERROR NewUpload] failed to get upload info: %v", err)
		return nil, err
	}
	actualID := uploadInfo.ID
	log.Printf("ProxyStore: [NewUpload] local upload created with ID=%q", actualID)

	// Initiate upload on remote storage with retry using the actual ID
	log.Printf("ProxyStore: [NewUpload] initiating remote upload for ID=%q", actualID)
	err = ps.retryOperation(func() error {
		return ps.remoteStorage.InitiateUpload(ctx, actualID, uploadInfo.Size, uploadInfo.MetaData)
	})

	if err != nil {
		log.Printf("ProxyStore: [WARNING NewUpload] Failed to initiate remote upload for %q: %v", actualID, err)
		// Don't fail the upload, we'll retry on write
	} else {
		log.Printf("ProxyStore: [NewUpload] remote upload initiated successfully for ID=%q", actualID)
	}

	ps.progressLock.Lock()
	ps.remoteOffsets[actualID] = 0
	ps.progressLock.Unlock()

	log.Printf("ProxyStore: [SUCCESS NewUpload] returning proxyUpload with id=%q", actualID)
	return &proxyUpload{
		upload:     upload,
		proxyStore: ps,
		id:         actualID,
	}, nil
}

// GetUpload retrieves an existing upload
func (ps *ProxyStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	log.Printf("ProxyStore: [ENTER GetUpload] id=%q (len=%d)", id, len(id))

	upload, err := ps.localStore.GetUpload(ctx, id)
	if err != nil {
		log.Printf("ProxyStore: [ERROR GetUpload] local store failed for ID=%q: %v", id, err)
		return nil, err
	}

	// Get upload info to check if we need to initiate remote upload
	info, err := upload.GetInfo(ctx)
	if err != nil {
		log.Printf("ProxyStore: [ERROR GetUpload] failed to get upload info for ID=%q: %v", id, err)
		return nil, err
	}

	// Try to get remote progress
	remoteOffset, err := ps.remoteStorage.GetUploadProgress(ctx, id)
	if err != nil {
		log.Printf("ProxyStore: [GetUpload] failed to get remote progress for %q: %v (will initiate)", id, err)
		remoteOffset = 0

		// If remote upload doesn't exist, initiate it now
		// This handles the case where an upload is resumed but remote upload was never initiated
		log.Printf("ProxyStore: [GetUpload] initiating remote upload for resumed upload %q", id)
		err = ps.retryOperation(func() error {
			return ps.remoteStorage.InitiateUpload(ctx, info.ID, info.Size, info.MetaData)
		})
		if err != nil {
			log.Printf("ProxyStore: [WARNING GetUpload] failed to initiate remote upload for %q: %v", id, err)
			// Don't fail the upload, we'll retry on write
		} else {
			log.Printf("ProxyStore: [GetUpload] remote upload initiated successfully for resumed upload %q", id)
		}
	} else {
		log.Printf("ProxyStore: [GetUpload] remote upload exists for %q, offset=%d", id, remoteOffset)
	}

	ps.progressLock.Lock()
	ps.remoteOffsets[id] = remoteOffset
	ps.progressLock.Unlock()

	log.Printf("ProxyStore: [SUCCESS GetUpload] returning proxyUpload with id=%q", id)
	return &proxyUpload{
		upload:     upload,
		proxyStore: ps,
		id:         id,
	}, nil
}

// AsTerminatableUpload wraps an upload for termination
func (ps *ProxyStore) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	if pu, ok := upload.(*proxyUpload); ok {
		return pu
	}
	// Fallback to local store's implementation
	if terminator, ok := ps.localStore.(handler.TerminaterDataStore); ok {
		return terminator.AsTerminatableUpload(upload)
	}
	return nil
}

// AsLengthDeclarableUpload wraps an upload for length declaration
func (ps *ProxyStore) AsLengthDeclarableUpload(upload handler.Upload) handler.LengthDeclarableUpload {
	if pu, ok := upload.(*proxyUpload); ok {
		return pu
	}
	// Fallback to local store's implementation
	if deferrer, ok := ps.localStore.(handler.LengthDeferrerDataStore); ok {
		return deferrer.AsLengthDeclarableUpload(upload)
	}
	return nil
}

// retryOperation executes an operation with exponential backoff retry
func (ps *ProxyStore) retryOperation(operation func() error) error {
	var err error
	for attempt := 0; attempt <= ps.retryAttempts; attempt++ {
		err = operation()
		if err == nil {
			return nil
		}

		if attempt < ps.retryAttempts {
			delay := ps.retryDelay * time.Duration(1<<uint(attempt))
			log.Printf("Operation failed (attempt %d/%d): %v. Retrying in %v...",
				attempt+1, ps.retryAttempts, err, delay)
			time.Sleep(delay)
		}
	}
	return fmt.Errorf("operation failed after %d attempts: %w", ps.retryAttempts, err)
}

// proxyUpload wraps an upload and streams data to remote storage
type proxyUpload struct {
	upload     handler.Upload
	proxyStore *ProxyStore
	id         string
}

// WriteChunk writes data to local storage first, then sends to remote storage
func (pu *proxyUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	log.Printf("ProxyUpload: [ENTER WriteChunk] id=%q (len=%d) offset=%d", pu.id, len(pu.id), offset)

	// Step 1: Write to local storage first to ensure data is persisted
	bytesWritten, localErr := pu.upload.WriteChunk(ctx, offset, src)
	if localErr != nil {
		return bytesWritten, localErr
	}

	// Step 2: Track this chunk and upload to remote asynchronously
	chunk := &chunkSync{
		offset: offset,
		size:   bytesWritten,
		done:   make(chan error, 1),
	}

	// Register the pending chunk
	pu.proxyStore.progressLock.Lock()
	pu.proxyStore.pendingChunks[pu.id] = append(pu.proxyStore.pendingChunks[pu.id], chunk)
	pu.proxyStore.progressLock.Unlock()

	// Upload to remote storage asynchronously
	go func() {
		log.Printf("ProxyUpload: [WriteChunk goroutine] starting remote upload for id=%q offset=%d size=%d", pu.id, offset, bytesWritten)

		// Use a background context for async operations to avoid cancelled context issues
		asyncCtx := context.Background()

		// Upload to remote storage with retry
		// For each retry attempt, we re-read the chunk from local storage
		err := pu.proxyStore.retryOperation(func() error {
			// Get a fresh reader from local storage for this attempt
			reader, err := pu.upload.GetReader(asyncCtx)
			if err != nil {
				return fmt.Errorf("failed to get reader: %w", err)
			}
			defer reader.Close()

			// Seek to the offset of the chunk we just wrote
			if seeker, ok := reader.(io.Seeker); ok {
				if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
					return fmt.Errorf("failed to seek to offset %d: %w", offset, err)
				}
			}

			// Create a limited reader to only read the exact chunk we just wrote
			chunkReader := io.LimitReader(reader, bytesWritten)

			// Send this specific chunk to remote storage
			log.Printf("ProxyUpload: [WriteChunk goroutine] calling remoteStorage.WriteChunk with id=%q (len=%d) offset=%d size=%d", pu.id, len(pu.id), offset, bytesWritten)
			return pu.proxyStore.remoteStorage.WriteChunk(asyncCtx, pu.id, offset, chunkReader, bytesWritten)
		})

		if err != nil {
			log.Printf("Remote upload failed for %s at offset %d (size %d): %v", pu.id, offset, bytesWritten, err)
		} else {
			// Update remote offset on success
			pu.proxyStore.progressLock.Lock()
			newOffset := offset + bytesWritten
			if currentOffset := pu.proxyStore.remoteOffsets[pu.id]; newOffset > currentOffset {
				pu.proxyStore.remoteOffsets[pu.id] = newOffset
			}
			pu.proxyStore.progressLock.Unlock()
			log.Printf("Successfully synced chunk to remote for %s: offset %d, size %d", pu.id, offset, bytesWritten)
		}

		// Signal completion (success or failure)
		chunk.done <- err
	}()

	// Return success immediately after local write completes
	// Remote upload happens asynchronously in background
	return bytesWritten, nil
}

// FinishUpload completes the upload
func (pu *proxyUpload) FinishUpload(ctx context.Context) error {
	// Finish local upload first
	if err := pu.upload.FinishUpload(ctx); err != nil {
		return err
	}

	// Wait for all pending chunks to complete and collect failures
	pu.proxyStore.progressLock.Lock()
	pendingChunks := pu.proxyStore.pendingChunks[pu.id]
	pu.proxyStore.progressLock.Unlock()

	log.Printf("Waiting for %d pending chunks to sync for upload %s", len(pendingChunks), pu.id)

	var failedChunks []*chunkSync
	for _, chunk := range pendingChunks {
		err := <-chunk.done
		if err != nil {
			failedChunks = append(failedChunks, chunk)
		}
	}

	// Retry any failed chunks synchronously before finalizing
	if len(failedChunks) > 0 {
		log.Printf("Retrying %d failed chunks for upload %s", len(failedChunks), pu.id)
		for _, chunk := range failedChunks {
			err := pu.proxyStore.retryOperation(func() error {
				reader, err := pu.upload.GetReader(ctx)
				if err != nil {
					return fmt.Errorf("failed to get reader: %w", err)
				}
				defer reader.Close()

				if seeker, ok := reader.(io.Seeker); ok {
					if _, err := seeker.Seek(chunk.offset, io.SeekStart); err != nil {
						return fmt.Errorf("failed to seek to offset %d: %w", chunk.offset, err)
					}
				}

				chunkReader := io.LimitReader(reader, chunk.size)
				return pu.proxyStore.remoteStorage.WriteChunk(ctx, pu.id, chunk.offset, chunkReader, chunk.size)
			})

			if err != nil {
				return fmt.Errorf("failed to sync chunk at offset %d to remote storage: %w", chunk.offset, err)
			}
			log.Printf("Successfully retried chunk at offset %d for upload %s", chunk.offset, pu.id)
		}
	}

	log.Printf("All chunks synced for upload %s, finalizing remote upload", pu.id)

	// Finalize remote upload with retry
	err := pu.proxyStore.retryOperation(func() error {
		return pu.proxyStore.remoteStorage.FinalizeUpload(ctx, pu.id)
	})

	if err != nil {
		return fmt.Errorf("failed to finalize remote upload: %w", err)
	}

	// Clean up tracking data
	pu.proxyStore.progressLock.Lock()
	delete(pu.proxyStore.remoteOffsets, pu.id)
	delete(pu.proxyStore.pendingChunks, pu.id)
	pu.proxyStore.progressLock.Unlock()

	log.Printf("Upload %s completed successfully on both local and remote storage", pu.id)
	return nil
}

// GetInfo returns upload information
func (pu *proxyUpload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	return pu.upload.GetInfo(ctx)
}

// GetReader returns a reader for the upload data
func (pu *proxyUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return pu.upload.GetReader(ctx)
}

// DeclareLength implements LengthDeclarableUpload
func (pu *proxyUpload) DeclareLength(ctx context.Context, length int64) error {
	if lengthDeferrer, ok := pu.upload.(handler.LengthDeclarableUpload); ok {
		return lengthDeferrer.DeclareLength(ctx, length)
	}
	return handler.ErrNotImplemented
}

// Terminate implements TerminatableUpload
func (pu *proxyUpload) Terminate(ctx context.Context) error {
	// Abort remote upload first
	if err := pu.proxyStore.remoteStorage.AbortUpload(ctx, pu.id); err != nil {
		log.Printf("Failed to abort remote upload %s: %v", pu.id, err)
	}

	pu.proxyStore.progressLock.Lock()
	delete(pu.proxyStore.remoteOffsets, pu.id)
	delete(pu.proxyStore.pendingChunks, pu.id)
	pu.proxyStore.progressLock.Unlock()

	// Then delete local
	if terminator, ok := pu.upload.(handler.TerminatableUpload); ok {
		return terminator.Terminate(ctx)
	}

	return nil
}
