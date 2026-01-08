# Go Project Template

[![Go Report Card](https://goreportcard.com/badge/github.com/uug-ai/templates-go)](https://goreportcard.com/report/github.com/uug-ai/templates-go)
[![GoDoc](https://godoc.org/github.com/uug-ai/templates-go?status.svg)](https://godoc.org/github.com/uug-ai/templates-go)
[![Release](https://img.shields.io/github/release/uug-ai/templates-go.svg)](https://github.com/uug-ai/templates-go/releases/latest)

A production-ready Go project template with best practices, standardized structure, and modern development tooling.

## 🚀 Features

- **Modern Go Structure**: Follows the [Standard Go Project Layout](https://github.com/golang-standards/project-layout)
- **Docker Support**: Multi-stage Dockerfile for optimized production builds
- **Dev Container**: Pre-configured development environment with VS Code integration
- **CI/CD Ready**: GitHub Actions workflows and conventional commit guidelines
- **Extensible Architecture**: Pre-structured packages for common requirements:
  - Database connectivity (`pkg/database/`)
  - Message queue integration (`pkg/queue/`)
  - HTTP routing and middleware (`internal/router/`)
  - Audit logging and tracing (`internal/`)

## 📁 Project Structure

```
.
├── cmd/                    # Main applications for this project
├── internal/               # Private application and library code
│   ├── audit.go           # Audit logging functionality
│   ├── tracing.go         # Distributed tracing support
│   └── router/            # HTTP server and middleware
│       ├── server.go
│       └── middleware.go
├── pkg/                    # Public library code (safe to import)
│   ├── database/          # Database clients and connections
│   │   └── mongodb.go
│   └── queue/             # Message queue integrations
│       └── rabbitmq.go
├── example/               # Example applications and documentation
├── .devcontainer/         # VS Code dev container configuration
├── .github/               # GitHub workflows and templates
├── Dockerfile             # Multi-stage production build
├── go.mod                 # Go module definition
└── main.go                # Application entry point
```

## 🛠️ Getting Started

### Prerequisites

- Go 1.24+ (or use the provided dev container)
- Docker (optional, for containerized development)

### Using This Template

1. **Create a new repository from this template**:
   - Click "Use this template" on GitHub
   - Or clone directly: `git clone https://github.com/uug-ai/templates-go.git your-project`

2. **Update module name**:
   ```bash
   # Replace the module path in go.mod
   go mod edit -module github.com/yourusername/yourproject
   ```

3. **Install dependencies**:
   ```bash
   go mod download
   ```

4. **Run the application**:
   ```bash
   go run main.go
   ```

### Development with Dev Container

This template includes a complete dev container configuration:

1. Open the project in VS Code
2. Install the "Dev Containers" extension
3. Click "Reopen in Container" when prompted
4. All tools and dependencies are automatically configured

## 🏗️ Building

### Local Build

```bash
# Build for your current platform
go build -o app main.go

# Build with optimizations
go build -ldflags="-s -w" -o app main.go
```

### Docker Build

```bash
# Build the Docker image
docker build \
  --build-arg project=myapp \
  --build-arg github_username=your-username \
  --build-arg github_token=your-token \
  -t myapp:latest .

# Run the container
docker run -p 8080:8080 myapp:latest
```

## 📦 Package Overview

### `internal/`

Private application code that should not be imported by other projects:

- **`audit.go`**: Audit logging and compliance tracking
- **`tracing.go`**: OpenTelemetry or similar tracing integration
- **`router/`**: HTTP server setup and middleware stack

### `pkg/`

Public packages that can be imported by external projects:

- **`database/`**: Database connection management (MongoDB, PostgreSQL, etc.)
- **`queue/`**: Message queue clients (RabbitMQ, Kafka, etc.)

### `cmd/`

Entry points for different applications (CLIs, services, workers):

```
cmd/
├── server/      # Main HTTP server
├── worker/      # Background job processor
└── migrate/     # Database migration tool
```

## 🧪 Testing

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with verbose output
go test -v ./...
```

## 🔧 Configuration

The template supports environment-based configuration:

1. Copy `.env` to `.env.local`:
   ```bash
   cp .env .env.local
   ```

2. Update `.env.local` with your local settings (this file is gitignored)

3. Common environment variables:
   ```bash
   # Application
   APP_NAME=myapp
   APP_ENV=development
   PORT=8080
   
   # Database
   MONGODB_URI=mongodb://localhost:27017
   DATABASE_NAME=myapp
   
   # Message Queue
   RABBITMQ_URL=amqp://guest:guest@localhost:5672/
   ```

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

## 📄 License

This template is available as open source. Modify and use it for your projects.

## 🔗 Resources

- [Go Documentation](https://go.dev/doc/)
- [Standard Go Project Layout](https://github.com/golang-standards/project-layout)
- [Effective Go](https://go.dev/doc/effective_go)
- [Conventional Commits](https://www.conventionalcommits.org/)

## 💡 Tips

- Keep `internal/` for code specific to your application
- Put reusable libraries in `pkg/` if you plan to share them
- Use `cmd/` for multiple binaries (services, CLI tools, etc.)
- Leverage the dev container for consistent development environments
- Follow the commit conventions for better changelog generation

---

**Built with ❤️ by [UUG.AI](https://github.com/uug-ai)**
