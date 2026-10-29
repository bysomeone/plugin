# rgb-tss-tool

Offline export/import of one node's CGGMP key material from `neutrino.db`
(bucket `rgbx-tss`, keys `cggmp-dkg-result` and `cggmp-refresh-result`).

The point is to avoid backing up the whole multi-GB `neutrino.db`: the SPV header
chain and filters are reproducible, the TSS share is not. The archive is a few KB.

## Build

From the repository root:

```
go build -o rgb-tss-tool ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-tss-tool
```

## What the archive contains

JSON, mode `0600`, written through a temp file and renamed into place:

```json
{
  "version": 1,
  "chain_title": "bityuan",
  "group_pubkey": "02...",
  "rid": "ab...",
  "share_id": "12D3KooW...",
  "exported_at": "2026-10-24T12:00:00Z",
  "entries": [
    {"key": "cggmp-dkg-result", "value_base64": "..."},
    {"key": "cggmp-refresh-result", "value_base64": "..."}
  ]
}
```

`entries` are the raw bucket values, unchanged. The values use the node's own
serialization (`cggmp.DKGResult` / the refresh envelope); the tool does not
introduce a second key-material format. `group_pubkey`, `rid` and `share_id` are
derived from the DKG entry for identification and are re-verified on import.
`share_id` is the participant id whose `g^{share}` matches this node's share; it
is empty if the DKG record cannot identify it. `chain_title` is informational
metadata supplied with `--chain-title`; it is not cryptographically bound.

## Usage

The node must be **stopped** for both commands. `neutrino.db` is bbolt, which
allows a single writer; the tool fails with a "database is locked" error if the
node still holds it. Export opens the DB read-only, but bbolt's writer lock still
conflicts with a running node.

### Export

```
rgb-tss-tool export --db /root/paradatadir/lightclient/neutrino.db \
                    --out /backup/para1-tss.json \
                    --chain-title bityuan
```

The database must already exist: a mistyped `--db` is an error, not a new empty
database. stdout prints the group public key, rid, share id and entry count. It
never prints share or Paillier material.

A corrupt refresh entry produces a warning and is exported verbatim; a DKG entry
that cannot be decoded is a hard error, because then the archive cannot be
identified or verified.

### Import

```
rgb-tss-tool import --file /backup/para1-tss.json \
                    --db /root/paradatadir/lightclient/neutrino.db
```

- The target database is created if absent (bbolt default).
- If the target bucket already has entries, import is refused unless `--force` is
  given.
- With `--force`, the existing bucket is first written to
  `<db>.tss-backup-<unix>` (same directory, `0600`, same archive format) so the
  overwrite can be rolled back.
- The bucket is replaced as a whole, then read back in a new transaction and
  compared byte-for-byte with the archive. Group public key, rid and share id are
  verified again from the stored DKG entry. Any mismatch is a non-zero exit.

Roll back an overwrite:

```
rgb-tss-tool import --file <db>.tss-backup-<unix> --db <db> --force
```

## Limits

- **One archive is one node's share.** A signing group still needs at least
  `threshold` shares — currently 3 of 4 — with the same `rid` / group public key.
  Back up every node separately; mixing shares from different DKG runs produces a
  set that cannot sign.
- **The share is not enough to rejoin as the same member.** The node's libp2p
  identity private key must also be restored, otherwise its peer id changes and
  the group does not recognize it (see `BACKUP_RECOVERY.md` §2.1, item B5).
- The archive is **plaintext key material**. Store it with the same care as the
  database: `0600`, encrypted at rest, off the node.
- The refresh record can be rerun if lost; the DKG result (the share) cannot. The
  tool always refuses to export without a decodable DKG entry.
- This tool does not check that the share matches the chain's `CrossChainInfo`;
  that check remains at node startup (`CONFIG.md` §4.3.1).

## Tests

```
go test -count=1 ./plugin/dapp/lightclient/rpc/lightclient/neutrino/cmd/rgb-tss-tool
```

The tests create temporary databases with the node's own walletdb/bdb write path
and cover round-trip byte equality, `0600` permissions, refusal without
`--force`, corrupt JSON, unsupported versions, `--force` overwrite with rollback
backup, export of a missing database, and that stdout never contains secret
material. No Docker or running node is involved.
