package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errData = errors.New("invalid PostgreSQL timezone data")

const maxInputBytes = 32 << 20
const maxFileBytes = 2 << 20
const maxFiles = 4096

type generator struct {
	digest               hash.Hash
	bytesRead, filesRead int
	zones                []zone
	zoneIndex            map[string]int
	data                 []byte
	abbreviations        []abbreviation
	version              string
}

func main() {
	zones := flag.String("zones", "", "compiled PostgreSQL TZif directory")
	sets := flag.String("sets", "", "PostgreSQL src/timezone/tznames directory")
	tzdata := flag.String("tzdata", "", "IANA tzdata.zi source")
	out := flag.String("out", "", "generated Go source")
	flag.Parse()
	if err := generate(*zones, *sets, *tzdata, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (g *generator) input(path, name string) ([]byte, error) {
	if g.filesRead >= maxFiles {
		return nil, fmt.Errorf("%w: input file budget", errData)
	}
	g.filesRead++
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	g.bytesRead += len(data)
	if len(data) > maxFileBytes || g.bytesRead > maxInputBytes {
		return nil, fmt.Errorf("%w: input byte budget", errData)
	}
	var frame [8]byte
	binary.BigEndian.PutUint64(frame[:], uint64(len(name)))
	g.digest.Write(frame[:])
	g.digest.Write([]byte(name))
	binary.BigEndian.PutUint64(frame[:], uint64(len(data)))
	g.digest.Write(frame[:])
	g.digest.Write(data)
	return data, nil
}

func generate(zones, sets, tzdata, out string) error {
	if zones == "" || sets == "" || tzdata == "" || out == "" {
		return fmt.Errorf("%w: all four paths are required", errData)
	}
	g := generator{digest: sha256.New(), zoneIndex: make(map[string]int)}
	source, err := g.input(tzdata, "source/tzdata.zi")
	if err != nil {
		return err
	}
	first, _, _ := strings.Cut(string(source), "\n")
	if !strings.HasPrefix(first, "# version ") {
		return fmt.Errorf("%w: missing tzdata version", errData)
	}
	g.version = strings.TrimPrefix(first, "# version ")
	if g.version != "2026b" {
		return fmt.Errorf("%w: tzdata version %q, expected 2026b", errData, g.version)
	}
	if err := g.loadZones(zones); err != nil {
		return err
	}
	if err := g.checkSourceInventory(string(source)); err != nil {
		return err
	}
	if err := g.loadDefault(sets); err != nil {
		return err
	}
	generated, err := g.render()
	if err != nil {
		return err
	}
	return os.WriteFile(out, generated, 0644)
}

func (g *generator) loadZones(root string) error {
	var paths []string
	entries := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > maxFiles {
			return fmt.Errorf("%w: zone directory budget", errData)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("%w: nonregular zone %s", errData, path)
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		if len(name) > 255 {
			return fmt.Errorf("%w: zone name length", errData)
		}
		for _, c := range name {
			if c < 33 || c > 126 {
				return fmt.Errorf("%w: non-ASCII zone name", errData)
			}
		}
		paths = append(paths, name)
		return nil
	})
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("%w: empty zone inventory", errData)
	}
	sort.Slice(paths, func(i, j int) bool { return strings.ToLower(paths[i]) < strings.ToLower(paths[j]) })
	type shared struct {
		raw []byte
		z   zone
	}
	dedup := make(map[[32]byte]shared)
	for _, name := range paths {
		lower := strings.ToLower(name)
		if _, exists := g.zoneIndex[lower]; exists {
			return fmt.Errorf("%w: case-insensitive duplicate zone %q", errData, name)
		}
		raw, err := g.input(filepath.Join(root, filepath.FromSlash(name)), "zone/"+name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		cached, exists := dedup[sum]
		var z zone
		if exists {
			if !bytes.Equal(cached.raw, raw) {
				return fmt.Errorf("%w: TZif digest collision", errData)
			}
			z = cached.z
		} else {
			z, err = parseTZif(raw)
			if err != nil {
				return fmt.Errorf("zone %s: %w", name, err)
			}
			// Retain only the selected block; all generated offsets are absolute.
			start, end := z.times, z.charsEnd
			base := uint32(len(g.data))
			g.data = append(g.data, raw[start:end]...)
			z.times = base
			z.indices = base + z.indices - start
			z.types = base + z.types - start
			z.chars = base + z.chars - start
			z.charsEnd = base + z.charsEnd - start
			dedup[sum] = shared{raw: raw, z: z}
		}
		z.name = name
		g.zoneIndex[lower] = len(g.zones)
		g.zones = append(g.zones, z)
	}
	return nil
}

func (g *generator) render() ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("// Code generated by internal/gentimezone from PostgreSQL REL_18_4 and IANA tzdata; DO NOT EDIT.\n")
	fmt.Fprintf(&out, "// Source: https://www.iana.org/time-zones (tzdata %s, vendored by PostgreSQL REL_18_4 under src/timezone/data)\n", g.version)
	out.WriteString("// License: public-domain (see THIRD_PARTY_NOTICES.md)\n")
	out.WriteString("// Sources: src/timezone/data/tzdata.zi, PostgreSQL-compiled TZif, src/timezone/tznames/Default and its includes.\n")
	out.WriteString("// Digest frames: uint64 big-endian name length, name, uint64 big-endian data length, data; source then sorted zones then set/include traversal.\n\npackage builtin\n\n")
	out.WriteString("import \"github.com/gosuda/supersql-alpha/internal/query/builtin/internal/tzrule\"\n\n")
	fmt.Fprintf(&out, "const TimeZoneDataVersion = %q\nconst TimeZoneDataSHA256 = %q\n", g.version, fmt.Sprintf("%x", g.digest.Sum(nil)))
	out.WriteString("const timezoneZoneBytes = \"\" +\n")
	for start := 0; start < len(g.data); start += 128 {
		fmt.Fprintf(&out, "%q", string(g.data[start:min(start+128, len(g.data))]))
		if start+128 < len(g.data) {
			out.WriteString(" +")
		}
		out.WriteByte('\n')
	}
	out.WriteString("\nvar timezoneZones = [...]timezoneZoneRecord{\n")
	for _, z := range g.zones {
		fmt.Fprintf(&out, "{name:%q,times:%d,indices:%d,types:%d,chars:%d,charsEnd:%d,count:%d,typeCount:%d,defaultType:%d,timeSize:%d,future:%#v,hasFuture:%t},\n", z.name, z.times, z.indices, z.types, z.chars, z.charsEnd, z.count, z.typeCount, z.defaultType, z.timeSize, z.future, z.hasFuture)
	}
	out.WriteString("}\n\nvar timezoneDefaultAbbreviations = [...]timezoneAbbreviationRecord{\n")
	for _, a := range g.abbreviations {
		fmt.Fprintf(&out, "{name:%q,offset:%d,zone:%d,dst:%t},\n", a.name, a.offset, a.zone, a.dst)
	}
	out.WriteString("}\n")
	return format.Source(out.Bytes())
}

func (g *generator) checkSourceInventory(source string) error {
	names := make(map[string]bool)
	for lineNumber, line := range strings.Split(source, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Z" && fields[0] != "L" {
			continue
		}
		count := 2
		if fields[0] == "L" {
			count = 3
		}
		if len(fields) < count {
			return fmt.Errorf("%w: tzdata.zi:%d incomplete zone/link", errData, lineNumber+1)
		}
		name := fields[count-1]
		if names[name] {
			return fmt.Errorf("%w: duplicate source zone/link %q", errData, name)
		}
		names[name] = true
		for _, ref := range fields[1:count] {
			index, ok := g.zoneIndex[strings.ToLower(ref)]
			if !ok || g.zones[index].name != ref {
				return fmt.Errorf("%w: missing compiled source zone %q", errData, ref)
			}
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("%w: empty source zone inventory", errData)
	}
	for _, z := range g.zones {
		if !names[z.name] {
			return fmt.Errorf("%w: compiled zone %q is absent from the pinned source", errData, z.name)
		}
	}
	return nil
}
