# Backup, At-Rest Encryption, WAL Archiving & PITR

SuperSQL supports online hot backups, incremental backup chains, continuous WAL archiving, Point-in-Time Recovery (PITR), and optional pure-Go **AES-256-GCM** (`NSE1`) encryption for both live storage and backups.

---

## 1. Pure-Go AES-256-GCM At-Rest Encryption

Provide a 32-byte key in `OpenOptions.EncryptionKey` (generate with `crypto/rand`, store in your KMS/secret manager—SuperSQL never writes the raw key to disk):

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory:     "/var/lib/myapp/db",
	Clock:         localclock.New(),
	EncryptionKey: liveKey32Bytes,
})
```

- All newly written WAL segments (`commits/*.wal`) and PAX v2 SST runs (`runs/*.sst`) are transparently encrypted in authenticated 64 KiB `NSE1` AES-256-GCM chunks bound with chunk-index AAD.
- `encryption.meta` stores only a key fingerprint so opening an encrypted directory with a missing or wrong key fails fast with `supersql.ErrInvalidEncryptionKey`.
- Existing unencrypted directories remain byte-for-byte compatible when `EncryptionKey == nil`.

---

## 2. Online Hot Backups (Plain or Encrypted)

Hot backups capture a consistent snapshot without blocking readers or writers, including MVCC catalog descriptors, roles, ACLs, RLS policies, and `fence.secret`:

```go
// Unencrypted hot backup
err := db.Backup(ctx, "/backups/full-2026-05-01")

// Encrypted hot backup (can use a separate backup encryption key)
err = db.CreateHotBackupWithOptions(ctx, "/backups/full-enc-2026-05-01", supersql.BackupOptions{
	EncryptionKey: backupKey32Bytes,
})
```

Restoring a checkpoint into an empty destination directory:

```go
// Unencrypted backup (pass LiveEncryptionKey via OpenOptions if the live DB itself was encrypted)
err := supersql.RestoreCheckpoint(ctx, "/backups/full-2026-05-01", "/restore/db", supersql.OpenOptions{
	Clock:         localclock.New(),
	EncryptionKey: liveKey32Bytes, // nil if live DB was unencrypted
})

// Encrypted backup
err = supersql.RestoreEncryptedCheckpoint(ctx, "/backups/full-enc-2026-05-01", "/restore/db", supersql.OpenOptions{
	Clock:         localclock.New(),
	EncryptionKey: liveKey32Bytes, // nil if live DB was unencrypted
}, backupKey32Bytes)
```

---

## 3. Incremental Backup Chains

Incremental backups copy only SST runs and WAL segments added since a base backup (`incremental.json`), minimizing backup I/O and storage:

```go
// 1. Take a full base backup
err := db.CreateHotBackupWithOptions(ctx, baseDir, supersql.BackupOptions{
	EncryptionKey: backupKey,
})

// 2. After additional writes, take incremental hop 1 (and optionally hop 2...)
info1, err := db.CreateIncrementalBackup(ctx, baseDir, inc1Dir, supersql.IncrementalBackupOptions{
	EncryptionKey: backupKey,
})
info2, err := db.CreateIncrementalBackup(ctx, inc1Dir, inc2Dir, supersql.IncrementalBackupOptions{
	EncryptionKey: backupKey,
})

// 3. Restore the full chain in order: [base, inc1, inc2]
err = supersql.RestoreIncremental(ctx, []string{baseDir, inc1Dir, inc2Dir}, restoreDir, supersql.IncrementalRestoreOptions{
	Open:          supersql.OpenOptions{Clock: clk, EncryptionKey: liveKey},
	EncryptionKey: backupKey,
})
```

---

## 4. Continuous WAL Archiving & Point-in-Time Recovery (PITR)

Enable WAL archiving either at `Open` time or dynamically on a live `*Catalog`:

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory:     dataDir,
	Clock:         clk,
	EncryptionKey: liveKey,       // optional
	WALArchiveDir: walArchiveDir, // continuously archives every committed WAL segment
	WALArchiveKey: archiveKey,    // optional 32-byte key for archived WAL files
})

// Or attach/change WAL archiving at runtime:
err = db.EnableWALArchive(walArchiveDir, archiveKey)
```

To recover a database to an exact timestamp (or log position) from a base backup + archived WAL directory:

```go
res, err := supersql.RestorePITR(ctx, baseDir, walArchiveDir, restoreDir, supersql.PITRRestoreOptions{
	Open:            supersql.OpenOptions{Clock: clk, EncryptionKey: liveKey},
	EncryptionKey:   backupKey,  // key used for baseDir backup (if encrypted)
	WALArchiveKey:   archiveKey, // key used for WAL archive (if encrypted)
	TargetTimestamp: targetTime, // replays WAL segments up to targetTime
	// TargetLogPosition: 42,   // alternative: exact log-position cutoff
})
```

For an incremental chain + WAL archive, call `supersql.RestorePITRFromChain(ctx, []string{baseDir, inc1Dir}, walArchiveDir, restoreDir, opts)`.
