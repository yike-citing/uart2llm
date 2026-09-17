package credentials

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
)

var ErrNotFound = errors.New("credential not found")

type Store interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}

func Ensure(s Store, key string) (string, error) {
	v, e := s.Get(key)
	if e == nil {
		return v, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return "", e
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return "", e
	}
	v = base64.RawURLEncoding.EncodeToString(b)
	return v, s.Set(key, v)
}

type Memory struct {
	mu     sync.RWMutex
	Values map[string]string
}

func (m *Memory) Get(k string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.Values[k]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}
func (m *Memory) Set(k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Values == nil {
		m.Values = make(map[string]string)
	}
	m.Values[k] = v
	return nil
}
func (m *Memory) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Values, k)
	return nil
}
