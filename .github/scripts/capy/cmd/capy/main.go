package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

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
		baseTreeOID := fs.String("base-tree", "", "base tree-ish")
		treeOID := fs.String("tree", "HEAD", "new tree-ish")
		author := fs.String("author", "", "commit author name/email")
		msg := fs.String("msg", "", "commit message (may contain Reviewed-by/Approved-by trailers)")
		_ = fs.Parse(args)

		actors := capy.ExtractCommitActors(*author, *msg)
		violations, err := capy.CheckOwners(ctx, *repoDir, *baseTreeOID, *treeOID, actors)
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
		baseTree := fs.String("base-tree", "", "base monorepo root tree-ish")
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
		newRoot, err := capy.SpliceSubtree(ctx, *repoDir, *baseTree, wf.OriginPrefix, *subTree)
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
