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

// ProxyStore wraps a local store and streams data to remote storage
type ProxyStore struct {
	localStore    handler.DataStore
	remoteStorage RemoteStorage
	retryAttempts int
	retryDelay    time.Duration

	// Track remote upload progress
	progressLock  sync.RWMutex
	remoteOffsets map[string]int64
}

// NewProxyStore creates a new proxy store that forwards data to remote storage
func NewProxyStore(localStore handler.DataStore, remoteStorage RemoteStorage) *ProxyStore {
	return &ProxyStore{
		localStore:    localStore,
		remoteStorage: remoteStorage,
		retryAttempts: 3,
		retryDelay:    time.Second * 2,
		remoteOffsets: make(map[string]int64),
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
	// Create upload in local store first
	upload, err := ps.localStore.NewUpload(ctx, info)
	if err != nil {
		return nil, err
	}

	// Initiate upload on remote storage with retry
	err = ps.retryOperation(func() error {
		return ps.remoteStorage.InitiateUpload(ctx, info.ID, info.Size, info.MetaData)
	})

	if err != nil {
		log.Printf("Failed to initiate remote upload for %s: %v", info.ID, err)
		// Don't fail the upload, we'll retry on write
	}

	ps.progressLock.Lock()
	ps.remoteOffsets[info.ID] = 0
	ps.progressLock.Unlock()

	return &proxyUpload{
		upload:     upload,
		proxyStore: ps,
		id:         info.ID,
	}, nil
}

// GetUpload retrieves an existing upload
func (ps *ProxyStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	upload, err := ps.localStore.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}

	// Try to get remote progress
	remoteOffset, err := ps.remoteStorage.GetUploadProgress(ctx, id)
	if err != nil {
		log.Printf("Failed to get remote progress for %s: %v", id, err)
		remoteOffset = 0
	}

	ps.progressLock.Lock()
	ps.remoteOffsets[id] = remoteOffset
	ps.progressLock.Unlock()

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

// WriteChunk writes data to both local and remote storage
func (pu *proxyUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	// Use a TeeReader to write to both local and remote simultaneously
	var remoteWriter io.Writer
	var pipeReader *io.PipeReader
	var pipeWriter *io.PipeWriter

	// Create a pipe to stream data to remote storage concurrently
	pipeReader, pipeWriter = io.Pipe()
	remoteWriter = pipeWriter

	// Channel to receive remote upload result
	remoteDone := make(chan error, 1)

	// Start remote upload in background
	go func() {
		defer pipeReader.Close()

		// Retry remote write
		err := pu.proxyStore.retryOperation(func() error {
			// Note: This is a simplified version. In production, you'd need to handle
			// the case where we need to re-read the data if retry is needed
			return pu.proxyStore.remoteStorage.WriteChunk(ctx, pu.id, offset, pipeReader, -1)
		})

		if err != nil {
			log.Printf("Remote upload failed for %s at offset %d: %v", pu.id, offset, err)
		} else {
			// Update remote offset on success
			pu.proxyStore.progressLock.Lock()
			if currentOffset := pu.proxyStore.remoteOffsets[pu.id]; offset > currentOffset {
				pu.proxyStore.remoteOffsets[pu.id] = offset
			}
			pu.proxyStore.progressLock.Unlock()
		}

		remoteDone <- err
	}()

	// Write to local storage while simultaneously streaming to remote
	teeReader := io.TeeReader(src, remoteWriter)
	bytesWritten, localErr := pu.upload.WriteChunk(ctx, offset, teeReader)

	// Close the pipe writer to signal we're done writing
	pipeWriter.Close()

	// Wait for remote upload to complete
	remoteErr := <-remoteDone

	// Return local error if it occurred (takes priority)
	if localErr != nil {
		return bytesWritten, localErr
	}

	// Log remote errors but don't fail the upload
	// The data is safe in local storage and can be synced later
	if remoteErr != nil {
		log.Printf("Warning: Remote storage sync failed for %s, data is in local storage", pu.id)
	}

	return bytesWritten, nil
}

// FinishUpload completes the upload
func (pu *proxyUpload) FinishUpload(ctx context.Context) error {
	// Finish local upload first
	if err := pu.upload.FinishUpload(ctx); err != nil {
		return err
	}

	// Finalize remote upload with retry
	err := pu.proxyStore.retryOperation(func() error {
		return pu.proxyStore.remoteStorage.FinalizeUpload(ctx, pu.id)
	})

	if err != nil {
		log.Printf("Failed to finalize remote upload %s: %v", pu.id, err)
		// Don't return error - upload is complete locally
		// You might want to trigger a background sync job here
	}

	pu.proxyStore.progressLock.Lock()
	delete(pu.proxyStore.remoteOffsets, pu.id)
	pu.proxyStore.progressLock.Unlock()

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
	pu.proxyStore.progressLock.Unlock()

	// Then delete local
	if terminator, ok := pu.upload.(handler.TerminatableUpload); ok {
		return terminator.Terminate(ctx)
	}

	return nil
}
