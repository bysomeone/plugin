package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestOpenStoreValidation(t *testing.T) {
	_, err := openStore("", time.Hour, testLogger())
	require.Error(t, err)
	_, err = openStore(t.TempDir(), 0, testLogger())
	require.Error(t, err)
}

func TestStoreStartupCleanup(t *testing.T) {
	dir := t.TempDir()
	s1, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	s1.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	created, err := s1.post("old-recipient", testTxID, nil, []byte("old"))
	require.NoError(t, err)
	require.True(t, created)

	s2, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	_, _, err = s2.get("old-recipient")
	require.ErrorIs(t, err, errNotFound)
	assertNoFiles(t, filepath.Join(dir, "blobs"))
	assertNoFiles(t, filepath.Join(dir, "index"))
}

func TestStoreCorruptIndexKeepsBlobs(t *testing.T) {
	dir := t.TempDir()
	s1, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	created, err := s1.post("recipient", testTxID, nil, []byte("data"))
	require.NoError(t, err)
	require.True(t, created)

	indexes, err := os.ReadDir(filepath.Join(dir, "index"))
	require.NoError(t, err)
	require.Len(t, indexes, 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index", indexes[0].Name()), []byte("{"), 0o600))

	s2, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	require.True(t, s2.orphanGCBlocked)
	_, _, err = s2.get("recipient")
	require.ErrorIs(t, err, errNotFound)

	blobs, err := os.ReadDir(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	assert.Len(t, blobs, 1, "blobs referenced by an unreadable index must be kept")
}

func TestStoreConcurrentPosts(t *testing.T) {
	s, err := openStore(t.TempDir(), time.Hour, testLogger())
	require.NoError(t, err)

	const workers = 20
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := s.post("same-recipient", testTxID, nil, []byte("same-bytes"))
			results <- created
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	assert.Equal(t, 1, createdCount, "exactly one upload should create the entry")

	_, data, err := s.get("same-recipient")
	require.NoError(t, err)
	assert.Equal(t, "same-bytes", string(data))
}

func TestStorePostAckPersistence(t *testing.T) {
	dir := t.TempDir()
	s1, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	_, err = s1.post("recipient", testTxID, nil, []byte("data"))
	require.NoError(t, err)

	created, err := s1.postAck("recipient", "", false)
	require.NoError(t, err)
	require.True(t, created)

	created, err = s1.postAck("recipient", "", false)
	require.NoError(t, err)
	require.False(t, created)

	_, err = s1.postAck("recipient", "", true)
	require.ErrorIs(t, err, errAckConflict)

	s2, err := openStore(dir, time.Hour, testLogger())
	require.NoError(t, err)
	ack, err := s2.getAck("recipient", "")
	require.NoError(t, err)
	require.NotNil(t, ack)
	assert.False(t, *ack)

	_, err = s2.getAck("missing", "")
	require.ErrorIs(t, err, errNotFound)
}

func TestStoreNoTempFilesLeft(t *testing.T) {
	s, err := openStore(t.TempDir(), time.Hour, testLogger())
	require.NoError(t, err)
	_, err = s.post("recipient", testTxID, nil, []byte("data"))
	require.NoError(t, err)
	assertNoFiles(t, s.tmp)
}

func TestCleanupInterval(t *testing.T) {
	assert.Equal(t, time.Second, cleanupInterval(100*time.Millisecond))
	assert.Equal(t, 15*time.Minute, cleanupInterval(time.Hour))
	assert.Equal(t, time.Hour, cleanupInterval(100*time.Hour))
}
