# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common Development Commands

### Building and Running
- `make build` - Build the achgateway binary to `bin/achgateway`
- `make run` - Build and run achgateway locally
- `make docker` - Build Docker image with current version
- `make dev-docker` - Build Docker image with dev version

### Testing and Quality
- `make test` - Run all tests with coverage
- `make check` - Run linting, formatting, and quality checks (downloads lint-project.sh if needed)
- Uses golangci-lint with prealloc linter and 44.5% coverage threshold

### Development Environment
- `make setup` - Start development services with docker-compose
- `make teardown` - Stop and remove development containers
- `docker compose up achgateway` - Start achgateway with dependencies

### Package Management
- `make install` - Run `go mod tidy` and `go mod vendor`
- `make update` - Update vendor dependencies

## Architecture Overview

ACHGateway is a distributed, fault-tolerant system for processing ACH (Automated Clearing House) files. The architecture follows a pipeline pattern with these key components:

### Core Components

**Environment & Service Management** (`internal/environment.go`, `internal/service/`)
- Centralized configuration and dependency injection
- Service lifecycle management with graceful shutdown
- Admin server for operational endpoints

**File Pipeline** (`internal/pipeline/`)
- File ingestion via HTTP API (`internal/incoming/web/`) and Kafka streams (`internal/incoming/stream/`)
- File aggregation and merging logic
- Scheduled processing at cutoff times
- Event emission for external systems

**Shard Management** (`internal/shards/`)
- Multi-tenant file organization
- Configurable routing rules for file distribution
- Support for multiple ODFI (Originating Depository Financial Institution) configurations

**Upload & Transfer** (`internal/upload/`)
- Multiple transfer protocols: FTP, SFTP
- File transformation and encryption (`internal/transform/`)
- Retry logic and network security controls
- Template-based filename generation

**Inbound Processing** (`internal/incoming/odfi/`)
- Download and process return files, corrections, and reconciliation data
- Automated cleanup of processed files
- Support for prenotes and IAT (International ACH Transaction) files

### Data Flow
1. Files submitted via HTTP API (`/shards/{shardName}/files/{fileID}`) or Kafka
2. Files stored and queued in shard-specific directories
3. Cutoff time triggers file merging and upload to ODFI
4. Inbound processor downloads responses from ODFI
5. Events emitted throughout for external system integration

### Storage Options
- Local filesystem
- Cloud blob storage (AWS S3, Google Cloud Storage, Azure Blob)
- Configurable encryption at rest

### Database Support
- MySQL for persistent state
- Google Spanner for cloud deployments
- In-memory repositories for testing

## Configuration

- Main config: `configs/config.default.yml`
- Docker config: `configs/config.docker.yml`
- Environment-specific settings via YAML or environment variables
- Configuration models in `internal/service/model_*.go`

## Testing Approach

- Unit tests alongside source files (`*_test.go`)
- Integration tests in `internal/test/` and `pkg/test/`
- Test data in `testdata/` directories
- Mock implementations for external dependencies
- Docker-based testing with `examples/getting-started/`

## Key API Endpoints

- `POST /shards/{shardName}/files/{fileID}` - Submit ACH file (synchronous persistence)
- `PUT /trigger-cutoff` - Manual cutoff processing
- `PUT /trigger-inbound` - Manual inbound file processing
- Admin endpoints on port 9494, public API on port 8484

## File Persistence Architecture

### Synchronous HTTP Persistence (Reliable)
ACH file submissions via HTTP API use **synchronous persistence** to ensure data durability:

1. **HTTP Request** → `CreateFileHandler` validates ACH file format
2. **Database Persistence** → Synchronous `fileRepository.Record()` with timeout and retry logic
3. **HTTP 200 Response** → Only returned after successful database persistence
4. **Queue Publishing** → Asynchronous publishing for downstream processing (failure doesn't affect response)

**Benefits:**
- HTTP 200 response guarantees file is durably stored
- Database timeouts return HTTP 500 instead of silent failure
- Retry logic handles transient database failures automatically
- Duplicate submissions are handled gracefully

**Configuration:**
- `inbound.http.database_timeout` - Database operation timeout (default: 30s)
- Retry attempts: 2 with exponential backoff
- Error classification: transient, duplicate_key, permanent, permission

### Asynchronous Queue Processing (Best Effort)
Files from queue processing use **best effort persistence** to avoid blocking the pipeline:

1. **Queue Message** → File received from Kafka/pubsub
2. **Database Persistence** → Attempt to record with duplicate detection
3. **Continue Processing** → File processing continues regardless of persistence result
4. **Graceful Handling** → Duplicate key errors treated as success (file already recorded)

This dual approach ensures HTTP clients get reliable persistence while maintaining queue processing throughput.

## Development Notes

- Uses Go 1.24+ with modules
- Vendored dependencies in `vendor/`
- OpenAPI specification in `openapi.yaml`
- Follows moov-io coding standards and practices
- Extensive logging with structured output
- Prometheus metrics on `/metrics` endpoint