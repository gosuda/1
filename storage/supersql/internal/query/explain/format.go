package explain

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type Format uint8

const (
	FormatJSON Format = iota
	FormatText
	FormatXML
	FormatYAML
)

func Write(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, format Format, options Options) error {
	switch format {
	case FormatJSON:
		return WriteJSON(ctx, output, graph, options)
	case FormatText:
		return writeText(ctx, output, graph, options)
	case FormatXML:
		return writeXML(ctx, output, graph, options)
	case FormatYAML:
		return writeYAML(ctx, output, graph, options)
	default:
		return ErrInvalid
	}
}

func documentWriter(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, options Options) (*jsonWriter, error) {
	if ctx == nil || output == nil || graph == nil || graph.Plan() == nil {
		return nil, ErrInvalid
	}
	limits := options.Limits
	if limits.MaxOutputBytes == 0 || limits.MaxNodeBytes == 0 || limits.MaxValueDepth == 0 || limits.MaxValueDepth > 64 {
		return nil, ErrInvalid
	}
	if options.Planning != nil && options.Planning.Duration < 0 {
		return nil, ErrInvalid
	}
	if options.Analysis != nil {
		if len(options.Analysis.Nodes) != int(graph.Plan().NodeCount()) {
			return nil, ErrInvalid
		}
		for index, observation := range options.Analysis.Nodes {
			if observation.Node != physical.NodeID(index+1) {
				return nil, ErrInvalid
			}
		}
	}
	return &jsonWriter{ctx: ctx, output: output, remaining: limits.MaxOutputBytes, depth: limits.MaxValueDepth}, nil
}

func writeText(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, options Options) error {
	w, err := documentWriter(ctx, output, graph, options)
	if err != nil {
		return err
	}
	for index := range graph.Plan().NodeCount() {
		id := physical.NodeID(index + 1)
		node, cloneErr := graph.Plan().CloneNode(ctx, id, options.Limits.MaxNodeBytes)
		if cloneErr != nil {
			return cloneErr
		}
		w.raw("Node ")
		w.raw(strconv.FormatUint(uint64(id), 10))
		w.raw(" ")
		w.raw(algorithmName(node.Algorithm))
		if options.Costs {
			w.raw(" cost=")
			w.fixed(node.Cost)
			w.raw(" rows=")
			w.fixed(node.Estimate)
		}
		if options.Verbose {
			w.raw(" logical=")
			w.fixed(node.Logical)
			w.raw(" output=")
			writeOutput(w, node)
			w.raw(" required=")
			writeRequirements(w, node)
			w.raw(" delivered=")
			w.properties(node.Delivered)
		}
		if options.Analysis != nil {
			writeTextObservation(w, options.Analysis.Nodes[index])
		}
		w.raw("\n")
	}
	if options.Planning != nil && options.Summary {
		w.raw("Planning Time: ")
		w.raw(strconv.FormatInt(options.Planning.Duration.Nanoseconds(), 10))
		w.raw(" ns\n")
	}
	if options.Analysis != nil && options.Summary {
		w.raw("Execution Time: ")
		w.raw(strconv.FormatUint(options.Analysis.ExecutionNanos, 10))
		w.raw(" ns\n")
	}
	if options.Analysis != nil {
		if options.AffectedRows != nil {
			w.raw("Affected Rows: ")
			w.raw(strconv.FormatUint(*options.AffectedRows, 10))
			w.raw("\n")
		}
		if options.Analysis.LockWaitNanos.Known {
			w.raw("Lock Wait: ")
			w.raw(strconv.FormatUint(options.Analysis.LockWaitNanos.Value, 10))
			w.raw(" ns\n")
		}
		if options.Analysis.ResourcesKnown {
			usage := options.Analysis.Resources
			w.raw("Memory: work peak ")
			w.raw(strconv.FormatInt(usage.Work.Peak, 10))
			w.raw(" bytes, output peak ")
			w.raw(strconv.FormatInt(usage.Output.Peak, 10))
			w.raw(" bytes\nSpill: peak ")
			w.raw(strconv.FormatInt(usage.Spill.Peak, 10))
			w.raw(" bytes, files ")
			w.raw(strconv.Itoa(usage.PeakFiles))
			w.raw(", readers ")
			w.raw(strconv.Itoa(usage.PeakReaders))
			w.raw("\n")
		}
	}
	if options.Serialization != "" {
		w.raw("Serialization: ")
		w.raw(options.Serialization)
		w.raw("\n")
	}
	if options.SerializedBytes != nil {
		w.raw("Serialized Bytes: ")
		w.raw(strconv.FormatUint(*options.SerializedBytes, 10))
		w.raw("\n")
	}
	for _, setting := range options.SettingValues {
		w.raw("Setting: ")
		w.raw(setting.Name)
		w.raw(" = ")
		w.raw(setting.Value)
		w.raw("\n")
	}
	return w.err
}

