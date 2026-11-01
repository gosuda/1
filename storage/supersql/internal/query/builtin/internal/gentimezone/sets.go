package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type abbreviation struct {
	name     string
	offset   int32
	zone     int32
	dst      bool
	zoneName string
}

func (g *generator) loadSet(root, name string, depth int, entries map[string]abbreviation) error {
	if depth > 3 || name == "" {
		return fmt.Errorf("%w: abbreviation include depth/name", errData)
	}
	for _, c := range name {
		lower := c >= 'a' && c <= 'z'
		upper := c >= 'A' && c <= 'Z'
		if !lower && !upper {
			return fmt.Errorf("%w: include name %q", errData, name)
		}
	}
	raw, err := g.input(filepath.Join(root, name), "set/"+name)
	if err != nil {
		return err
	}
	override := false
	for lineNumber, line := range strings.Split(string(raw), "\n") {
		if len(line) >= 1022 {
			return fmt.Errorf("%w: %s:%d line too long", errData, name, lineNumber+1)
		}
		line = strings.TrimLeft(line, " \t\r\v\f")
		if line == "" || line[0] == '#' {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "@INCLUDE") {
			fields := strings.Fields(line[len("@INCLUDE"):])
			if len(fields) == 0 {
				return fmt.Errorf("%w: missing include name", errData)
			}
			if err := g.loadSet(root, fields[0], depth+1, entries); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(upper, "@OVERRIDE") {
			override = true
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) > 10 {
			return fmt.Errorf("%w: %s:%d abbreviation syntax", errData, name, lineNumber+1)
		}
		a := abbreviation{name: strings.ToLower(fields[0]), zone: -1}
		value := fields[1]
		next := 2
		digit := value[0] >= '0' && value[0] <= '9'
		if digit || value[0] == '+' || value[0] == '-' {
			n, err := strconv.ParseInt(value, 10, 32)
			if err != nil || n < -50400 || n > 50400 {
				return fmt.Errorf("%w: %s:%d offset", errData, name, lineNumber+1)
			}
			a.offset = int32(n)
			if next < len(fields) && strings.EqualFold(fields[next], "D") {
				a.dst = true
				next++
			}
		} else {
			index, ok := g.zoneIndex[strings.ToLower(value)]
			if !ok {
				return fmt.Errorf("%w: %s:%d unresolved zone %q", errData, name, lineNumber+1, value)
			}
			a.zone = int32(index)
			a.zoneName = value
		}
		if next < len(fields) && !strings.HasPrefix(fields[next], "#") {
			return fmt.Errorf("%w: %s:%d trailing fields", errData, name, lineNumber+1)
		}
		if old, ok := entries[a.name]; ok && !override {
			if old.offset == a.offset && old.dst == a.dst && old.zoneName == a.zoneName {
				continue
			}
			return fmt.Errorf("%w: %s:%d conflicting abbreviation %q", errData, name, lineNumber+1, a.name)
		}
		entries[a.name] = a
	}
	return nil
}

func (g *generator) loadDefault(root string) error {
	entries := make(map[string]abbreviation)
	if err := g.loadSet(root, "Default", 0, entries); err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g.abbreviations = append(g.abbreviations, entries[name])
	}
	return nil
}
