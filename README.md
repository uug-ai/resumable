# Resumable Upload Server

[![Go Report Card](https://goreportcard.com/badge/github.com/uug-ai/resumable)](https://goreportcard.com/report/github.com/uug-ai/resumable)
[![GoDoc](https://godoc.org/github.com/uug-ai/resumable?status.svg)](https://godoc.org/github.com/uug-ai/resumable)
[![Release](https://img.shields.io/github/release/uug-ai/resumable.svg)](https://github.com/uug-ai/resumable/releases/latest)

A production-ready Go server for resumable file uploads with automatic streaming to remote storage (S3, Azure, GCS). Built on the [TUS protocol](https://tus.io/) for reliable, resumable uploads that can survive network interruptions.

## 🚀 Features

- **Resumable Uploads**: Built on TUS protocol - uploads automatically resume after network failures
- **Real-time Streaming**: Data streams directly to remote storage (S3, Azure, GCS) as it's received
- **Local Caching**: Maintains local copies for resumability and fast recovery
- **Automatic Retry**: Built-in retry logic for transient remote storage failures
- **Production Ready**: Docker support, dev container, and structured error handling
- **Multiple Storage Backends**: Easily extensible to support various cloud storage providers

## 📁 Project Structure

```
.
├── cmd/
│   └── client_upload.go   # Example TUS client for testing uploads
├── internal/
│   ├── handler/           # Upload event handlers
│   └── router/            # HTTP routing and middleware
├── pkg/
│   └── storage/           # Storage backends
│       ├── proxy_store.go # Streaming proxy to remote storage
│       ├── s3.go          # AWS S3 storage implementation
│       ├── s3_mock.go     # S3 mock for testing
│       └── s3_test.go     # Storage tests
├── uploads/               # Local file storage (for resumability)
├── examples/              # Example configurations and usage
├── docs/                  # Documentation
├── .devcontainer/         # VS Code dev container configuration
├── Dockerfile             # Multi-stage production build
├── go.mod                 # Go module definition
└── main.go                # Server entry point
```

## 🛠️ Getting Started

### Prerequisites

- Go 1.24+ (or use the provided dev container)
- AWS S3 bucket or compatible storage (MinIO, LocalStack, etc.)
- Docker (optional, for containerized development)

### Quick Start

1. **Clone the repository**:
   ```bash
   git clone https://github.com/uug-ai/resumable.git
   cd resumable
   ```

2. **Set environment variables**:
   ```bash
   export AWS_S3_BUCKET=your-bucket-name
   export AWS_S3_REGION=us-east-1
   export AWS_ACCESS_KEY_ID=your-access-key
   export AWS_SECRET_ACCESS_KEY=your-secret-key
   # Optional: for S3-compatible services (MinIO, etc.)
   export AWS_S3_ENDPOINT=https://your-endpoint.com
   ```

3. **Install dependencies**:
   ```bash
   go mod download
   ```

4. **Run the server**:
   ```bash
   go run main.go
   ```

   The server will start on `http://localhost:8080/files/`

5. **Test with the example client**:
   ```bash
   # In another terminal
   go run cmd/client_upload.go
   ```

### Development with Dev Container

This project includes a complete dev container configuration:

1. Open the project in VS Code
2. Install the "Dev Containers" extension
3. Click "Reopen in Container" when prompted
4. All tools (Go, Git, AWS CLI) are automatically configured

## 🔧 Configuration

### Environment Variables

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `AWS_S3_BUCKET` | Yes | S3 bucket name | - |
| `AWS_ACCESS_KEY_ID` | Yes | AWS access key | - |
| `AWS_SECRET_ACCESS_KEY` | Yes | AWS secret key | - |
| `AWS_S3_REGION` | No | AWS region | `us-east-1` |
| `AWS_S3_ENDPOINT` | No | Custom S3 endpoint (for MinIO, etc.) | AWS default |

### Storage Configuration

The server uses a two-tier storage approach:

1. **Local Storage** (`./uploads/`): Stores files locally for resumability
2. **Remote Storage** (S3): Streams data to remote storage in real-time

You can configure:
- Upload directory location
- Retry attempts and delays
- Storage backend (S3, Azure, GCS - extensible)

## 📡 API Usage

### Upload a File with TUS

The server implements the full [TUS protocol](https://tus.io/protocols/resumable-upload). Any TUS client can be used.

**Using cURL:**

```bash
# Create upload
curl -X POST http://localhost:8080/files/ \
  -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Length: 1000000" \
  -H "Upload-Metadata: filename dmlkZW8ubXA0,filetype dmlkZW8vbXA0"

# Upload data (returns Location header with upload URL)
curl -X PATCH http://localhost:8080/files/{upload-id} \
  -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Offset: 0" \
  -H "Content-Type: application/offset+octet-stream" \
  --data-binary @video.mp4
```

**Using the included Go client:**

```go
package main

import (
    "github.com/bdragon300/tusgo"
    "net/http"
    "net/url"
)

func main() {
    baseURL, _ := url.Parse("http://localhost:8080/files/")
    client := tusgo.NewClient(http.DefaultClient, baseURL)
    
    // Upload file
    upload := tusgo.Upload{}
    upload, err := client.CreateUpload(context.Background(), file, metadata, false)
    // ... handle upload
}
```

See [cmd/client_upload.go](cmd/client_upload.go) for a complete example.

## 🔍 How It Works

1. **Client initiates upload**: TUS client creates an upload with file metadata
2. **Local storage**: Server creates local file in `./uploads/` for resumability
3. **Remote streaming**: As chunks arrive, they're immediately streamed to S3
4. **Progress tracking**: Server tracks both local and remote upload progress
5. **Automatic retry**: If remote upload fails, chunks are retried automatically
6. **Resume capability**: If connection drops, client can resume from last successful offset
7. **Completion**: When upload completes, server notifies and optionally cleans local file

## 🏗️ Building

### Local Build

```bash
# Build for your current platform
go build -o resumable-server main.go

# Build with optimizations
go build -ldflags="-s -w" -o resumable-server main.go

# Run the built binary
./resumable-server
```

### Docker Build

```bash
# Build the Docker image
docker build -t resumable-server:latest .

# Run the container with environment variables
docker run -p 8080:8080 \
  -e AWS_S3_BUCKET=your-bucket \
  -e AWS_ACCESS_KEY_ID=your-key \
  -e AWS_SECRET_ACCESS_KEY=your-secret \
  -v $(pwd)/uploads:/app/uploads \
  resumable-server:latest
```

## 🧪 Testing

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with verbose output
go test -v ./...

# Test specific package
go test ./pkg/storage/...
```

## 🔌 Extending Storage Backends

The server is designed to support multiple storage backends. To add a new backend:

1. Implement the `RemoteStorage` interface in `pkg/storage/`:

```go
type RemoteStorage interface {
    InitiateUpload(ctx context.Context, uploadID string, size int64, metadata map[string]string) error
    WriteChunk(ctx context.Context, uploadID string, offset int64, data io.Reader, size int64) error
    FinalizeUpload(ctx context.Context, uploadID string) error
    AbortUpload(ctx context.Context, uploadID string) error
    GetUploadProgress(ctx context.Context, uploadID string) (int64, error)
}
```

2. Create your storage implementation (e.g., `pkg/storage/azure.go`)
3. Update `main.go` to use your new backend

Examples:
- [pkg/storage/s3.go](pkg/storage/s3.go) - AWS S3 implementation
- [pkg/storage/s3_mock.go](pkg/storage/s3_mock.go) - Mock for testing

## 📦 Package Overview

### `pkg/storage/`

Storage backend implementations and interfaces:

- **`proxy_store.go`**: Core streaming proxy that manages local+remote storage
- **`s3.go`**: AWS S3 storage implementation with multipart uploads
- **`s3_mock.go`**: Mock storage for testing
- **`s3_test.go`**: Storage backend tests

### `internal/`

Private application code:

- **`handler/`**: Upload event handlers and processing
- **`router/`**: HTTP server setup and middleware

### `cmd/`

Command-line tools and utilities:

- **`client_upload.go`**: Example TUS client for testing uploads

## 📝 Commit Guidelines

This project follows [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

**Types**: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`

**Examples**:
```
feat(api): add user authentication endpoint
fix(database): resolve connection pooling issue
docs(readme): update installation instructions
```

See [.github/copilot-instructions.md](.github/copilot-instructions.md) for detailed guidelines.

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/amazing-feature`
3. Commit your changes: `git commit -m 'feat: add amazing feature'`
4. Push to the branch: `git push origin feat/amazing-feature`
5. Open a Pull Request

## � Resources

- [TUS Protocol Documentation](https://tus.io/protocols/resumable-upload)
- [tusd - TUS Server Reference Implementation](https://github.com/tus/tusd)
- [Go Documentation](https://go.dev/doc/)
- [AWS S3 Go SDK](https://docs.aws.amazon.com/sdk-for-go/)
- [Conventional Commits](https://www.conventionalcommits.org/)

## 🎯 Use Cases

- **Video Upload Platforms**: Large video file uploads with resume capability
- **Media Asset Management**: Reliable uploads for media production workflows
- **Backup Systems**: Resumable backup uploads to cloud storage
- **File Sharing Services**: User file uploads with automatic cloud sync
- **Content Delivery**: Upload and distribute large files reliably

## 🐛 Troubleshooting

### Upload fails immediately

- Verify AWS credentials are correct
- Check S3 bucket exists and you have write permissions
- Ensure bucket region matches `AWS_S3_REGION`

### Upload succeeds locally but not to S3

- Check S3 endpoint connectivity
- Verify IAM permissions for multipart uploads
- Review server logs for detailed error messages

### Uploads are slow

- Check network bandwidth to S3
- Consider using S3 Transfer Acceleration
- Adjust chunk size if needed (default is optimized for most cases)

## 📄 License

This project is available as open source under the MIT License.

---

**Built with ❤️ by [UUG.AI](https://github.com/uug-ai)**
