package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"gosuda.org/0/scripts/capy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		cfgPath := fs.String("config", "capy.star", "path to capy.star")
		asJSON := fs.Bool("json", false, "emit JSON output")
		_ = fs.Parse(args)

		cfg, err := capy.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(cfg.Workflows)
			return
		}
		for _, wf := range cfg.Workflows {
			fmt.Printf("%s\t%s\t%s\t%s\n", wf.Name, wf.OriginPrefix, wf.DestinationRepo, wf.Branch)
		}

	case "check-visibility":
		fs := flag.NewFlagSet("check-visibility", flag.ExitOnError)
		repoDir := fs.String("repo", ".", "path to git repository")
		baseTreeOID := fs.String("base-tree", "", "optional base tree-ish to diff against")
		treeOID := fs.String("tree", "HEAD", "tree-ish to inspect")
		_ = fs.Parse(args)

		violations, err := capy.CheckVisibilityDiff(ctx, *repoDir, *baseTreeOID, *treeOID)
		if err != nil {
			fatal(err)
		}
		if len(violations) > 0 {
			for _, v := range violations {
				fmt.Fprintln(os.Stderr, v)
			}
			os.Exit(1)
		}

	case "check-owners":
		fs := flag.NewFlagSet("check-owners", flag.ExitOnError)
		repoDir := fs.String("repo", ".", "path to git repository")
		baseTreeOID := fs.String("base-tree", "", "base tree-ish (OWNERS are read only from here)")
		treeOID := fs.String("tree", "HEAD", "new tree-ish")
		requester := fs.String("requester", "", "login that requested the landing")
		approvers := fs.String("approvers", "", "comma-separated logins that approved the exact tip")
		var teams teamFlags
		fs.Var(&teams, "team", "static team membership org/slug=login,login (repeatable)")
		ghRepo := fs.String("github-repo", "", "resolve identity through the GitHub API on this owner/repo (needs GITHUB_TOKEN)")
		ghRef := fs.String("github-ref", "", "full ref whose tip push identifies the requester")
		ghSHA := fs.String("github-sha", "", "tip sha of -github-ref")
		ghReview := fs.String("github-review-head", "", "review branch (review/<id>) whose PR approvals count")
		_ = fs.Parse(args)

		id := capy.Identity{Requester: *requester}
		for _, a := range strings.Split(*approvers, ",") {
			if a = strings.TrimSpace(a); a != "" {
				id.Approvers = append(id.Approvers, a)
			}
		}
		var resolver capy.TeamResolver
		if len(teams) > 0 {
			resolver = teams.resolver()
		}
		if *ghRepo != "" {
			token := os.Getenv("GITHUB_TOKEN")
			if token == "" {
				fatal(fmt.Errorf("-github-repo requires GITHUB_TOKEN"))
			}
			gh := capy.NewGitHub(token)
			if id.Requester == "" {
				login, err := gh.PusherForTip(ctx, *ghRepo, *ghRef, *ghSHA)
				if err != nil {
					fatal(err)
				}
				id.Requester = login
			}
			if *ghReview != "" {
				got, err := gh.ApproversForTip(ctx, *ghRepo, *ghReview, *ghSHA)
				if err != nil {
					fatal(err)
				}
				id.Approvers = append(id.Approvers, got...)
			}
			if resolver == nil {
				resolver = gh.TeamResolver(ctx)
			}
		}
		fmt.Fprintf(os.Stderr, "check-owners: requester=%q approvers=%v\n", id.Requester, id.Approvers)

		violations, err := capy.CheckOwners(ctx, *repoDir, *baseTreeOID, *treeOID, id, resolver)
		if err != nil {
			fatal(err)
		}
		if len(violations) > 0 {
			for _, v := range violations {
				fmt.Fprintln(os.Stderr, v)
			}
			os.Exit(1)
		}

	case "export-tree":
		fs := flag.NewFlagSet("export-tree", flag.ExitOnError)
		repoDir := fs.String("repo", ".", "path to git repository")
		cfgPath := fs.String("config", "capy.star", "path to capy.star")
		wfName := fs.String("workflow", "", "workflow name")
		treeOID := fs.String("tree", "HEAD", "root tree-ish")
		_ = fs.Parse(args)

		cfg, err := capy.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		wf, ok := cfg.FindWorkflow(*wfName)
		if !ok {
			fatal(fmt.Errorf("unknown workflow %q", *wfName))
		}
		outOID, err := capy.TransformSubtree(ctx, *repoDir, *treeOID, wf)
		if err != nil {
			fatal(err)
		}
		fmt.Println(outOID)

	case "import-tree":
		fs := flag.NewFlagSet("import-tree", flag.ExitOnError)
		repoDir := fs.String("repo", ".", "path to git repository")
		cfgPath := fs.String("config", "capy.star", "path to capy.star")
		wfName := fs.String("workflow", "", "workflow name")
		baseTree := fs.String("base-tree", "", "monorepo root tree-ish at the last sync point (not the current trunk)")
		subTree := fs.String("sub-tree", "", "external repository root tree-ish")
		_ = fs.Parse(args)

		cfg, err := capy.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		wf, ok := cfg.FindWorkflow(*wfName)
		if !ok {
			fatal(fmt.Errorf("unknown workflow %q", *wfName))
		}
		newRoot, err := capy.ImportTree(ctx, *repoDir, *baseTree, wf, *subTree)
		if err != nil {
			fatal(err)
		}
		fmt.Println(newRoot)

	case "format-msg":
		fs := flag.NewFlagSet("format-msg", flag.ExitOnError)
		cfgPath := fs.String("config", "capy.star", "path to capy.star")
		wfName := fs.String("workflow", "", "workflow name")
		sourceRev := fs.String("source-rev", "", "source commit SHA")
		msg := fs.String("msg", "", "raw commit message")
		_ = fs.Parse(args)

		cfg, err := capy.LoadConfig(*cfgPath)
		if err != nil {
			fatal(err)
		}
		wf, ok := cfg.FindWorkflow(*wfName)
		if !ok {
			fatal(fmt.Errorf("unknown workflow %q", *wfName))
		}
		fmt.Print(capy.FormatCommitMessage(*msg, wf, *sourceRev))

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: capy <list|check-visibility|check-owners|export-tree|import-tree|format-msg> [flags]")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "capy: %v\n", err)
	os.Exit(1)
}

// teamFlags implements repeatable -team org/slug=login,login flags for offline runs and tests.
type teamFlags []string

func (t *teamFlags) String() string     { return strings.Join(*t, ";") }
func (t *teamFlags) Set(v string) error { *t = append(*t, v); return nil }

func (t teamFlags) resolver() capy.TeamResolver {
	members := map[string]map[string]bool{}
	for _, spec := range t {
		name, list, _ := strings.Cut(spec, "=")
		m := map[string]bool{}
		for _, l := range strings.Split(list, ",") {
			m[strings.ToLower(strings.TrimSpace(l))] = true
		}
		members[strings.ToLower(name)] = m
	}
	return func(team, login string) (bool, error) {
		return members[strings.ToLower(team)][login], nil
	}
}
