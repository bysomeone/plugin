package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/33cn/chain33/system/crypto/tss/cggmp"
	"github.com/33cn/plugin/plugin/dapp/lightclient/rpc/lightclient/neutrino"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb"
	"github.com/stretchr/testify/require"
)

// testDKG builds a DKG result whose Share corresponds to the PartialPubKeys
// entry for peer-self. That is the same relationship cggmp.exchangePartialPubKeys
// produces, and it is what lets the tool derive the share id.
func testDKG(t *testing.T, seed byte) (*cggmp.DKGResult, string) {
	t.Helper()
	groupPriv, groupPub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{seed}, 32))
	sharePriv, sharePub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{seed + 1}, 32))
	otherPriv, otherPub := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{seed + 2}, 32))
	_ = groupPriv
	_ = otherPriv

	dkg := &cggmp.DKGResult{
		PubX:  groupPub.X().Bytes(),
		PubY:  groupPub.Y().Bytes(),
		Share: sharePriv.Serialize(),
		Rid:   bytes.Repeat([]byte{seed}, 32),
		PartialPubKeys: map[string]*cggmp.ECPoint{
			"peer-self": {X: sharePub.X().Bytes(), Y: sharePub.Y().Bytes()},
			"peer-a":    {X: otherPub.X().Bytes(), Y: otherPub.Y().Bytes()},
		},
	}
	return dkg, hex.EncodeToString(groupPub.SerializeCompressed())
}

type testRefreshRecord struct {
	DkgRid string                 `json:"dkgRid"`
	Result map[string]interface{} `json:"result"`
}

func testRefresh(t *testing.T, rid []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(testRefreshRecord{
		DkgRid: hex.EncodeToString(rid),
		Result: map[string]interface{}{"share": "AA=="},
	})
	require.NoError(t, err)
	return raw
}

// createTestDB writes the bucket with the same walletdb/bdb path the node uses
// (walletdb.Update + CreateTopLevelBucket), not with a hand-rolled file format.
func createTestDB(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	db, err := walletdb.Create("bdb", path, false, 5*time.Second, false)
	require.NoError(t, err)
	require.NoError(t, walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte(neutrino.TSSBucketName))
		if err != nil {
			return err
		}
		for key, value := range entries {
			if err := bucket.Put([]byte(key), value); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, db.Close())
}

func readDBEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	db, err := walletdb.Open("bdb", path, true, 5*time.Second, false)
	require.NoError(t, err)
	defer db.Close()

	entries := make(map[string][]byte)
	require.NoError(t, walletdb.View(db, func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte(neutrino.TSSBucketName))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			entries[string(k)] = append([]byte(nil), v...)
			return nil
		})
	}))
	return entries
}

func writeArchiveFixture(t *testing.T, dir string) (path string, dkgJSON, refreshJSON []byte, groupPubKey string) {
	t.Helper()
	dkg, pub := testDKG(t, 0x11)
	dkgJSON, err := json.Marshal(dkg)
	require.NoError(t, err)
	refreshJSON = testRefresh(t, dkg.Rid)
	src := filepath.Join(dir, "source.db")
	createTestDB(t, src, map[string][]byte{
		neutrino.TSSDKGResultKey:     dkgJSON,
		neutrino.TSSRefreshResultKey: refreshJSON,
	})
	path = filepath.Join(dir, "archive.json")
	_, err = exportTSS(exportOptions{DBPath: src, OutPath: path, ChainTitle: "test-chain", Warn: io.Discard})
	require.NoError(t, err)
	return path, dkgJSON, refreshJSON, pub
}

func TestExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archivePath, dkgJSON, refreshJSON, wantPub := writeArchiveFixture(t, dir)

	info, err := os.Stat(archivePath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "archive must be 0600")

	raw, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	var a archive
	require.NoError(t, json.Unmarshal(raw, &a))
	require.Equal(t, archiveVersion, a.Version)
	require.Equal(t, "test-chain", a.ChainTitle)
	require.Equal(t, wantPub, a.GroupPubKey)
	require.NotEmpty(t, a.RID)
	require.Equal(t, "peer-self", a.ShareID)
	require.Len(t, a.Entries, 2)
	require.False(t, a.ExportedAt.IsZero())

	dst := filepath.Join(dir, "restored.db")
	summary, err := importTSS(importOptions{FilePath: archivePath, DBPath: dst, Warn: io.Discard})
	require.NoError(t, err)
	require.Equal(t, 2, summary.Entries)
	require.Equal(t, wantPub, summary.GroupPubKey)
	require.Equal(t, a.RID, summary.RID)
	require.Equal(t, "peer-self", summary.ShareID)
	require.Empty(t, summary.BackupPath)

	stored := readDBEntries(t, dst)
	require.Len(t, stored, 2)
	require.Equal(t, dkgJSON, stored[neutrino.TSSDKGResultKey])
	require.Equal(t, refreshJSON, stored[neutrino.TSSRefreshResultKey])
	require.Equal(t, readDBEntries(t, filepath.Join(dir, "source.db")), stored,
		"restored bucket must be byte-for-byte identical to the source bucket")
}

func TestImportRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	archivePath, _, _, _ := writeArchiveFixture(t, dir)

	oldDKG, _ := testDKG(t, 0x31)
	oldJSON, err := json.Marshal(oldDKG)
	require.NoError(t, err)
	target := filepath.Join(dir, "target.db")
	createTestDB(t, target, map[string][]byte{neutrino.TSSDKGResultKey: oldJSON})

	_, err = importTSS(importOptions{FilePath: archivePath, DBPath: target, Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
	require.Equal(t, oldJSON, readDBEntries(t, target)[neutrino.TSSDKGResultKey], "refused import must not modify the database")
}

func TestImportForceOverwritesAndWritesBackup(t *testing.T) {
	dir := t.TempDir()
	archivePath, dkgJSON, refreshJSON, _ := writeArchiveFixture(t, dir)

	oldDKG, _ := testDKG(t, 0x41)
	oldJSON, err := json.Marshal(oldDKG)
	require.NoError(t, err)
	target := filepath.Join(dir, "target.db")
	createTestDB(t, target, map[string][]byte{
		neutrino.TSSDKGResultKey: oldJSON,
		"stale-key":              []byte("stale"),
	})

	summary, err := importTSS(importOptions{FilePath: archivePath, DBPath: target, Force: true, Warn: io.Discard})
	require.NoError(t, err)
	require.NotEmpty(t, summary.BackupPath)

	stored := readDBEntries(t, target)
	require.Equal(t, dkgJSON, stored[neutrino.TSSDKGResultKey])
	require.Equal(t, refreshJSON, stored[neutrino.TSSRefreshResultKey])
	_, stale := stored["stale-key"]
	require.False(t, stale, "bucket is replaced, so keys absent from the archive must be gone")

	backupInfo, err := os.Stat(summary.BackupPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), backupInfo.Mode().Perm(), "rollback backup must be 0600")
	raw, err := os.ReadFile(summary.BackupPath)
	require.NoError(t, err)
	var backupArchive archive
	require.NoError(t, json.Unmarshal(raw, &backupArchive))
	var foundOld bool
	for _, entry := range backupArchive.Entries {
		if entry.Key == neutrino.TSSDKGResultKey {
			value, err := base64.StdEncoding.DecodeString(entry.ValueBase64)
			require.NoError(t, err)
			require.Equal(t, oldJSON, value)
			foundOld = true
		}
	}
	require.True(t, foundOld, "backup must contain the previous DKG value")
}

func TestImportRejectsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := importTSS(importOptions{FilePath: path, DBPath: filepath.Join(dir, "target.db"), Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse archive JSON")
	_, statErr := os.Stat(filepath.Join(dir, "target.db"))
	require.True(t, os.IsNotExist(statErr), "a rejected archive must not create the target database")
}

func TestImportRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":99,"entries":[{"key":"x","value_base64":""}]}`), 0o600))

	_, err := importTSS(importOptions{FilePath: path, DBPath: filepath.Join(dir, "target.db"), Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported archive version 99")
}

func TestImportRejectsMissingDKGMaterial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-dkg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"entries":[{"key":"something","value_base64":""}]}`), 0o600))

	_, err := importTSS(importOptions{FilePath: path, DBPath: filepath.Join(dir, "target.db"), Warn: io.Discard})
	require.Error(t, err)
	require.Contains(t, err.Error(), "it is not a TSS key-material backup")
}

func TestExportRequiresExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "archive.json")

	_, err := exportTSS(exportOptions{
		DBPath:  filepath.Join(dir, "missing.db"),
		OutPath: out,
		Warn:    io.Discard,
	})
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "missing.db"))
	require.True(t, os.IsNotExist(statErr), "export must not create a database")
	_, statErr = os.Stat(out)
	require.True(t, os.IsNotExist(statErr), "failed export must not leave an archive")
}

func TestExportDoesNotPrintSecretMaterial(t *testing.T) {
	dir := t.TempDir()
	dkg, _ := testDKG(t, 0x51)
	dkgJSON, err := json.Marshal(dkg)
	require.NoError(t, err)
	src := filepath.Join(dir, "source.db")
	createTestDB(t, src, map[string][]byte{neutrino.TSSDKGResultKey: dkgJSON})
	out := filepath.Join(dir, "archive.json")

	var stdout, stderr bytes.Buffer
	code := run([]string{"export", "--db", src, "--out", out}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())

	printed := stdout.String()
	require.Contains(t, printed, "share id: peer-self")
	require.NotContains(t, printed, hex.EncodeToString(dkg.Share), "stdout must not contain the share")
	require.NotContains(t, printed, base64.StdEncoding.EncodeToString(dkg.Share), "stdout must not contain the share")
	require.False(t, strings.Contains(printed, base64.StdEncoding.EncodeToString(dkgJSON)),
		"stdout must not contain the serialized DKG entry")
}

func TestRunUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, 2, run(nil, &stdout, &stderr))
	require.Contains(t, stderr.String(), "Usage:")

	stdout.Reset()
	stderr.Reset()
	require.Equal(t, 2, run([]string{"bogus"}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "unknown command")

	stdout.Reset()
	stderr.Reset()
	require.Equal(t, 2, run([]string{"export", "--db", "x"}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "--db and --out are required")
}

func TestRunImportRejectsCorruptArchiveWithNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{"import", "--file", path, "--db", filepath.Join(dir, "target.db")}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "parse archive JSON")
}
