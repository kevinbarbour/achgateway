// Licensed to The Moov Authors under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. The Moov Authors licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/moov-io/ach"
	"github.com/moov-io/achgateway/internal/files"
	"github.com/moov-io/achgateway/internal/incoming"
	"github.com/moov-io/achgateway/internal/incoming/stream"
	"github.com/moov-io/achgateway/internal/service"
	"github.com/moov-io/achgateway/pkg/compliance"
	"github.com/moov-io/achgateway/pkg/models"
	"github.com/moov-io/base/log"
	"github.com/moov-io/base/telemetry"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gocloud.dev/pubsub"
)

func NewFilesController(logger log.Logger, cfg service.HTTPConfig, pub stream.Publisher, fileRepo files.Repository, cancellationResponses chan models.FileCancellationResponse) *FilesController {
	controller := &FilesController{
		logger:    logger,
		cfg:       cfg,
		publisher: pub,
		fileRepo:  fileRepo,

		activeCancellations:   make(map[string]chan models.FileCancellationResponse),
		cancellationResponses: cancellationResponses,
	}
	controller.listenForCancellations()
	return controller
}

type FilesController struct {
	logger    log.Logger
	cfg       service.HTTPConfig
	publisher stream.Publisher
	fileRepo  files.Repository

	cancellationLock      sync.Mutex
	activeCancellations   map[string]chan models.FileCancellationResponse
	cancellationResponses chan models.FileCancellationResponse
}

func (c *FilesController) listenForCancellations() {
	c.logger.Info().Log("listening for cancellation responses")
	go func() {
		for {
			// Wait for a message
			cancel := <-c.cancellationResponses
			c.logger.Info().Logf("received cancellation response: %#v", cancel)

			fileID := strings.TrimSuffix(cancel.FileID, ".ach")

			c.cancellationLock.Lock()
			out, exists := c.activeCancellations[fileID]
			if exists {
				out <- cancel
				delete(c.activeCancellations, fileID)
			}
			c.cancellationLock.Unlock()
		}
	}()
}

func (c *FilesController) AppendRoutes(router *mux.Router) *mux.Router {
	router.
		Name("Files.create").
		Methods("POST").
		Path("/shards/{shardKey}/files/{fileID}").
		HandlerFunc(c.CreateFileHandler)

	router.
		Name("Files.cancel").
		Methods("DELETE").
		Path("/shards/{shardKey}/files/{fileID}").
		HandlerFunc(c.CancelFileHandler)

	return router
}

func (c *FilesController) CreateFileHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	shardKey, fileID := vars["shardKey"], vars["fileID"]
	if shardKey == "" || fileID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx, span := telemetry.StartSpan(r.Context(), "create-file-handler", trace.WithAttributes(
		attribute.String("achgateway.shardKey", shardKey),
		attribute.String("achgateway.fileID", fileID),
	))
	defer span.End()

	logger := c.logger.With(log.Fields{
		"shard_key": log.String(shardKey),
		"file_id":   log.String(fileID),
	})

	bs, err := c.readBody(r)
	if err != nil {
		logger.LogErrorf("error reading file: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	file, err := ach.NewReader(bytes.NewReader(bs)).Read()
	if err != nil {
		// attempt JSON decode
		f, err := ach.FileFromJSON(bs)
		if f == nil || err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file = *f
	}

	// Persist the file to database BEFORE publishing to queue
	// This ensures 200 response only happens after successful persistence
	hostname, _ := os.Hostname()
	acceptedFile := files.AcceptedFile{
		FileID:     fileID,
		ShardKey:   shardKey,
		Hostname:   hostname,
		AcceptedAt: time.Now().In(time.UTC),
	}

	// Add timeout for database operations to prevent hanging HTTP requests
	timeout := c.cfg.DatabaseTimeout
	if timeout == 0 {
		timeout = 30 * time.Second // Default timeout
	}
	dbCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	if err := c.recordFileWithRetry(dbCtx, acceptedFile, 2); err != nil {
		duration := time.Since(start)
		errorClass := classifyDatabaseError(err)
		
		logger.With(log.Fields{
			"duration_ms":  log.Float64(float64(duration.Nanoseconds()) / 1e6),
			"operation":    log.String("database_record"),
			"error_class":  log.String(errorClass),
			"retry_count":  log.Int(2),
		}).LogErrorf("error persisting file to database after retries: %v", err)
		
		// Add telemetry attributes for failed database operation
		span.SetAttributes(
			attribute.Float64("achgateway.database.duration_ms", float64(duration.Nanoseconds())/1e6),
			attribute.String("achgateway.database.operation", "record_file"),
			attribute.Bool("achgateway.database.success", false),
			attribute.String("achgateway.database.error", err.Error()),
			attribute.String("achgateway.database.error_class", errorClass),
			attribute.Int("achgateway.database.retry_count", 2),
		)
		
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	
	duration := time.Since(start)
	logger.With(log.Fields{
		"duration_ms": log.Float64(float64(duration.Nanoseconds()) / 1e6),
		"operation":   log.String("database_record"),
	}).Log("file persisted to database successfully, publishing to queue")

	// Add telemetry attributes for database operation monitoring
	span.SetAttributes(
		attribute.Float64("achgateway.database.duration_ms", float64(duration.Nanoseconds())/1e6),
		attribute.String("achgateway.database.operation", "record_file"),
		attribute.Bool("achgateway.database.success", true),
	)

	if err := c.publishFile(ctx, shardKey, fileID, &file); err != nil {
		logger.LogErrorf("error publishing file to queue (file already persisted): %v", err)
		// Note: File is already persisted, so we should still return 200
		// The async processor will handle the file even if this publish fails
	}

	w.WriteHeader(http.StatusOK)
}

func (c *FilesController) readBody(req *http.Request) ([]byte, error) {
	defer req.Body.Close()

	var reader io.Reader = req.Body
	if c.cfg.MaxBodyBytes > 0 {
		reader = io.LimitReader(reader, c.cfg.MaxBodyBytes)
	}
	bs, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	return compliance.Reveal(c.cfg.Transform, bs)
}

func (c *FilesController) publishFile(ctx context.Context, shardKey, fileID string, file *ach.File) error {
	bs, err := compliance.Protect(c.cfg.Transform, models.Event{
		Event: incoming.ACHFile{
			FileID:   fileID,
			ShardKey: shardKey,
			File:     file,
		},
	})
	if err != nil {
		return fmt.Errorf("unable to protect incoming file event: %v", err)
	}

	meta := make(map[string]string)
	meta["fileID"] = fileID
	meta["shardKey"] = shardKey

	return c.publisher.Send(ctx, &pubsub.Message{
		Body:     bs,
		Metadata: meta,
	})
}

// isTransientError determines if an error is likely transient and worth retrying
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	
	errStr := err.Error()
	// Check for common transient error patterns
	return strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "temporary failure") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "UNAVAILABLE") ||
		strings.Contains(errStr, "DeadlineExceeded")
}

