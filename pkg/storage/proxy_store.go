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

	// Track uploads currently syncing from local to remote
	syncInProgress map[string]bool
}

// NewProxyStore creates a new proxy store that forwards data to remote storage
func NewProxyStore(localStore handler.DataStore, remoteStorage RemoteStorage) *ProxyStore {
	return &ProxyStore{
		localStore:     localStore,
		remoteStorage:  remoteStorage,
		retryAttempts:  3,
		retryDelay:     time.Second * 2,
		remoteOffsets:  make(map[string]int64),
		pendingChunks:  make(map[string][]*chunkSync),
		syncInProgress: make(map[string]bool),
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

	// After a restart, we need to handle two scenarios:
	// 1. Local has more data than remote (buffered data not yet flushed to S3)
	// 2. Remote has more data than local (server restarted, local .info file reset)

	// Check actual local file size, not just info.Offset
	// The .info file can be reset to Offset=0, but the binary data file still has content
	var localDataSize int64 = info.Offset
	reader, err := upload.GetReader(ctx)
	if err == nil {
		defer reader.Close()
		// Try to seek to end to get actual file size
		if seeker, ok := reader.(io.Seeker); ok {
			if endPos, err := seeker.Seek(0, io.SeekEnd); err == nil {
				localDataSize = endPos
				if localDataSize != info.Offset {
					log.Printf("ProxyStore: [GetUpload] detected mismatch: info.Offset=%d but actual file size=%d for %q",
						info.Offset, localDataSize, id)
				}
			}
		}
	}

	// Case 1: Local has more data than remote - sync missing data TO remote
	if localDataSize > remoteOffset {
		log.Printf("ProxyStore: [GetUpload] local data size (%d) > remote offset (%d), syncing missing data TO remote for %q",
			localDataSize, remoteOffset, id)

		// Mark this upload as syncing to block concurrent writes
		ps.progressLock.Lock()
		ps.syncInProgress[id] = true
		ps.progressLock.Unlock()
		defer func() {
			ps.progressLock.Lock()
			delete(ps.syncInProgress, id)
			ps.progressLock.Unlock()
		}()

		// Read the missing chunk from local storage
		reader, err := upload.GetReader(ctx)
		if err != nil {
			log.Printf("ProxyStore: [WARNING GetUpload] failed to get reader for syncing: %v", err)
		} else {
			defer reader.Close()

			// Seek to the remote offset
			if seeker, ok := reader.(io.Seeker); ok {
				if _, err := seeker.Seek(remoteOffset, io.SeekStart); err != nil {
					log.Printf("ProxyStore: [WARNING GetUpload] failed to seek to offset %d: %v", remoteOffset, err)
				} else {
					// Sync the missing chunk
					missingSize := localDataSize - remoteOffset
					log.Printf("ProxyStore: [GetUpload] syncing %d bytes from offset %d to %d", missingSize, remoteOffset, localDataSize)

					err := ps.retryOperation(func() error {
						// Re-open reader for each retry attempt
						retryReader, err := upload.GetReader(ctx)
						if err != nil {
							return fmt.Errorf("failed to get reader: %w", err)
						}
						defer retryReader.Close()

						if seeker, ok := retryReader.(io.Seeker); ok {
							if _, err := seeker.Seek(remoteOffset, io.SeekStart); err != nil {
								return fmt.Errorf("failed to seek to offset %d: %w", remoteOffset, err)
							}
						}

						limitedReader := io.LimitReader(retryReader, missingSize)
						return ps.remoteStorage.WriteChunk(ctx, id, remoteOffset, limitedReader, missingSize)
					})

					if err != nil {
						log.Printf("ProxyStore: [WARNING GetUpload] failed to sync missing data TO remote: %v", err)
					} else {
						log.Printf("ProxyStore: [GetUpload] successfully synced missing data TO remote for %q", id)
						ps.progressLock.Lock()
						ps.remoteOffsets[id] = localDataSize
						ps.progressLock.Unlock()
					}
				}
			}
		}
	}

	// Case 2: Remote ahead of local - this happens after server restart when .info file is reset
	// We need to tell the client the actual remote offset so it doesn't resend already-uploaded data
	if remoteOffset > localDataSize {
		log.Printf("ProxyStore: [GetUpload] remote offset (%d) > local file size (%d) for %q",
			remoteOffset, localDataSize, id)
		log.Printf("ProxyStore: [GetUpload] WARNING: Local file data lost. Client should resume from offset %d", remoteOffset)
	}

	log.Printf("ProxyStore: [SUCCESS GetUpload] returning proxyUpload with id=%q localOffset=%d localFileSize=%d remoteOffset=%d",
		id, info.Offset, localDataSize, remoteOffset)
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

	// Wait if a sync operation is in progress (from GetUpload)
	for {
		pu.proxyStore.progressLock.RLock()
		syncing := pu.proxyStore.syncInProgress[pu.id]
		pu.proxyStore.progressLock.RUnlock()
		if !syncing {
			break
		}
		log.Printf("ProxyUpload: [WriteChunk] waiting for sync to complete for id=%q", pu.id)
		time.Sleep(100 * time.Millisecond)
	}

	// Validate offset against remote progress to prevent gaps after server restart
	pu.proxyStore.progressLock.RLock()
	remoteOffset := pu.proxyStore.remoteOffsets[pu.id]
	pu.proxyStore.progressLock.RUnlock()

	// The offset must match either:
	// 1. Local offset (normal case), or
	// 2. Remote offset (after restart when local is behind)
	// We can't accept writes that would create gaps
	localInfo, err := pu.upload.GetInfo(ctx)
	if err != nil {
		log.Printf("ProxyUpload: [ERROR WriteChunk] failed to get upload info: %v", err)
		return 0, err
	}

	expectedOffset := localInfo.Offset
	if remoteOffset > expectedOffset {
		expectedOffset = remoteOffset
	}

	if offset != expectedOffset {
		log.Printf("ProxyUpload: [ERROR WriteChunk] offset mismatch for id=%q: expected=%d got=%d (local=%d remote=%d)",
			pu.id, expectedOffset, offset, localInfo.Offset, remoteOffset)
		return 0, fmt.Errorf("offset mismatch: expected %d, got %d", expectedOffset, offset)
	}

	// Special case: if remote is ahead of local (after restart), we need to skip local write
	// because the data is already in S3, and writing it to local at the wrong offset creates corruption
	if remoteOffset > localInfo.Offset && offset >= localInfo.Offset && offset < remoteOffset {
		log.Printf("ProxyUpload: [WriteChunk] skipping redundant write for id=%q at offset=%d (already in S3, remote=%d, local=%d)",
			pu.id, offset, remoteOffset, localInfo.Offset)
		// Read and discard the data
		n, err := io.Copy(io.Discard, src)
		return n, err
	}

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
	info, err := pu.upload.GetInfo(ctx)
	if err != nil {
		return info, err
	}

	// After a server restart, the local .info file may show Offset=0
	// but the remote storage may have partial data already uploaded
	// We need to return the maximum offset to the client
	pu.proxyStore.progressLock.RLock()
	remoteOffset := pu.proxyStore.remoteOffsets[pu.id]
	pu.proxyStore.progressLock.RUnlock()

	// Use the maximum of local and remote offsets
	// This ensures the client knows the true upload progress after a restart
	if remoteOffset > info.Offset {
		log.Printf("ProxyUpload: [GetInfo] adjusting offset for %q from local=%d to remote=%d",
			pu.id, info.Offset, remoteOffset)
		info.Offset = remoteOffset
	}

	return info, nil
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
