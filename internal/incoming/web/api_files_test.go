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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moov-io/achgateway/internal/files"
	"github.com/moov-io/achgateway/internal/incoming"
	"github.com/moov-io/achgateway/internal/incoming/stream/streamtest"
	"github.com/moov-io/achgateway/internal/service"
	"github.com/moov-io/achgateway/pkg/models"
	"github.com/moov-io/base/log"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestCreateFileHandler(t *testing.T) {
	topic, sub := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository()
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Send a file over HTTP
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))
	req := httptest.NewRequest("POST", "/shards/s1/files/f1", bytes.NewReader(bs))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Verify our subscription receives a message
	msg, err := sub.Receive(context.Background())
	require.NoError(t, err)

	var file incoming.ACHFile
	require.NoError(t, models.ReadEvent(msg.Body, &file))

	require.Equal(t, "f1", file.FileID)
	require.Equal(t, "s1", file.ShardKey)
	require.Equal(t, "231380104", file.File.Header.ImmediateDestination)

	validateOpts := file.File.GetValidation()
	require.NotNil(t, validateOpts)
	require.True(t, validateOpts.PreserveSpaces)
}

func TestCreateFileHandlerErr(t *testing.T) {
	topic, _ := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository()
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Send a file over HTTP
	req := httptest.NewRequest("POST", "/shards/s1/files/f1", strings.NewReader(`"invalid"`))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCancelFileHandler(t *testing.T) {
	topic, sub := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository()
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Cancel our file
	req := httptest.NewRequest("DELETE", "/shards/s2/files/f2.ach", nil)

	// Setup the response
	go func() {
		time.Sleep(time.Second)
		cancellationResponses <- models.FileCancellationResponse{
			FileID:     "f2.ach",
			ShardKey:   "s2",
			Successful: true,
		}
	}()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Verify our subscription receives a message
	msg, err := sub.Receive(context.Background())
	require.NoError(t, err)

	var file incoming.CancelACHFile
	require.NoError(t, models.ReadEvent(msg.Body, &file))

	require.Equal(t, "f2", file.FileID) // make sure .ach suffix is trimmed
	require.Equal(t, "s2", file.ShardKey)

	var response incoming.FileCancellationResponse
	json.NewDecoder(w.Body).Decode(&response)

	require.Equal(t, "f2.ach", response.FileID)
	require.Equal(t, "s2", response.ShardKey)
	require.True(t, response.Successful)
}

func TestCreateFileHandler_DatabasePersistenceFailure(t *testing.T) {
	topic, _ := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository().(*files.MockRepository)
	
	// Simulate database persistence failure
	fileRepo.SetError(errors.New("spanner: code = \"DeadlineExceeded\", desc = \"context deadline exceeded\""))
	
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Send a valid file over HTTP
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))
	req := httptest.NewRequest("POST", "/shards/s1/files/f1", bytes.NewReader(bs))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Should return 500 when database persistence fails
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCreateFileHandler_ConcurrentSameFileID(t *testing.T) {
	topic, sub := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository()
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Load test file
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))

	// Submit the same file ID concurrently
	var wg sync.WaitGroup
	const numGoroutines = 5
	results := make([]int, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			
			req := httptest.NewRequest("POST", "/shards/s1/files/concurrent-test", bytes.NewReader(bs))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			
			results[index] = w.Code
		}(i)
	}

	wg.Wait()

	// All requests should succeed (200) since our implementation handles duplicates gracefully
	successCount := 0
	for _, code := range results {
		require.True(t, code == http.StatusOK, fmt.Sprintf("Expected 200, got %d", code))
		if code == http.StatusOK {
			successCount++
		}
	}
	require.Equal(t, numGoroutines, successCount)

	// Verify we received messages (could be multiple due to concurrent processing)
	for i := 0; i < numGoroutines; i++ {
		msg, err := sub.Receive(context.Background())
		require.NoError(t, err)
		
		var file incoming.ACHFile
		require.NoError(t, models.ReadEvent(msg.Body, &file))
		require.Equal(t, "concurrent-test", file.FileID)
	}
}

func TestCreateFileHandler_SpannerTimeoutError(t *testing.T) {
	topic, _ := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository().(*files.MockRepository)
	
	// Simulate specific Spanner timeout error
	fileRepo.SetError(errors.New("spanner: code = \"DeadlineExceeded\", desc = \"context deadline exceeded, transaction outcome unknown\""))
	
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Send a valid file over HTTP
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))
	req := httptest.NewRequest("POST", "/shards/s1/files/timeout-test", bytes.NewReader(bs))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Should return 500 for database timeout errors
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestCreateFileHandler_DatabaseRecovery(t *testing.T) {
	topic, sub := streamtest.InmemStream(t)

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository().(*files.MockRepository)
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Load test file
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))

	// First request fails due to database error
	fileRepo.SetError(errors.New("database connection failed"))
	
	req1 := httptest.NewRequest("POST", "/shards/s1/files/recovery-test-1", bytes.NewReader(bs))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusInternalServerError, w1.Code)

	// Second request succeeds after database recovery
	fileRepo.SetError(nil)
	
	req2 := httptest.NewRequest("POST", "/shards/s1/files/recovery-test-2", bytes.NewReader(bs))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	// Verify successful message was published
	msg, err := sub.Receive(context.Background())
	require.NoError(t, err)
	
	var file incoming.ACHFile
	require.NoError(t, models.ReadEvent(msg.Body, &file))
	require.Equal(t, "recovery-test-2", file.FileID)
}

func TestCreateFileHandler_PublishingFailureAfterPersistence(t *testing.T) {
	// This test verifies that publishing failures don't affect HTTP response
	// when the file has already been successfully persisted
	topic, _ := streamtest.InmemStream(t)
	
	// Close the topic to simulate publishing failure
	topic.Shutdown(context.Background())

	cancellationResponses := make(chan models.FileCancellationResponse)
	fileRepo := files.NewMockRepository() // No errors set - persistence succeeds
	controller := NewFilesController(log.NewTestLogger(), service.HTTPConfig{}, topic, cancellationResponses, fileRepo)
	r := mux.NewRouter()
	controller.AppendRoutes(r)

	// Send a valid file over HTTP
	bs, _ := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "ppd-valid.json"))
	req := httptest.NewRequest("POST", "/shards/s1/files/publish-fail-test", bytes.NewReader(bs))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Should still return 200 because file was successfully persisted
	// even though publishing failed
	require.Equal(t, http.StatusOK, w.Code)
}
