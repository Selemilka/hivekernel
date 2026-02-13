package ipc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/selemilka/hivekernel/internal/process"
)

// Visibility levels for artifacts.
type Visibility int

const (
	VisPrivate Visibility = iota // Only the storing process
	VisUser                      // All processes with same USER
	VisSubtree                   // All descendants of the storing process
	VisGlobal                    // Everyone
)

func (v Visibility) String() string {
	switch v {
	case VisPrivate:
		return "private"
	case VisUser:
		return "user"
	case VisSubtree:
		return "subtree"
	case VisGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Artifact is a stored piece of data in shared memory.
type Artifact struct {
	ID          string
	Key         string
	Content     []byte
	ContentType string
	Visibility  Visibility
	StoredByPID process.PID
	StoredAt    time.Time
}

// SharedMemory provides artifact storage with visibility-based access control.
type SharedMemory struct {
	mu       sync.RWMutex
	byID     map[string]*Artifact
	byKey    map[string]*Artifact // key -> latest artifact
	registry *process.Registry
	nextID   atomic.Uint64
}

// NewSharedMemory creates a new shared memory store.
func NewSharedMemory(registry *process.Registry) *SharedMemory {
	return &SharedMemory{
		byID:     make(map[string]*Artifact),
		byKey:    make(map[string]*Artifact),
		registry: registry,
	}
}

// Store saves an artifact and returns its ID.
func (sm *SharedMemory) Store(
	storerPID process.PID,
	key string,
	content []byte,
	contentType string,
	visibility Visibility,
) (string, error) {
	if key == "" {
		return "", fmt.Errorf("artifact key is required")
	}

	_, err := sm.registry.Get(storerPID)
	if err != nil {
		return "", fmt.Errorf("storer PID %d not found", storerPID)
	}

	id := fmt.Sprintf("art-%d", sm.nextID.Add(1))

	art := &Artifact{
		ID:          id,
		Key:         key,
		Content:     content,
		ContentType: contentType,
		Visibility:  visibility,
		StoredByPID: storerPID,
		StoredAt:    time.Now(),
	}

	sm.mu.Lock()
	sm.byID[id] = art
	sm.byKey[key] = art
	sm.mu.Unlock()

	return id, nil
}

// Get retrieves an artifact by key, checking access permissions.
func (sm *SharedMemory) Get(readerPID process.PID, key string) (*Artifact, error) {
	sm.mu.RLock()
	art, ok := sm.byKey[key]
	sm.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("artifact %q not found", key)
	}

	if err := sm.checkAccess(readerPID, art); err != nil {
		return nil, err
	}

	return art, nil
}

// GetByID retrieves an artifact by ID, checking access permissions.
func (sm *SharedMemory) GetByID(readerPID process.PID, id string) (*Artifact, error) {
	sm.mu.RLock()
	art, ok := sm.byID[id]
	sm.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("artifact %q not found", id)
	}

	if err := sm.checkAccess(readerPID, art); err != nil {
		return nil, err
	}

	return art, nil
}

// List returns all artifacts matching a prefix that the reader can access.
func (sm *SharedMemory) List(readerPID process.PID, prefix string) []*Artifact {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var result []*Artifact
	for _, art := range sm.byKey {
		if prefix != "" && !hasPrefix(art.Key, prefix) {
			continue
		}
		if sm.checkAccess(readerPID, art) == nil {
			result = append(result, art)
		}
	}
	return result
}

// checkAccess verifies that readerPID is allowed to read the artifact.
func (sm *SharedMemory) checkAccess(readerPID process.PID, art *Artifact) error {
	switch art.Visibility {
	case VisGlobal:
		return nil

	case VisPrivate:
		if readerPID != art.StoredByPID {
			return fmt.Errorf("access denied: artifact %q is private to PID %d", art.Key, art.StoredByPID)
		}
		return nil

	case VisUser:
		reader, err := sm.registry.Get(readerPID)
		if err != nil {
			return fmt.Errorf("reader PID %d not found", readerPID)
		}
		storer, err := sm.registry.Get(art.StoredByPID)
		if err != nil {
			return fmt.Errorf("storer PID %d not found", art.StoredByPID)
		}
		if reader.User != storer.User {
			return fmt.Errorf("access denied: artifact %q belongs to user %q, reader is %q",
				art.Key, storer.User, reader.User)
		}
		return nil

	case VisSubtree:
		// Reader must be a descendant of the storer (or the storer itself).
		if readerPID == art.StoredByPID {
			return nil
		}
		descendants := sm.registry.GetDescendants(art.StoredByPID)
		for _, d := range descendants {
			if d.PID == readerPID {
				return nil
			}
		}
		return fmt.Errorf("access denied: artifact %q is subtree-only under PID %d", art.Key, art.StoredByPID)

	default:
		return fmt.Errorf("unknown visibility %d", art.Visibility)
	}
}

// Delete removes an artifact. Only the storer or a global admin can delete.
func (sm *SharedMemory) Delete(callerPID process.PID, key string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	art, ok := sm.byKey[key]
	if !ok {
		return fmt.Errorf("artifact %q not found", key)
	}

	if callerPID != art.StoredByPID {
		// Check if caller is kernel.
		caller, err := sm.registry.Get(callerPID)
		if err != nil || caller.Role != process.RoleKernel {
			return fmt.Errorf("access denied: only storer or kernel can delete artifact %q", key)
		}
	}

	delete(sm.byKey, key)
	delete(sm.byID, art.ID)
	return nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
