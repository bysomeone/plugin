package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	errNotFound     = errors.New("consignment not found")
	errFileConflict = errors.New("consignment file cannot be changed")
	errAckConflict  = errors.New("ack cannot be changed")
)

// entry is the on-disk metadata record for one consignment. The consignment
// bytes live in a separate content-addressed blob; this record is the index.
//
// Several consignments may share one recipient_id: an address-based RGB
// invoice uses the recipient's script as its beneficiary, so every invoice to
// the same script yields the same recipient_id. They are kept as a queue and
// delivered oldest-first.
type entry struct {
	RecipientID string    `json:"recipient_id"`
	TxID        string    `json:"txid"`
	Vout        *uint32   `json:"vout,omitempty"`
	Hash        string    `json:"hash"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	Ack         *bool     `json:"ack,omitempty"`
}

// store keeps consignments on disk, indexed by recipient_id and addressed by
// the SHA-256 of their content. All exported behaviour takes the same mutex,
// so concurrent posts cannot interleave with cleanup.
type store struct {
	mu     sync.Mutex
	dir    string
	blobs  string
	index  string
	tmp    string
	ttl    time.Duration
	logger *log.Logger
	now    func() time.Time

	// entries maps recipient_id to that recipient's queue, oldest first.
	entries map[string][]*entry

	// orphanGCBlocked is set when an index file failed to load. Deleting
	// blobs then could destroy data referenced by the unreadable index.
	orphanGCBlocked bool
}

func openStore(dir string, ttl time.Duration, logger *log.Logger) (*store, error) {
	if dir == "" {
		return nil, errors.New("empty data directory")
	}
	if ttl <= 0 {
		return nil, errors.New("ttl must be positive")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	s := &store{
		dir:     abs,
		blobs:   filepath.Join(abs, "blobs"),
		index:   filepath.Join(abs, "index"),
		tmp:     filepath.Join(abs, "tmp"),
		ttl:     ttl,
		logger:  logger,
		now:     time.Now,
		entries: make(map[string][]*entry),
	}
	if s.logger == nil {
		s.logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	for _, d := range []string{s.dir, s.blobs, s.index, s.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	// Temporary files are only created by a running process; anything left
	// here is from a crash or a previous process and is safe to discard.
	if leftovers, err := os.ReadDir(s.tmp); err == nil {
		for _, de := range leftovers {
			_ = os.Remove(filepath.Join(s.tmp, de.Name()))
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if n, err := s.cleanup(); err != nil {
		s.logger.Printf("startup cleanup: %v", err)
	} else if n > 0 {
		s.logger.Printf("startup cleanup removed %d expired consignments", n)
	}
	return s, nil
}

func (s *store) load() error {
	files, err := os.ReadDir(s.index)
	if err != nil {
		return fmt.Errorf("read index directory: %w", err)
	}
	for _, de := range files {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.index, de.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			s.logger.Printf("index %s: %v (orphan blob cleanup disabled)", de.Name(), err)
			s.orphanGCBlocked = true
			continue
		}
		var e entry
		if err := json.Unmarshal(data, &e); err != nil {
			s.logger.Printf("index %s: %v (orphan blob cleanup disabled)", de.Name(), err)
			s.orphanGCBlocked = true
			continue
		}
		if !validEntry(&e) {
			s.logger.Printf("index %s: invalid record (orphan blob cleanup disabled)", de.Name())
			s.orphanGCBlocked = true
			continue
		}
		s.entries[e.RecipientID] = append(s.entries[e.RecipientID], &e)
	}
	for _, queue := range s.entries {
		sort.Slice(queue, func(i, j int) bool { return queue[i].CreatedAt.Before(queue[j].CreatedAt) })
	}
	return nil
}

func validEntry(e *entry) bool {
	if e.RecipientID == "" || len(e.RecipientID) > maxRecipientIDLen {
		return false
	}
	if e.CreatedAt.IsZero() || e.Size < 0 {
		return false
	}
	if !validHash(e.Hash) || !validTxID(e.TxID) {
		return false
	}
	return true
}

func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func validTxID(txid string) bool {
	return validHash(txid)
}

// indexPath is one file per (recipient_id, txid): a recipient may have several
// outstanding consignments, each with its own ACK.
func (s *store) indexPath(recipientID, txid string) string {
	sum := sha256.Sum256([]byte(recipientID + "\x00" + txid))
	return filepath.Join(s.index, hex.EncodeToString(sum[:])+".json")
}

func (s *store) blobPath(hash string) string {
	return filepath.Join(s.blobs, hash)
}

func (s *store) expired(e *entry, now time.Time) bool {
	return !now.Before(e.CreatedAt.Add(s.ttl))
}

// post stores data for recipientID. It returns true on the first upload and
// false when the exact same content is uploaded again. A different file for
// the same (recipientID, txid) is rejected with errFileConflict; a different
// txid is queued behind the existing consignments. Expired entries are
// dropped first, so a recipient ID can be reused after its TTL.
func (s *store) post(recipientID, txid string, vout *uint32, data []byte) (bool, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	queue := s.pruneLocked(recipientID, now)
	for _, prev := range queue {
		if prev.TxID != txid {
			continue
		}
		s.entries[recipientID] = queue
		if prev.Hash == hash {
			return false, nil
		}
		return false, errFileConflict
	}

	if err := s.writeBlob(hash, data); err != nil {
		return false, err
	}
	e := &entry{
		RecipientID: recipientID,
		TxID:        txid,
		Vout:        copyUint32(vout),
		Hash:        hash,
		Size:        int64(len(data)),
		CreatedAt:   now,
	}
	if err := s.writeIndex(e); err != nil {
		return false, err
	}
	s.entries[recipientID] = append(queue, e)
	return true, nil
}

// get returns the oldest consignment for recipientID that has not been ACKed.
func (s *store) get(recipientID string) (entry, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for _, e := range s.entries[recipientID] {
		if s.expired(e, now) || e.Ack != nil {
			continue
		}
		data, err := os.ReadFile(s.blobPath(e.Hash))
		if err != nil {
			return entry{}, nil, fmt.Errorf("read consignment blob: %w", err)
		}
		out := *e
		out.Vout = copyUint32(e.Vout)
		out.Ack = copyBool(e.Ack)
		return out, data, nil
	}
	return entry{}, nil, errNotFound
}

// getAck returns (nil, nil) when the consignment exists but has not been
// acknowledged yet. With txid empty, the newest consignment's ACK is returned.
func (s *store) getAck(recipientID, txid string) (*bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	queue := s.entries[recipientID]
	var target *entry
	for _, e := range queue {
		if s.expired(e, now) {
			continue
		}
		if txid != "" {
			if e.TxID == txid {
				target = e
				break
			}
			continue
		}
		target = e
	}
	if target == nil {
		return nil, errNotFound
	}
	return copyBool(target.Ack), nil
}

// postAck records an ACK. With txid set it targets that consignment; otherwise
// it targets the oldest un-ACKed one (the one get would have returned).
func (s *store) postAck(recipientID, txid string, ack bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var target, firstUnacked, newest *entry
	for _, e := range s.entries[recipientID] {
		if s.expired(e, now) {
			continue
		}
		newest = e
		if txid != "" {
			if e.TxID == txid {
				target = e
				break
			}
			continue
		}
		if e.Ack == nil && firstUnacked == nil {
			firstUnacked = e
		}
	}
	if txid == "" {
		// Prefer the next un-ACKed consignment; if every one is already ACKed, fall back to the
		// newest so repeating the same ACK is idempotent and a different value is a conflict.
		target = firstUnacked
		if target == nil {
			target = newest
		}
	}
	if target == nil {
		return false, errNotFound
	}
	if target.Ack != nil {
		if *target.Ack == ack {
			return false, nil
		}
		return false, errAckConflict
	}
	target.Ack = &ack
	if err := s.writeIndex(target); err != nil {
		target.Ack = nil
		return false, err
	}
	return true, nil
}

// pruneLocked drops expired entries for one recipient and returns the live
// queue. Callers must hold the mutex.
func (s *store) pruneLocked(recipientID string, now time.Time) []*entry {
	queue := s.entries[recipientID]
	kept := queue[:0]
	for _, e := range queue {
		if s.expired(e, now) {
			if err := os.Remove(s.indexPath(e.RecipientID, e.TxID)); err != nil && !os.IsNotExist(err) {
				s.logger.Printf("remove expired index: %v", err)
			}
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(s.entries, recipientID)
		return nil
	}
	s.entries[recipientID] = kept
	return kept
}

// cleanup removes expired index records and any blob no longer referenced by a
// live record.
func (s *store) cleanup() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupLocked()
}

func (s *store) cleanupLocked() (int, error) {
	now := s.now()
	removed := 0
	for id := range s.entries {
		before := len(s.entries[id])
		s.pruneLocked(id, now)
		removed += before - len(s.entries[id])
	}
	if s.orphanGCBlocked {
		return removed, nil
	}
	if err := s.gcOrphansLocked(); err != nil {
		return removed, err
	}
	return removed, nil
}

func (s *store) gcOrphansLocked() error {
	referenced := make(map[string]struct{})
	for _, queue := range s.entries {
		for _, e := range queue {
			referenced[e.Hash] = struct{}{}
		}
	}
	files, err := os.ReadDir(s.blobs)
	if err != nil {
		return fmt.Errorf("read blob directory: %w", err)
	}
	for _, de := range files {
		if de.IsDir() {
			continue
		}
		if _, ok := referenced[de.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(s.blobs, de.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove orphan blob: %w", err)
		}
	}
	return nil
}

func (s *store) writeBlob(hash string, data []byte) error {
	path := s.blobPath(hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat blob: %w", err)
	}
	return writeFileAtomic(s.tmp, s.blobs, hash, data)
}

func (s *store) writeIndex(e *entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode index: %w", err)
	}
	return writeFileAtomic(s.tmp, s.index, filepath.Base(s.indexPath(e.RecipientID, e.TxID)), data)
}

// writeFileAtomic writes data to a temporary file, fsyncs it, then renames it
// into dir. After a crash either the old file or the complete new file exists.
func writeFileAtomic(tmpDir, dstDir, name string, data []byte) (err error) {
	f, err := os.CreateTemp(tmpDir, "tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err = os.Rename(tmpName, filepath.Join(dstDir, name)); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return syncDir(dstDir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func copyUint32(v *uint32) *uint32 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func copyBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