func writeTextObservation(w *jsonWriter, observation exec.NodeObservation) {
	w.raw(" actual rows=")
	w.raw(strconv.FormatUint(observation.Rows, 10))
	w.raw(" loops=")
	w.raw(strconv.FormatUint(observation.Loops, 10))
	if observation.Nanoseconds.Known {
		w.raw(" time=")
		w.raw(strconv.FormatUint(observation.Nanoseconds.Value, 10))
		w.raw("ns")
	}
	metrics := []struct {
		name  string
		value exec.MeasuredCount
	}{
		{"read_bytes", observation.Storage.ReadBytes},
		{"decoded_bytes", observation.Storage.DecodedBytes},
		{"blocks_read", observation.Storage.BlocksRead},
		{"blocks_pruned", observation.Storage.BlocksPruned},
		{"ranges_pruned", observation.Storage.RangesPruned},
	}
	for _, metric := range metrics {
		if metric.value.Known {
			w.raw(" ")
			w.raw(metric.name)
			w.raw("=")
			w.raw(strconv.FormatUint(metric.value.Value, 10))
		}
	}
}

func writeOutput(w *jsonWriter, node physical.NodeSpec) {
	w.list(len(node.Output), func(index int) { w.fixed(node.Output[index]) })
}

func writeRequirements(w *jsonWriter, node physical.NodeSpec) {
	w.list(len(node.Required), func(index int) { w.properties(node.Required[index]) })
}

