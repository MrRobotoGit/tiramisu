package opentracker

import "sync"

// OpenTracker mantiene contatori per handle FUSE aperti, indicizzati per hash
// torrent e per path fisico. Permette query O(1) da goroutine di manutenzione
// (cleanup, eviction, unlink) senza scansionare activeHandles.
//
// OpenTracker NON sostituisce NativePumpState.refCount (fonte di verità per il
// ciclo vita della pump). Aggiunge solo la dimensione per-hash e un'interfaccia
// consultabile dall'esterno senza accedere a activePumps.
type OpenTracker struct {
	mu     sync.RWMutex
	byHash map[string]int32    // torrent hash (lowercase hex40) → handle count
	byPath map[string]int32    // file path (assoluto) → handle count
	byKey  map[any][][2]string // handle → the (hash, path) of each IncFor not yet released
}

// New restituisce un OpenTracker pronto all'uso.
func New() *OpenTracker {
	return &OpenTracker{
		byHash: make(map[string]int32),
		byPath: make(map[string]int32),
		byKey:  make(map[any][][2]string),
	}
}

// IncFor is Inc on behalf of handle key: ReleaseFor undoes each of key's increments exactly once,
// whatever happened to the pump the handle attached to.
func (t *OpenTracker) IncFor(key any, hash, path string) {
	t.mu.Lock()
	t.byKey[key] = append(t.byKey[key], [2]string{hash, path})
	t.mu.Unlock()
	t.Inc(hash, path)
}

// ReleaseFor decrements every increment IncFor recorded for key; a second call does nothing.
func (t *OpenTracker) ReleaseFor(key any) {
	t.mu.Lock()
	incs := t.byKey[key]
	delete(t.byKey, key)
	t.mu.Unlock()
	for _, hp := range incs {
		t.Dec(hp[0], hp[1])
	}
}

// Inc incrementa i contatori per hash e path. Chiamare subito dopo
// l'incremento di NativePumpState.refCount (quando h.hasSlot diventa true).
func (t *OpenTracker) Inc(hash, path string) {
	t.mu.Lock()
	if hash != "" {
		t.byHash[hash]++
	}
	if path != "" {
		t.byPath[path]++
	}
	t.mu.Unlock()
}

// Dec decrementa i contatori per hash e path. Chiamare subito dopo il
// decremento di NativePumpState.refCount in Release(), solo se h.hasSlot.
// Le entry vengono eliminate quando il contatore raggiunge zero.
func (t *OpenTracker) Dec(hash, path string) {
	t.mu.Lock()
	if hash != "" {
		if t.byHash[hash] <= 1 {
			delete(t.byHash, hash)
		} else {
			t.byHash[hash]--
		}
	}
	if path != "" {
		if t.byPath[path] <= 1 {
			delete(t.byPath, path)
		} else {
			t.byPath[path]--
		}
	}
	t.mu.Unlock()
}

// CountByHash restituisce il numero di handle aperti per un dato hash.
func (t *OpenTracker) CountByHash(hash string) int32 {
	t.mu.RLock()
	n := t.byHash[hash]
	t.mu.RUnlock()
	return n
}

// CountByPath restituisce il numero di handle aperti per un dato path.
func (t *OpenTracker) CountByPath(path string) int32 {
	t.mu.RLock()
	n := t.byPath[path]
	t.mu.RUnlock()
	return n
}

// IsHashOpen restituisce true se esiste almeno un handle aperto per l'hash.
func (t *OpenTracker) IsHashOpen(hash string) bool {
	return t.CountByHash(hash) > 0
}

// IsPathOpen restituisce true se esiste almeno un handle aperto per il path.
func (t *OpenTracker) IsPathOpen(path string) bool {
	return t.CountByPath(path) > 0
}

// OpenPaths restituisce una slice con tutti i path attualmente aperti.
// Usato da cleanup per proteggere i file in streaming dallo sweep InodeMap.
func (t *OpenTracker) OpenPaths() []string {
	t.mu.RLock()
	paths := make([]string, 0, len(t.byPath))
	for p := range t.byPath {
		paths = append(paths, p)
	}
	t.mu.RUnlock()
	return paths
}