// isDuplicateKeyError determines if an error indicates a duplicate key constraint violation
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	
	errStr := err.Error()
	// Check for duplicate key error patterns in different databases
	return strings.Contains(errStr, "duplicate key") ||
		strings.Contains(errStr, "UNIQUE constraint") ||
		strings.Contains(errStr, "AlreadyExists") ||
		strings.Contains(errStr, "Entry already exists")
}

// classifyDatabaseError categorizes database errors for better handling and monitoring
func classifyDatabaseError(err error) string {
	if err == nil {
		return "none"
	}
	
	if isDuplicateKeyError(err) {
		return "duplicate_key"
	}
	
	if isTransientError(err) {
		return "transient"
	}
	
	errStr := err.Error()
	if strings.Contains(errStr, "permission") || strings.Contains(errStr, "authentication") {
		return "permission"
	}
	
	if strings.Contains(errStr, "not found") || strings.Contains(errStr, "NotFound") {
		return "not_found"
	}
	
	return "permanent"
}

// recordFileWithRetry attempts to record a file with retry logic for transient failures
func (c *FilesController) recordFileWithRetry(ctx context.Context, acceptedFile files.AcceptedFile, maxRetries int) error {
	var lastErr error
	
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			backoff := time.Duration(attempt*attempt) * 100 * time.Millisecond
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
			
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		
		err := c.fileRepo.Record(ctx, acceptedFile)
		if err == nil {
			return nil // Success
		}
		
		lastErr = err
		errorClass := classifyDatabaseError(err)
		
		// Don't retry duplicate key errors - they indicate the file is already processed
		if errorClass == "duplicate_key" {
			return nil // Treat duplicate as success since file is already recorded
		}
		
		// Don't retry permanent errors
		if errorClass == "permanent" || errorClass == "permission" {
			break
		}
		
		// Only retry transient errors
		if errorClass != "transient" {
			break
		}
	}
	
	return lastErr
}

func (c *FilesController) CancelFileHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	shardKey, fileID := vars["shardKey"], vars["fileID"]
	if shardKey == "" || fileID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Remove .ach suffix if the request added it
	fileID = strings.TrimSuffix(fileID, ".ach")

	ctx, span := telemetry.StartSpan(r.Context(), "cancel-file-handler", trace.WithAttributes(
		attribute.String("achgateway.shardKey", shardKey),
		attribute.String("achgateway.fileID", fileID),
	))
	defer span.End()

	waiter := make(chan models.FileCancellationResponse, 1)

	err := c.cancelFile(ctx, shardKey, fileID, waiter)
	if err != nil {
		c.logger.With(log.Fields{
			"shard_key": log.String(shardKey),
			"file_id":   log.String(fileID),
		}).LogErrorf("canceling file: %v", err)

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var response models.FileCancellationResponse
	select {
	case resp := <-waiter:
		response = resp

	case <-time.After(10 * time.Second):
		response = models.FileCancellationResponse{
			FileID:     fileID,
			ShardKey:   shardKey,
			Successful: false,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (c *FilesController) cancelFile(ctx context.Context, shardKey, fileID string, waiter chan models.FileCancellationResponse) error {
	c.cancellationLock.Lock()
	c.activeCancellations[fileID] = waiter
	c.cancellationLock.Unlock()

	bs, err := compliance.Protect(c.cfg.Transform, models.Event{
		Event: incoming.CancelACHFile{
			FileID:   fileID,
			ShardKey: shardKey,
		},
	})
	if err != nil {
		return fmt.Errorf("unable to protect cancel file event: %v", err)
	}

	meta := make(map[string]string)
	meta["fileID"] = fileID
	meta["shardKey"] = shardKey

	return c.publisher.Send(ctx, &pubsub.Message{
		Body:     bs,
		Metadata: meta,
	})
}