func writeXML(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, options Options) error {
	w, err := documentWriter(ctx, output, graph, options)
	if err != nil {
		return err
	}
	w.raw(`<?xml version="1.0" encoding="UTF-8"?>`)
	w.raw("\n<Explain xmlns=\"https://github.com/gosuda/supersql-alpha/explain/v1\"><Plan>")
	for index := range graph.Plan().NodeCount() {
		id := physical.NodeID(index + 1)
		node, cloneErr := graph.Plan().CloneNode(ctx, id, options.Limits.MaxNodeBytes)
		if cloneErr != nil {
			return cloneErr
		}
		w.raw("<Node id=\"")
		w.raw(strconv.FormatUint(uint64(id), 10))
		w.raw("\"><Algorithm>")
		xmlText(w, algorithmName(node.Algorithm))
		w.raw("</Algorithm>")
		if options.Costs {
			w.raw("<Cost>")
			w.fixed(node.Cost)
			w.raw("</Cost><Estimate>")
			w.fixed(node.Estimate)
			w.raw("</Estimate>")
		}
		if options.Verbose {
			w.raw("<Logical>")
			w.fixed(node.Logical)
			w.raw("</Logical><Output>")
			writeOutput(w, node)
			w.raw("</Output><Required>")
			writeRequirements(w, node)
			w.raw("</Required><Delivered>")
			w.properties(node.Delivered)
			w.raw("</Delivered>")
		}
		if options.Analysis != nil {
			observation := options.Analysis.Nodes[index]
			w.raw("<Actual rows=\"")
			w.raw(strconv.FormatUint(observation.Rows, 10))
			w.raw("\" loops=\"")
			w.raw(strconv.FormatUint(observation.Loops, 10))
			w.raw("\"")
			if observation.Nanoseconds.Known {
				w.raw(" nanoseconds=\"")
				w.raw(strconv.FormatUint(observation.Nanoseconds.Value, 10))
				w.raw("\"")
			}
			xmlMetric(w, "read-bytes", observation.Storage.ReadBytes)
			xmlMetric(w, "decoded-bytes", observation.Storage.DecodedBytes)
			xmlMetric(w, "blocks-read", observation.Storage.BlocksRead)
			xmlMetric(w, "blocks-pruned", observation.Storage.BlocksPruned)
			xmlMetric(w, "ranges-pruned", observation.Storage.RangesPruned)
			w.raw("/>")
		}
		w.raw("</Node>")
	}
	if options.Analysis != nil {
		w.raw("<Execution nanoseconds=\"")
		w.raw(strconv.FormatUint(options.Analysis.ExecutionNanos, 10))
		w.raw("\"")
		if options.AffectedRows != nil {
			w.raw(" affected-rows=\"")
			w.raw(strconv.FormatUint(*options.AffectedRows, 10))
			w.raw("\"")
		}
		if options.Serialization != "" {
			w.raw(" serialization=\"")
			xmlText(w, options.Serialization)
			w.raw("\"")
		}
		if options.Analysis.LockWaitNanos.Known {
			w.raw(" lock-wait-nanoseconds=\"")
			w.raw(strconv.FormatUint(options.Analysis.LockWaitNanos.Value, 10))
			w.raw("\"")
		}
		if options.SerializedBytes != nil {
			w.raw(" serialized-bytes=\"")
			w.raw(strconv.FormatUint(*options.SerializedBytes, 10))
			w.raw("\"")
		}
		w.raw(">")
		if options.Analysis.ResourcesKnown {
			usage := options.Analysis.Resources
			w.raw("<Resources work-peak-bytes=\"")
			w.raw(strconv.FormatInt(usage.Work.Peak, 10))
			w.raw("\" output-peak-bytes=\"")
			w.raw(strconv.FormatInt(usage.Output.Peak, 10))
			w.raw("\" spill-peak-bytes=\"")
			w.raw(strconv.FormatInt(usage.Spill.Peak, 10))
			w.raw("\" peak-files=\"")
			w.raw(strconv.Itoa(usage.PeakFiles))
			w.raw("\" peak-readers=\"")
			w.raw(strconv.Itoa(usage.PeakReaders))
			w.raw("\"/>")
		}
		w.raw("</Execution>")
	}
	w.raw("</Plan>")
	for _, setting := range options.SettingValues {
		w.raw("<Setting name=\"")
		xmlText(w, setting.Name)
		w.raw("\">")
		xmlText(w, setting.Value)
		w.raw("</Setting>")
	}
	w.raw("</Explain>\n")
	return w.err
}

func xmlText(w *jsonWriter, value string) {
	for len(value) != 0 && w.err == nil {
		index := strings.IndexAny(value, "&<>\"'")
		if index < 0 {
			w.raw(value)
			return
		}
		w.raw(value[:index])
		switch value[index] {
		case '&':
			w.raw("&amp;")
		case '<':
			w.raw("&lt;")
		case '>':
			w.raw("&gt;")
		case '"':
			w.raw("&quot;")
		case '\'':
			w.raw("&apos;")
		}
		value = value[index+1:]
	}
}

func xmlMetric(w *jsonWriter, name string, metric exec.MeasuredCount) {
	if !metric.Known {
		return
	}
	w.raw(" ")
	w.raw(name)
	w.raw("=\"")
	w.raw(strconv.FormatUint(metric.Value, 10))
	w.raw("\"")
}

