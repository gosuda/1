package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gosuda.org/grt/internal/formal"
)

func main() {
	outEngine := flag.String("engine-out", filepath.Join("internal", "engine", "z_formal_gen.go"), "output path for generated Go engine primitives")
	outTLA := flag.String("tla-dir", filepath.Join("spec", "formal"), "output directory for generated TLA+ specification")
	flag.Parse()

	if err := run(*outEngine, *outTLA); err != nil {
		fmt.Fprintf(os.Stderr, "grtgen: %v\n", err)
		os.Exit(1)
	}
}

func run(outEngine, outTLA string) error {
	goSrc, err := formal.GenerateEngineGo()
	if err != nil {
		return err
	}
	if err := os.WriteFile(outEngine, goSrc, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outEngine, err)
	}

	tlaSpec, tlaCfg, err := formal.GenerateTLA()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outTLA, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outTLA, err)
	}
	tlaPath := filepath.Join(outTLA, "GRT11.tla")
	if err := os.WriteFile(tlaPath, []byte(tlaSpec), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tlaPath, err)
	}
	cfgPath := filepath.Join(outTLA, "GRT11.cfg")
	if err := os.WriteFile(cfgPath, []byte(tlaCfg), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	return nil
}
