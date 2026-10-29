// Package main implements rgb-tss-tool, an offline export/import utility for the
// CGGMP key material stored in a neutrino.db (bucket "rgbx-tss").
//
// The tool talks to the bbolt file directly. It must only be run while the node is
// stopped: bbolt allows a single writer, and a running node would both keep the
// database locked and potentially write key material while it is being exported.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	ctss "github.com/33cn/chain33/system/crypto/tss"
	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino"
	"github.com/btcsuite/btcd/btcec/v2"
	bolt "go.etcd.io/bbolt"
)

const (
	// archiveVersion is the on-disk format version. Import refuses anything else.
	archiveVersion = 1

	dbOpenTimeout  = 5 * time.Second
	maxArchiveSize = 16 << 20
)

// archive is the standalone export format. Entries carry the raw bucket values;
// the metadata fields exist so an operator can tell which key and which node the
// archive belongs to without opening the database or decoding the payload.
//
// This is a container only: the entry values are the node's own serialization
// (cggmp.DKGResult / refreshRecord JSON), not a new encoding.
type archive struct {
	Version     int            `json:"version"`
	ChainTitle  string         `json:"chain_title"`
	GroupPubKey string         `json:"group_pubkey"`
	RID         string         `json:"rid"`
	ShareID     string         `json:"share_id"`
	ExportedAt  time.Time      `json:"exported_at"`
	Entries     []archiveEntry `json:"entries"`
}

type archiveEntry struct {
	Key         string `json:"key"`
	ValueBase64 string `json:"value_base64"`
}

// bucketData is an in-memory snapshot of the rgbx-tss bucket.
type bucketData struct {
	keys   []string
	values map[string][]byte
}

func (b *bucketData) empty() bool { return b == nil || len(b.keys) == 0 }

type exportOptions struct {
	DBPath     string
	OutPath    string
	ChainTitle string
	// Warn receives non-fatal inconsistencies (e.g. a refresh record that does
	// not decode). Nil means discard.
	Warn io.Writer
}

type exportSummary struct {
	OutPath     string
	GroupPubKey string
	RID         string
	ShareID     string
	Entries     int
}

type importOptions struct {
	FilePath   string
	DBPath     string
	Force      bool
	ChainTitle string
	Warn       io.Writer
}

type importSummary struct {
	DBPath      string
	GroupPubKey string
	RID         string
	ShareID     string
	Entries     int
	BackupPath  string
}

func warnf(w io.Writer, format string, args ...interface{}) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "warning: "+format+"\n", args...)
}

// exportTSS reads the rgbx-tss bucket and writes it to opts.OutPath (0600,
// temp file + rename). The database must already exist; exporting never creates it.
func exportTSS(opts exportOptions) (*exportSummary, error) {
	dbPath := filepath.Clean(opts.DBPath)
	outPath := filepath.Clean(opts.OutPath)
	if dbPath == "." || outPath == "." || opts.DBPath == "" || opts.OutPath == "" {
		return nil, errors.New("--db and --out are required")
	}
	if sameFile(dbPath, outPath) {
		return nil, fmt.Errorf("--out must not be the database file itself (%s)", dbPath)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("database file %s: %w", dbPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database path %s is not a regular file", dbPath)
	}

	db, err := openBolt(dbPath, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var data *bucketData
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		data, err = readBucket(tx)
		return err
	}); err != nil {
		return nil, err
	}
	if data.empty() {
		return nil, fmt.Errorf("bucket %q in %s is empty; nothing to export", neutrino.TSSBucketName, dbPath)
	}

	dkg, err := decodeDKG(data)
	if err != nil {
		return nil, fmt.Errorf("database %s: %w", dbPath, err)
	}
	pub, err := groupPubKey(dkg)
	if err != nil {
		return nil, fmt.Errorf("decode group public key from %q: %w", neutrino.TSSDKGResultKey, err)
	}
	rid := hex.EncodeToString(dkg.Rid)

	// The refresh record is not required for the archive to be useful (it can be
	// rerun), but when it is present and decodable it must belong to this DKG.
	if raw, ok := data.values[neutrino.TSSRefreshResultKey]; ok {
		var envelope struct {
			DkgRid string `json:"dkgRid"`
		}
		switch err := json.Unmarshal(raw, &envelope); {
		case err != nil:
			warnf(opts.Warn, "refresh entry %q does not decode (%v); exporting its raw bytes anyway",
				neutrino.TSSRefreshResultKey, err)
		case envelope.DkgRid != "" && rid != "" && envelope.DkgRid != rid:
			warnf(opts.Warn, "refresh entry belongs to DKG rid %s, but the DKG result has rid %s; "+
				"exporting both anyway", envelope.DkgRid, rid)
		}
	}

	a := &archive{
		Version:     archiveVersion,
		ChainTitle:  strings.TrimSpace(opts.ChainTitle),
		GroupPubKey: hex.EncodeToString(pub.SerializeCompressed()),
		RID:         rid,
		ShareID:     shareID(dkg),
		ExportedAt:  time.Now().UTC(),
		Entries:     make([]archiveEntry, 0, len(data.keys)),
	}
	for _, key := range data.keys {
		a.Entries = append(a.Entries, archiveEntry{
			Key:         key,
			ValueBase64: base64.StdEncoding.EncodeToString(data.values[key]),
		})
	}

	if err := writeArchiveAtomic(outPath, a); err != nil {
		return nil, err
	}
	return &exportSummary{
		OutPath:     outPath,
		GroupPubKey: a.GroupPubKey,
		RID:         a.RID,
		ShareID:     a.ShareID,
		Entries:     len(a.Entries),
	}, nil
}

