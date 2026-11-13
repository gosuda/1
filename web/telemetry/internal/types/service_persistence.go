package types

import (
	"context"
)

type PersistenceService interface {
	Ping(ctx context.Context) error

	RandflakeGC(ctx context.Context) error
	RandflakeLeaseCreate(ctx context.Context) (*RandflakeLease, error)
	RandflakeLeaseExtend(ctx context.Context, prev *RandflakeLease) (*RandflakeLease, error)

	ClientRegisterFingerprint(ctx context.Context, fpID int64, clientID int64, userAgent string, userAgentData string, fpversion int32, fphash string) error
	ClientLookupByID(ctx context.Context, clientID int64) (ClientIdentifier, error)
	ClientLookupByToken(ctx context.Context, token string) (ClientIdentifier, error)
	ClientVerifyToken(ctx context.Context, clientID int64, token string) (bool, error)
	ClientRegister(ctx context.Context, id int64, token string) error

	// URL-related methods
	UrlLookupByUrl(ctx context.Context, url string) (Url, error)
	UrlInsert(ctx context.Context, id int64, url string) error

	// View-related methods
	ViewInsertWithCount(ctx context.Context, id int64, urlID int64, clientID int64, countID int64) error
	ViewCountLookup(ctx context.Context, urlID int64) (ViewCount, error)

	// Like-related methods (mirrors view implementation; likes are read-heavy so no combined write+get helper on client)
	LikeInsertWithCount(ctx context.Context, id int64, urlID int64, clientID int64, countID int64) error
	LikeCountLookup(ctx context.Context, urlID int64) (LikeCount, error)

	// Bulk counts: return view and like counts for a list of normalized URLs
	BulkCountsByUrls(ctx context.Context, urls []string) ([]BulkCountEntry, error)
}
