package bucket

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mem is an in-memory bucket for tests. Now, when set, stamps modification times.
type Mem struct {
	mu      sync.Mutex
	objects map[string]memObject
	Now     func() time.Time
}

type memObject struct {
	data []byte
	mod  time.Time
}

func NewMem() *Mem { return &Mem{objects: map[string]memObject{}} }

func (m *Mem) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mem) Put(_ context.Context, key string, data []byte, ifAbsent bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok && ifAbsent {
		return fmt.Errorf("%s: %w", key, ErrExists)
	}
	m.objects[key] = memObject{append([]byte(nil), data...), m.now()}
	return nil
}

func (m *Mem) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	return append([]byte(nil), o.data...), nil
}

func (m *Mem) Head(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return Object{}, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	return Object{Key: key, Size: int64(len(o.data)), Modified: o.mod}, nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *Mem) List(_ context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k, Size: int64(len(o.data)), Modified: o.mod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