// importTSS validates the archive, writes its entries into the rgbx-tss bucket,
// then reads the bucket back and verifies it byte-for-byte.
//
// The database file may not exist yet (bbolt creates it). If the target bucket
// already holds entries, the import is refused unless opts.Force is set; in that
// case the existing values are first copied to <db>.tss-backup-<unix> (0600) so
// the overwrite can be rolled back.
func importTSS(opts importOptions) (*importSummary, error) {
	filePath := filepath.Clean(opts.FilePath)
	dbPath := filepath.Clean(opts.DBPath)
	if opts.FilePath == "" || opts.DBPath == "" || dbPath == "." {
		return nil, errors.New("--file and --db are required")
	}
	if sameFile(filePath, dbPath) {
		return nil, fmt.Errorf("--file must not be the database file itself (%s)", dbPath)
	}

	a, values, err := loadArchive(filePath)
	if err != nil {
		return nil, err
	}
	meta, err := validateArchive(a, values, opts.Warn)
	if err != nil {
		return nil, err
	}
	if a.ChainTitle != "" && opts.ChainTitle != "" && a.ChainTitle != strings.TrimSpace(opts.ChainTitle) {
		warnf(opts.Warn, "archive chain_title %q differs from --chain-title %q; continuing",
			a.ChainTitle, strings.TrimSpace(opts.ChainTitle))
	}

	if info, err := os.Stat(dbPath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("database path %s is not a regular file", dbPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("database file %s: %w", dbPath, err)
	}

	db, err := openBolt(dbPath, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	existing, err := readExisting(db)
	if err != nil {
		return nil, err
	}

	var backupPath string
	if !existing.empty() {
		if !opts.Force {
			return nil, fmt.Errorf("database %s already contains %d entries in bucket %q; "+
				"refusing to overwrite without --force", dbPath, len(existing.keys), neutrino.TSSBucketName)
		}
		backupPath, err = writeOverwriteBackup(dbPath, existing)
		if err != nil {
			return nil, fmt.Errorf("write rollback backup: %w", err)
		}
	}

	keys := orderedKeys(a, values)
	if err := db.Update(func(tx *bolt.Tx) error {
		// Replace the whole bucket: the archive contains every key that was
		// exported, so anything else would be stale material from an older write.
		if err := tx.DeleteBucket([]byte(neutrino.TSSBucketName)); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		bucket, err := tx.CreateBucket([]byte(neutrino.TSSBucketName))
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := bucket.Put([]byte(key), values[key]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("write bucket %q to %s: %w", neutrino.TSSBucketName, dbPath, err)
	}

	// Read back through a fresh transaction: the write is not "successful" until
	// the stored bytes are proven identical to the archive.
	var stored *bucketData
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		stored, err = readBucket(tx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read back after import: %w", err)
	}
	if len(stored.keys) != len(values) {
		return nil, fmt.Errorf("read back after import: bucket has %d entries, archive has %d", len(stored.keys), len(values))
	}
	for _, key := range keys {
		if !bytes.Equal(stored.values[key], values[key]) {
			return nil, fmt.Errorf("read back after import: value of %q differs from the archive", key)
		}
	}

	storedDKG, err := decodeDKG(stored)
	if err != nil {
		return nil, fmt.Errorf("read back after import: %w", err)
	}
	storedPub, err := groupPubKey(storedDKG)
	if err != nil {
		return nil, fmt.Errorf("read back group public key after import: %w", err)
	}
	storedPubHex := hex.EncodeToString(storedPub.SerializeCompressed())
	storedRID := hex.EncodeToString(storedDKG.Rid)
	if meta.groupPubKey != "" && !strings.EqualFold(storedPubHex, meta.groupPubKey) {
		return nil, fmt.Errorf("read back after import: stored group public key %s != archive %s", storedPubHex, meta.groupPubKey)
	}
	if meta.rid != "" && !strings.EqualFold(storedRID, meta.rid) {
		return nil, fmt.Errorf("read back after import: stored rid %s != archive %s", storedRID, meta.rid)
	}

	return &importSummary{
		DBPath:      dbPath,
		GroupPubKey: storedPubHex,
		RID:         storedRID,
		ShareID:     shareID(storedDKG),
		Entries:     len(stored.keys),
		BackupPath:  backupPath,
	}, nil
}

type archiveMeta struct {
	groupPubKey string
	rid         string
	shareID     string
}

// loadArchive reads and structurally validates the JSON file.
func loadArchive(path string) (*archive, map[string][]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("archive file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("archive path %s is not a regular file", path)
	}
	if info.Size() > maxArchiveSize {
		return nil, nil, fmt.Errorf("archive file %s is too large (%d bytes, limit %d)", path, info.Size(), maxArchiveSize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read archive file %s: %w", path, err)
	}

	var a archive
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, nil, fmt.Errorf("parse archive JSON %s: %w", path, err)
	}
	if a.Version != archiveVersion {
		return nil, nil, fmt.Errorf("unsupported archive version %d (this tool supports version %d)", a.Version, archiveVersion)
	}
	if len(a.Entries) == 0 {
		return nil, nil, fmt.Errorf("archive %s has no entries", path)
	}

	values := make(map[string][]byte, len(a.Entries))
	for i, entry := range a.Entries {
		if entry.Key == "" {
			return nil, nil, fmt.Errorf("archive %s: entry %d has an empty key", path, i)
		}
		if !utf8.ValidString(entry.Key) {
			return nil, nil, fmt.Errorf("archive %s: entry %d key is not valid UTF-8", path, i)
		}
		if _, dup := values[entry.Key]; dup {
			return nil, nil, fmt.Errorf("archive %s: duplicate key %q", path, entry.Key)
		}
		value, err := base64.StdEncoding.DecodeString(entry.ValueBase64)
		if err != nil {
			return nil, nil, fmt.Errorf("archive %s: entry %q: invalid base64: %w", path, entry.Key, err)
		}
		values[entry.Key] = value
	}
	return &a, values, nil
}

// validateArchive checks the archive's own metadata against its DKG entry, so a
// corrupted or mismatched file is rejected before the database is touched.
func validateArchive(a *archive, values map[string][]byte, warn io.Writer) (*archiveMeta, error) {
	dkg, err := decodeDKG(&bucketData{values: values})
	if err != nil {
		return nil, fmt.Errorf("archive: %w; it is not a TSS key-material backup", err)
	}
	pub, err := groupPubKey(dkg)
	if err != nil {
		return nil, fmt.Errorf("archive entry %q has an invalid group public key: %w", neutrino.TSSDKGResultKey, err)
	}
	meta := &archiveMeta{
		groupPubKey: hex.EncodeToString(pub.SerializeCompressed()),
		rid:         hex.EncodeToString(dkg.Rid),
		shareID:     shareID(dkg),
	}
	if a.GroupPubKey != "" && !strings.EqualFold(a.GroupPubKey, meta.groupPubKey) {
		return nil, fmt.Errorf("archive group_pubkey %s does not match its DKG entry (%s)", a.GroupPubKey, meta.groupPubKey)
	}
	if a.RID != "" && !strings.EqualFold(a.RID, meta.rid) {
		return nil, fmt.Errorf("archive rid %s does not match its DKG entry (%s)", a.RID, meta.rid)
	}
	if raw, ok := values[neutrino.TSSRefreshResultKey]; ok {
		var envelope struct {
			DkgRid string `json:"dkgRid"`
		}
		switch err := json.Unmarshal(raw, &envelope); {
		case err != nil:
			warnf(warn, "refresh entry does not decode (%v); the node will rerun refresh after restore", err)
		case envelope.DkgRid != "" && meta.rid != "" && !strings.EqualFold(envelope.DkgRid, meta.rid):
			warnf(warn, "refresh entry belongs to DKG rid %s, not %s; the node will rerun refresh after restore",
				envelope.DkgRid, meta.rid)
		}
	}
	if a.ShareID != "" {
		if meta.shareID == "" {
			warnf(warn, "archive share_id %q cannot be verified: the DKG entry does not identify this node's share", a.ShareID)
		} else if a.ShareID != meta.shareID {
			return nil, fmt.Errorf("archive share_id %q does not match this share (%s)", a.ShareID, meta.shareID)
		}
	}
	meta.groupPubKey = a.GroupPubKey
	if meta.groupPubKey == "" {
		meta.groupPubKey = hex.EncodeToString(pub.SerializeCompressed())
	}
	meta.rid = a.RID
	if meta.rid == "" {
		meta.rid = hex.EncodeToString(dkg.Rid)
	}
	return meta, nil
}

func orderedKeys(a *archive, values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, e := range a.Entries {
		if !seen[e.Key] {
			seen[e.Key] = true
			keys = append(keys, e.Key)
		}
	}
	return keys
}

func openBolt(path string, readOnly bool) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{
		Timeout:  dbOpenTimeout,
		ReadOnly: readOnly,
	})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, fmt.Errorf("open %s: database is locked by another process "+
				"(stop the node before running this tool; bbolt permits a single writer): %w", path, err)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// readBucket returns every key/value pair in the TSS bucket. The bucket must exist.
func readBucket(tx *bolt.Tx) (*bucketData, error) {
	bucket := tx.Bucket([]byte(neutrino.TSSBucketName))
	if bucket == nil {
		return nil, fmt.Errorf("bucket %q not found (is this the node's neutrino.db?)", neutrino.TSSBucketName)
	}
	data := &bucketData{values: make(map[string][]byte)}
	err := bucket.ForEach(func(k, v []byte) error {
		if !utf8.Valid(k) {
			return fmt.Errorf("bucket key %x is not valid UTF-8", k)
		}
		if bucket.Bucket(k) != nil {
			return fmt.Errorf("bucket %q contains a nested bucket %q; this tool only supports flat key/value entries",
				neutrino.TSSBucketName, k)
		}
		key := string(k)
		data.keys = append(data.keys, key)
		data.values[key] = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read bucket %q: %w", neutrino.TSSBucketName, err)
	}
	return data, nil
}

// readExisting returns the bucket contents, or an empty snapshot when the bucket
// does not exist yet (first import into a fresh database).
func readExisting(db *bolt.DB) (*bucketData, error) {
	var data *bucketData
	err := db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(neutrino.TSSBucketName))
		if bucket == nil {
			data = &bucketData{values: make(map[string][]byte)}
			return nil
		}
		var err error
		data, err = readBucket(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// writeOverwriteBackup copies the current bucket into <db>.tss-backup-<unix>
// using the same archive format, so it can be fed back to "import --force".
func writeOverwriteBackup(dbPath string, existing *bucketData) (string, error) {
	backupPath := fmt.Sprintf("%s.tss-backup-%d", dbPath, time.Now().Unix())
	if _, err := os.Stat(backupPath); err == nil {
		return "", fmt.Errorf("backup file %s already exists", backupPath)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	a := &archive{
		Version:    archiveVersion,
		ExportedAt: time.Now().UTC(),
		Entries:    make([]archiveEntry, 0, len(existing.keys)),
	}
	// Metadata is best effort: even a bucket whose DKG entry is corrupt must be
	// preserved verbatim before the overwrite.
	if raw, ok := existing.values[neutrino.TSSDKGResultKey]; ok {
		var dkg cggmp.DKGResult
		if json.Unmarshal(raw, &dkg) == nil {
			a.RID = hex.EncodeToString(dkg.Rid)
			a.ShareID = shareID(&dkg)
			if pub, err := groupPubKey(&dkg); err == nil {
				a.GroupPubKey = hex.EncodeToString(pub.SerializeCompressed())
			}
		}
	}
	for _, key := range existing.keys {
		a.Entries = append(a.Entries, archiveEntry{
			Key:         key,
			ValueBase64: base64.StdEncoding.EncodeToString(existing.values[key]),
		})
	}
	if err := writeArchiveAtomic(backupPath, a); err != nil {
		return "", err
	}
	return backupPath, nil
}

// writeArchiveAtomic writes the archive to a 0600 temp file in the destination
// directory and renames it into place, so a crash never leaves a partial file at
// the final path.
func writeArchiveAtomic(path string, a *archive) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".rgb-tss-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		tmp.Close()
		return fmt.Errorf("write archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename archive into place: %w", err)
	}
	// Best effort: make the rename durable. Not all platforms support syncing a
	// directory, so a failure here is not fatal.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func decodeDKG(data *bucketData) (*cggmp.DKGResult, error) {
	raw, ok := data.values[neutrino.TSSDKGResultKey]
	if !ok {
		return nil, fmt.Errorf("no %q entry; the TSS share is missing", neutrino.TSSDKGResultKey)
	}
	var dkg cggmp.DKGResult
	if err := json.Unmarshal(raw, &dkg); err != nil {
		return nil, fmt.Errorf("decode %q: %w", neutrino.TSSDKGResultKey, err)
	}
	if len(dkg.Share) == 0 {
		return nil, fmt.Errorf("decode %q: the record carries no share", neutrino.TSSDKGResultKey)
	}
	var scalar btcec.ModNScalar
	if overflow := scalar.SetByteSlice(dkg.Share); overflow || scalar.IsZero() {
		return nil, fmt.Errorf("decode %q: the share is not a valid secp256k1 scalar", neutrino.TSSDKGResultKey)
	}
	return &dkg, nil
}

// groupPubKey uses the same parser as the node's groupPubKeyFromDKG.
//
// ParseBtcecPublicKey only checks that the coordinates fit in the field; it does
// not check curve membership. Round-trip through the validated parser so a
// corrupted archive is rejected here instead of being written to the database.
func groupPubKey(dkg *cggmp.DKGResult) (*btcec.PublicKey, error) {
	if dkg == nil {
		return nil, errors.New("nil DKG result")
	}
	pub, err := ctss.ParseBtcecPublicKey(&ctss.DKGResult{PubX: dkg.PubX, PubY: dkg.PubY})
	if err != nil {
		return nil, err
	}
	if _, err := btcec.ParsePubKey(pub.SerializeUncompressed()); err != nil {
		return nil, fmt.Errorf("point is not on the secp256k1 curve: %w", err)
	}
	return pub, nil
}

// shareID identifies which participant this node's share belongs to. The DKG
// result stores every participant's g^{share_i} (PartialPubKeys) but not its own
// id, so derive the local g^{share} from the share and match it. Returns "" when
// the material does not let us identify the node (e.g. PartialPubKeys absent).
func shareID(dkg *cggmp.DKGResult) string {
	if dkg == nil || len(dkg.Share) == 0 || len(dkg.PartialPubKeys) == 0 {
		return ""
	}
	_, pub := btcec.PrivKeyFromBytes(dkg.Share)
	ids := make([]string, 0, len(dkg.PartialPubKeys))
	for id := range dkg.PartialPubKeys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		point := dkg.PartialPubKeys[id]
		if point == nil {
			continue
		}
		if new(big.Int).SetBytes(point.X).Cmp(pub.X()) == 0 &&
			new(big.Int).SetBytes(point.Y).Cmp(pub.Y()) == 0 {
			return id
		}
	}
	return ""
}

func sameFile(a, b string) bool {
	// Catch symlinks/hardlinks to the same file when both paths exist.
	if ai, err := os.Stat(a); err == nil {
		if bi, err := os.Stat(b); err == nil && os.SameFile(ai, bi) {
			return true
		}
	}
	aa, err := filepath.Abs(a)
	if err != nil {
		return a == b
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return a == b
	}
	return aa == bb
}