func writeYAML(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, options Options) error {
	w, err := documentWriter(ctx, output, graph, options)
	if err != nil {
		return err
	}
	w.raw("---\nFormat: supersql.plan.v1\nRoot: ")
	w.raw(strconv.FormatUint(uint64(graph.Plan().Root()), 10))
	if options.Analysis != nil {
		w.raw("\nExecution Nanoseconds: ")
		w.raw(strconv.FormatUint(options.Analysis.ExecutionNanos, 10))
		if options.AffectedRows != nil {
			w.raw("\nAffected Rows: ")
			w.raw(strconv.FormatUint(*options.AffectedRows, 10))
		}
		if options.Analysis.LockWaitNanos.Known {
			w.raw("\nLock Wait Nanoseconds: ")
			w.raw(strconv.FormatUint(options.Analysis.LockWaitNanos.Value, 10))
		}
		if options.Analysis.ResourcesKnown {
			usage := options.Analysis.Resources
			w.raw("\nResources:\n  Work Peak Bytes: ")
			w.raw(strconv.FormatInt(usage.Work.Peak, 10))
			w.raw("\n  Output Peak Bytes: ")
			w.raw(strconv.FormatInt(usage.Output.Peak, 10))
			w.raw("\n  Spill Peak Bytes: ")
			w.raw(strconv.FormatInt(usage.Spill.Peak, 10))
			w.raw("\n  Peak Files: ")
			w.raw(strconv.Itoa(usage.PeakFiles))
			w.raw("\n  Peak Readers: ")
			w.raw(strconv.Itoa(usage.PeakReaders))
		}
		if options.SerializedBytes != nil {
			w.raw("\nSerialized Bytes: ")
			w.raw(strconv.FormatUint(*options.SerializedBytes, 10))
		}
	}
	if options.Serialization != "" {
		w.raw("\nSerialization: ")
		w.text(options.Serialization)
	}
	w.raw("\n")
	w.raw("\nNodes:\n")
	for index := range graph.Plan().NodeCount() {
		id := physical.NodeID(index + 1)
		node, cloneErr := graph.Plan().CloneNode(ctx, id, options.Limits.MaxNodeBytes)
		if cloneErr != nil {
			return cloneErr
		}
		w.raw("  - Node ID: ")
		w.raw(strconv.FormatUint(uint64(id), 10))
		w.raw("\n    Algorithm: ")
		w.text(algorithmName(node.Algorithm))
		if options.Costs {
			w.raw("\n    Cost: ")
			w.fixed(node.Cost)
			w.raw("\n    Estimate: ")
			w.fixed(node.Estimate)
		}
		if options.Verbose {
			w.raw("\n    Logical: ")
			w.fixed(node.Logical)
			w.raw("\n    Output: ")
			writeOutput(w, node)
			w.raw("\n    Required: ")
			writeRequirements(w, node)
			w.raw("\n    Delivered: ")
			w.properties(node.Delivered)
		}
		if options.Analysis != nil {
			observation := options.Analysis.Nodes[index]
			w.raw("\n    Actual Rows: ")
			w.raw(strconv.FormatUint(observation.Rows, 10))
			w.raw("\n    Actual Loops: ")
			w.raw(strconv.FormatUint(observation.Loops, 10))
			if observation.Nanoseconds.Known {
				w.raw("\n    Actual Nanoseconds: ")
				w.raw(strconv.FormatUint(observation.Nanoseconds.Value, 10))
			}
			yamlMetric(w, "Read Bytes", observation.Storage.ReadBytes)
			yamlMetric(w, "Decoded Bytes", observation.Storage.DecodedBytes)
			yamlMetric(w, "Blocks Read", observation.Storage.BlocksRead)
			yamlMetric(w, "Blocks Pruned", observation.Storage.BlocksPruned)
			yamlMetric(w, "Ranges Pruned", observation.Storage.RangesPruned)
		}
		w.raw("\n")
	}
	if len(options.SettingValues) != 0 {
		w.raw("Settings:\n")
		for _, setting := range options.SettingValues {
			w.raw("  ")
			w.text(setting.Name)
			w.raw(": ")
			w.text(setting.Value)
			w.raw("\n")
		}
	}
	return w.err
}

func yamlMetric(w *jsonWriter, name string, metric exec.MeasuredCount) {
	if !metric.Known {
		return
	}
	w.raw("\n    ")
	w.raw(name)
	w.raw(": ")
	w.raw(strconv.FormatUint(metric.Value, 10))
}
