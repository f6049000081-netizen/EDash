// Package kybtest has an in-memory document store, for tests only.
package kybtest

import (
	"context"
	"errors"
	"sync"
)

type MemStore struct {
	mu      sync.Mutex
	Objects map[string][]byte
}

func NewMemStore() *MemStore { return &MemStore{Objects: map[string][]byte{}} }

func (m *MemStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *MemStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.Objects[key]
	if !ok {
		return nil, errors.New("kybtest: no such object")
	}
	return append([]byte(nil), b...), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Objects, key)
	return nil
}

// Photo is a minimal file that passes the JPEG check, with filler so it clears the minimum size.
// Different seeds give different files.
func Photo(seed string) []byte {
	b := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(seed)...)
	for len(b) < 2048 {
		b = append(b, byte(len(b)))
	}
	return b
}
