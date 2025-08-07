package files

import (
	"context"
	"sync"
)

type MockRepository struct {
	mu  sync.RWMutex
	err error
}

func NewMockRepository() Repository {
	return &MockRepository{}
}

func (r *MockRepository) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *MockRepository) GetError() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

func (r *MockRepository) Record(_ context.Context, file AcceptedFile) error {
	return r.GetError()
}

func (r *MockRepository) Cancel(_ context.Context, fileID string) error {
	return r.GetError()
}
