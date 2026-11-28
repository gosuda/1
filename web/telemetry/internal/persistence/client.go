package persistence

import (
	"context"
	"database/sql"
	"time"

	"github.com/go-sql-driver/mysql"
	"telemetry.gosuda.org/telemetry/internal/persistence/database"
	"telemetry.gosuda.org/telemetry/internal/types"
)

func (g *PersistenceClient) ClientRegister(ctx context.Context, id int64, token string) error {
	return g.db.ClientRegister(ctx, database.ClientRegisterParams{
		ID:        id,
		Token:     token,
		CreatedAt: time.Now().UnixNano(),
	})
}

func (g *PersistenceClient) ClientLookupByID(ctx context.Context, id int64) (types.ClientIdentifier, error) {
	return g.db.ClientLookupByID(ctx, id)
}

func (g *PersistenceClient) ClientLookupByToken(ctx context.Context, token string) (types.ClientIdentifier, error) {
	return g.db.ClientLookupByToken(ctx, token)
}

func (g *PersistenceClient) ClientVerifyToken(ctx context.Context, id int64, token string) (bool, error) {
	ret, err := g.db.ClientVerifyToken(ctx, database.ClientVerifyTokenParams{
		ID:    id,
		Token: token,
	})
	if err != nil {
		return false, err
	}
	return ret == 1, nil
}

func (g *PersistenceClient) ClientRegisterFingerprint(
	ctx context.Context,
	fpID int64,
	clientID int64,
	userAgent string,
	userAgentData string,
	fpversion int32,
	fphash string,
) error {
	return g.db.ClientRegisterFingerprint(ctx, database.ClientRegisterFingerprintParams{
		ID:            fpID,
		ClientID:      clientID,
		UserAgent:     userAgent,
		UserAgentData: userAgentData,
		Fpversion:     fpversion,
		Fphash:        fphash,
		CreatedAt:     time.Now().UnixNano(),
	})
}

func (g *PersistenceClient) ViewInsertWithCount(ctx context.Context, id int64, urlID int64, clientID int64, countID int64) error {
	// Start a transaction
	tx, err := g.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Create a new queries instance using the transaction
	txQueries := database.New(tx)
	now := time.Now().UnixNano()

	// Insert the view
	err = txQueries.ViewInsert(ctx, database.ViewInsertParams{
		ID:        id,
		UrlID:     urlID,
		ClientID:  clientID,
		CreatedAt: now,
	})
	if err != nil {
		// If insert failed with duplicate entry (unlikely since no unique constraint), treat as no-op
		if me, ok := err.(*mysql.MySQLError); ok && me.Number == 1062 {
			return nil
		}
		return err
	}

	// Lookup view count row inside transaction. If none, insert; handle race by falling back to update on duplicate.
	_, err = txQueries.ViewCountLookup(ctx, urlID)
	if err != nil {
		if err == sql.ErrNoRows {
			// no count row; try to insert one
			err = txQueries.ViewCountInsert(ctx, database.ViewCountInsertParams{
				ID:        countID,
				UrlID:     urlID,
				UpdatedAt: now,
			})
			if err != nil {
				// If insert failed because a concurrent tx inserted it, try update
				if me, ok := err.(*mysql.MySQLError); ok && me.Number == 1062 {
					if err = txQueries.ViewCountUpdate(ctx, database.ViewCountUpdateParams{
						UpdatedAt: now,
						UrlID:     urlID,
					}); err != nil {
						return err
					}
				} else {
					return err
				}
			}
		} else {
			return err
		}
	} else {
		// count row exists -> update it
		if err = txQueries.ViewCountUpdate(ctx, database.ViewCountUpdateParams{
			UpdatedAt: now,
			UrlID:     urlID,
		}); err != nil {
			return err
		}
	}

	// Commit the transaction
	return tx.Commit()
}

func (g *PersistenceClient) UrlLookupByUrl(ctx context.Context, url string) (types.Url, error) {
	return g.db.UrlLookupByUrl(ctx, url)
}

func (g *PersistenceClient) UrlInsert(ctx context.Context, id int64, url string) error {
	return g.db.UrlInsert(ctx, database.UrlInsertParams{
		ID:        id,
		Url:       url,
		CreatedAt: time.Now().UnixNano(),
	})
}

func (g *PersistenceClient) ViewCountLookup(ctx context.Context, urlID int64) (types.ViewCount, error) {
	return g.db.ViewCountLookup(ctx, urlID)
}

func (g *PersistenceClient) LikeInsertWithCount(ctx context.Context, id int64, urlID int64, clientID int64, countID int64) error {
	// Start a transaction
	tx, err := g.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Create a new queries instance using the transaction
	txQueries := database.New(tx)
	now := time.Now().UnixNano()

	// Insert the like
	err = txQueries.LikeInsert(ctx, database.LikeInsertParams{
		ID:        id,
		UrlID:     urlID,
		ClientID:  clientID,
		CreatedAt: now,
	})
	if err != nil {
		// If this is a duplicate like (client already liked this URL), treat as idempotent no-op.
		if me, ok := err.(*mysql.MySQLError); ok && me.Number == 1062 {
			// Do not increment count when like already exists.
			return nil
		}
		return err
	}

	// Lookup like count row inside transaction. If none, insert; handle race by falling back to update on duplicate.
	_, err = txQueries.LikeCountLookup(ctx, urlID)
	if err != nil {
		if err == sql.ErrNoRows {
			// no count row; try to insert one
			err = txQueries.LikeCountInsert(ctx, database.LikeCountInsertParams{
				ID:        countID,
				UrlID:     urlID,
				UpdatedAt: now,
			})
			if err != nil {
				// If insert failed because a concurrent tx inserted it, try update
				if me, ok := err.(*mysql.MySQLError); ok && me.Number == 1062 {
					if err = txQueries.LikeCountUpdate(ctx, database.LikeCountUpdateParams{
						UpdatedAt: now,
						UrlID:     urlID,
					}); err != nil {
						return err
					}
				} else {
					return err
				}
			}
		} else {
			return err
		}
	} else {
		// count row exists -> update it
		if err = txQueries.LikeCountUpdate(ctx, database.LikeCountUpdateParams{
			UpdatedAt: now,
			UrlID:     urlID,
		}); err != nil {
			return err
		}
	}

	// Commit the transaction
	return tx.Commit()
}

func (g *PersistenceClient) LikeCountLookup(ctx context.Context, urlID int64) (types.LikeCount, error) {
	return g.db.LikeCountLookup(ctx, urlID)
}

// BulkCountsByUrls returns view and like counts for the provided normalized URLs.
// It delegates to the generated SQL helper and maps the result into types.BulkCountEntry.
func (g *PersistenceClient) BulkCountsByUrls(ctx context.Context, urls []string) ([]types.BulkCountEntry, error) {
	rows, err := g.db.BulkCountsByUrls(ctx, urls)
	if err != nil {
		return nil, err
	}
	out := make([]types.BulkCountEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, types.BulkCountEntry{
			URL:       r.Url,
			ViewCount: r.ViewCount,
			LikeCount: r.LikeCount,
		})
	}
	return out, nil
}
