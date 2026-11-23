# Backup, At-Rest Encryption, WAL Archiving & PITR

SuperSQL supports online hot backups, incremental backup chains, continuous WAL
archiving, Point-in-Time Recovery (PITR), and pure-Go AES-256-GCM encryption.

The examples use a `context.Context` named `ctx`, an open `*supersql.Catalog`
named `db`, and these imports:

```go
import (
	"fmt"

	"github.com/gosuda/supersql-alpha"
	localclock "github.com/gosuda/supersql-alpha/clock/local"
)
```

Backup and restore destinations must be missing or empty, and their parent
directories must exist. Each restore function returns an open `*supersql.Catalog`;
close it when finished. Check every returned error before using the catalog.

## 1. At-Rest Encryption

Supply a 32-byte key through `OpenOptions.EncryptionKey`. Generate keys with
`crypto/rand` and keep them in your key manager; SuperSQL does not write the raw
key to disk.

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory:     "/var/lib/myapp/db",
	Clock:         localclock.New(),
	EncryptionKey: liveKey,
})
if err != nil {
	return err
}
defer db.Close()
```

New WAL segments (`commits/*.wal`) and SST runs (`runs/*.sst`) use authenticated
64 KiB `NSE1` AES-256-GCM chunks. `encryption.meta` records a key fingerprint;
opening an encrypted database with a missing or incorrect key returns
`supersql.ErrInvalidEncryptionKey`. Leave `EncryptionKey` empty for an
unencrypted database.

Live encryption and backup encryption have separate keys. A backup preserves
the source database's live encryption; backup encryption adds another layer to
the files copied into the backup. Restore therefore needs the original live key
as well as the backup key when both layers are enabled.

## 2. Online Hot Backups

Hot backups capture a consistent closed snapshot, including catalog descriptors,
roles, privileges, row security policies, and `fence.secret`, while the database
continues serving queries.

```go
if err := db.CreateHotBackup(ctx, plainBackupDir); err != nil {
	return err
}
if err := db.CreateHotBackupWithOptions(ctx, encryptedBackupDir, supersql.BackupOptions{
	EncryptionKey: backupKey,
}); err != nil {
	return err
}
```

Restore a backup without additional backup encryption:

```go
restored, err := supersql.RestoreCheckpoint(ctx, plainBackupDir, supersql.OpenOptions{
	Directory:     restoreDir,
	Clock:         localclock.New(),
	EncryptionKey: liveKey, // Empty if the source database was unencrypted.
})
if err != nil {
	return err
}
defer restored.Close()
```

Restore an encrypted backup by passing its key before `OpenOptions`:

```go
restored, err := supersql.RestoreEncryptedCheckpoint(ctx, encryptedBackupDir, backupKey, supersql.OpenOptions{
	Directory:     restoreDir,
	Clock:         localclock.New(),
	EncryptionKey: liveKey,
})
if err != nil {
	return err
}
defer restored.Close()
```

Restoration verifies file checksums and the certified read fence before
publishing the destination. It then opens the recovered catalog under the
readiness rules in `OpenOptions`.

## 3. Incremental Backup Chains

An incremental backup copies changed files and records unchanged files as
inherited references in `backup.manifest`. Keep the full backup and every
intermediate hop needed by the chain.

```go
if err := db.CreateHotBackupWithOptions(ctx, baseDir, supersql.BackupOptions{
	EncryptionKey: backupKey,
}); err != nil {
	return err
}

// Take each incremental backup after the next set of application writes.
if _, err := db.CreateIncrementalBackup(ctx, baseDir, supersql.IncrementalBackupOptions{
	Destination:   inc1Dir,
	EncryptionKey: backupKey,
}); err != nil {
	return err
}
if _, err := db.CreateIncrementalBackup(ctx, inc1Dir, supersql.IncrementalBackupOptions{
	Destination:   inc2Dir,
	EncryptionKey: backupKey,
}); err != nil {
	return err
}
```

Restore the complete chain in order:

```go
restored, err := supersql.RestoreIncremental(ctx, []string{baseDir, inc1Dir, inc2Dir}, supersql.IncrementalRestoreOptions{
	Open: supersql.OpenOptions{
		Directory:     restoreDir,
		Clock:         localclock.New(),
		EncryptionKey: liveKey,
	},
	EncryptionKey: backupKey,
})
if err != nil {
	return err
}
defer restored.Close()
```

The restore validates parent lineage, file ownership and checksums, encryption
keys, and the certified read fence. Use the same backup key for encrypted hops
in one chain; `IncrementalRestoreOptions` supplies one backup key.

## 4. WAL Archiving and Point-in-Time Recovery

Enable archiving when opening the live database:

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory:     dataDir,
	Clock:         localclock.New(),
	EncryptionKey: liveKey,
	WALArchiveDir: archiveDir,
	WALArchiveKey: archiveKey,
})
if err != nil {
	return err
}
defer db.Close()
```

Or enable it on an existing catalog:

```go
if err := db.EnableWALArchive(ctx, archiveDir, archiveKey); err != nil {
	return err
}
```

Archiving copies existing unretired WAL segments and subsequent durable commit
segments before their local copies can be retired. Enable it before taking the
base backup. A failed archive update leaves the previous destination active.
When `WALArchiveKey` is empty, an encrypted live database uses its
live key for archived WAL; an unencrypted database writes plaintext archives.

The PITR target is a `clock.Instant` in the database's clock scale. It is a
positional argument, not a field in `PITRRestoreOptions`. Recovery replays
contiguous WAL through the last commit at or before that target:

```go
restored, result, err := supersql.RestorePITR(ctx, baseDir, archiveDir, targetTS, supersql.PITRRestoreOptions{
	Open: supersql.OpenOptions{
		Directory:     restoreDir,
		Clock:         localclock.New(),
		EncryptionKey: liveKey,
	},
	EncryptionKey: backupKey,
	WALArchiveKey: archiveKey,
})
if err != nil {
	return err
}
defer restored.Close()
fmt.Printf("Restored through timestamp %d at log position %d\n",
	result.RecoveredCommitTS, result.RecoveredLogPosition)
```

Pass the archive's actual key explicitly when it differs from the backup key.
For a chain, use
`RestorePITRFromChain(ctx, []string{baseDir, inc1Dir, inc2Dir}, archiveDir, targetTS, opts)`
with the same `PITRRestoreOptions`.

PITR rejects targets earlier than the base snapshot, missing intermediate WAL,
corrupt segments, and inconsistent archive metadata. The first segment beyond
the target is also verified before its timestamp can establish the stopping
point. `PITRResult` reports the last recovered commit; a later requested timestamp
does not imply that the archive contains commits through that time.

Recovery bounds each decoded WAL body by `Open.Compaction.WALBytes`, using the
storage default when unset. Set this limit high enough to read the source
database's committed WAL segments. Encrypted archives are authenticated in
chunks and checked against the same plaintext limit.
