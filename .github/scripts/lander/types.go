package lander

import (
	"context"
	"log/slog"
	"time"

	"gosuda.org/0/scripts/capy"
)

// Status is a commit status published on landing request tips.
type Status struct {
	State       string // pending, success, failure, error
	Context     string // for example suda/land
	Description string
}

// PullRequest is the subset of a pull request the lander consumes.
type PullRequest struct {
	Number  int
	HeadRef string
	HeadSHA string
	Draft   bool
	Title   string
	URL     string // web address
	Author  string // login of the opener
}

// MergedPull is a pull request that was merged on GitHub.
type MergedPull struct {
	Number   int
	HeadRef  string
	HeadSHA  string // head when the pull request was merged
	MergeSHA string // commit the merge set the base branch to
}

// Forge abstracts the GitHub API surface the lander needs. All identity answers come from
// GitHub-authenticated sources; none derive from commit metadata.
type Forge interface {
	PushForTip(ctx context.Context, repo, ref, sha string) (capy.Push, error)
	// PushChain lists, oldest first, the updates that moved ref from stop to tip with the actor of
	// each. It wraps capy.ErrActorNotVisible when the chain is incomplete.
	PushChain(ctx context.Context, repo, ref, stop, tip string) ([]capy.RefPush, error)
	ApproversForTip(ctx context.Context, repo, head, sha string) ([]string, error)
	TeamResolver(ctx context.Context) capy.TeamResolver
	SetStatus(ctx context.Context, repo, sha string, st Status) error
	OpenPulls(ctx context.Context, repo string) ([]PullRequest, error)
	// ClosePull closes a pull request and then posts comment on it (pull_requests:write). A
	// failed comment leaves the pull request closed; closing is never repeated.
	ClosePull(ctx context.Context, repo string, number int, comment string) error
	// CreatePull opens a pull request from head into base (both branches of repo) and returns its
	// number (pull_requests:write).
	CreatePull(ctx context.Context, repo, head, base, title, body string) (int, error)
	// MergedPulls lists the most recently updated merged pull requests of repo (pull_requests:read).
	MergedPulls(ctx context.Context, repo string) ([]MergedPull, error)
	// UpdatePull replaces the title and body of a pull request (pull_requests:write).
	UpdatePull(ctx context.Context, repo string, number int, title, body string) error
}

// PresubmitInput describes a candidate landing.
type PresubmitInput struct {
	Store    *Store
	BaseTree string   // current trunk root tree
	NewTree  string   // merged candidate root tree
	Changed  []string // changed paths (BaseTree -> NewTree)
}

// PresubmitResult is the verdict of a presubmit run. Summary is short and safe to publish in a
// commit status; Details stay in the lander log.
type PresubmitResult struct {
	Passed  bool
	Summary string
	Details string
}

// Presubmit validates a candidate tree before it lands. Implementations must never execute
// repository code in a process that holds credentials.
type Presubmit interface {
	Run(ctx context.Context, in PresubmitInput) (PresubmitResult, error)
}

// Config wires the lander to its remotes and collaborators.
type Config struct {
	Store *Store
	Forge Forge

	Trunk Remote // gosuda/0
	User  Remote // gosuda/1
	// Mirror maps a destination repository ("owner/name") to its remote.
	Mirror func(repo string) Remote

	Presubmit Presubmit // nil skips presubmit

	// HistoryDepth bounds the base-provenance search over refs/suda/history.
	HistoryDepth int
	// MaxLandAttempts bounds retries after a lease failure.
	MaxLandAttempts int
	// WorkspaceTTL is the age after which refs/ws/* in the user repository are pruned.
	WorkspaceTTL time.Duration
	// EmbargoTeam ("org/slug") may land embargoed changes (ids "sec-...", see embargo.go). When
	// empty, embargoed changes cannot be landed at all.
	EmbargoTeam string
	// EmbargoTTL is the age after which embargoed workspace references are pruned (default 180 days).
	EmbargoTTL time.Duration
	// MagpieURL (for example "https://magpie.gosuda.org") is the base of the diff links in the pull
	// requests the lander opens for imported mirror pull requests. Empty omits those links.
	MagpieURL string
	// AckTip, when set, accepts that unsigned refs/suda/history tip once.
	AckTip string
	DryRun bool
	// MirrorInterval bounds how often mirrors are fetched and reconciled. Zero means every cycle.
	// A landing that advanced the trunk forces the next cycle to reconcile immediately.
	MirrorInterval time.Duration

	Now func() time.Time
	Log *slog.Logger
}

func (c *Config) defaults() {
	if c.HistoryDepth == 0 {
		c.HistoryDepth = 500
	}
	if c.MaxLandAttempts == 0 {
		c.MaxLandAttempts = 3
	}
	if c.WorkspaceTTL == 0 {
		c.WorkspaceTTL = 14 * 24 * time.Hour
	}
	if c.EmbargoTTL == 0 {
		c.EmbargoTTL = 180 * 24 * time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Store.Log == nil {
		c.Store.Log = c.Log
	}
}

// Store ref namespaces. Every remote maps into a disjoint namespace of the shared object store.
const (
	trunkHeads   = "refs/trunk/heads/"
	trunkSuda    = "refs/trunk/suda/"
	trunkMain    = trunkSuda + "main"
	trunkHistory = trunkSuda + "history"
	trunkSync    = trunkSuda + "sync/"
	userLand     = "refs/user/land/"
	userReview   = "refs/user/review/"
	userBase     = "refs/user/base/"
	userSuda     = "refs/user/suda/"
	userMain     = "refs/user/main"
	userStaged   = "refs/user/staged/"
	mirrorNS     = "refs/mirror/"
)
