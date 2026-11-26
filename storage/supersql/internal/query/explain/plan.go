// Package explain streams immutable plan metadata without opening an executor.
package explain

import (
	"context"
	"io"
	"time"

	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type Limits struct {
	MaxOutputBytes uint64
	MaxNodeBytes   uint64
	MaxValueDepth  uint32
}

// Planning contains measured planner metadata; nil omits it rather than claiming
// that an unmeasured planning operation took zero time.
type Planning struct {
	Duration        time.Duration
	BudgetExhausted bool
}

type Setting struct {
	Name  string
	Value string
}

type Options struct {
	Limits        Limits
	Planning      *Planning
	Analysis      *exec.Observation
	AffectedRows  *uint64
	SettingValues []Setting
	Verbose       bool
	Costs         bool
	Summary       bool
	Serialization string
	SerializedBytes *uint64
}

// WriteJSON borrows graph and output. A writer failure or budget error can leave
// a prefix in output; callers requiring atomic publication must stage that prefix.
// MaxNodeBytes bounds each owned node copy. Strings and datums stream in bounded
// chunks; no document-sized buffer, reader, worker or snapshot is created.
func WriteJSON(ctx context.Context, output io.Writer, graph *fragment.FragmentGraph, options Options) error {
	w, err := documentWriter(ctx, output, graph, options)
	if err != nil {
		return err
	}
	w.raw(`{"Format":"supersql.plan.v1","Root":`)
	w.fixed(graph.Plan().Root())
	if options.Planning != nil {
		w.raw(`,"Planning Nanoseconds":`)
		w.fixed(options.Planning.Duration.Nanoseconds())
		w.raw(`,"Optimizer Budget Exhausted":`)
		w.fixed(options.Planning.BudgetExhausted)
	}
	if options.Analysis != nil {
		w.raw(`,"Execution Nanoseconds":`)
		w.fixed(options.Analysis.ExecutionNanos)
		if options.AffectedRows != nil {
			w.raw(`,"Affected Rows":`)
			w.fixed(*options.AffectedRows)
		}
		if options.Analysis.LockWaitNanos.Known {
			w.raw(`,"Lock Wait Nanoseconds":`)
			w.fixed(options.Analysis.LockWaitNanos.Value)
		}
		if options.Analysis.ResourcesKnown {
			usage := options.Analysis.Resources
			w.raw(`,"Resources":{"Work Peak Bytes":`)
			w.fixed(usage.Work.Peak)
			w.raw(`,"Output Peak Bytes":`)
			w.fixed(usage.Output.Peak)
			w.raw(`,"Spill Peak Bytes":`)
			w.fixed(usage.Spill.Peak)
			w.raw(`,"Peak Files":`)
			w.fixed(usage.PeakFiles)
			w.raw(`,"Peak Readers":`)
			w.fixed(usage.PeakReaders)
			w.raw("}")
		}
	}
	if options.Serialization != "" {
		w.raw(`,"Serialization":`)
		w.text(options.Serialization)
	}
	if options.SerializedBytes != nil {
		w.raw(`,"Serialized Bytes":`)
		w.fixed(*options.SerializedBytes)
	}
	if len(options.SettingValues) != 0 {
		w.raw(`,"Settings":`)
		w.list(len(options.SettingValues), func(index int) {
			setting := options.SettingValues[index]
			w.raw(`{"Name":`)
			w.text(setting.Name)
			w.raw(`,"Value":`)
			w.text(setting.Value)
			w.raw("}")
		})
	}
	w.raw(`,"Nodes":`)
	w.list(int(graph.Plan().NodeCount()), func(index int) {
		id := physical.NodeID(index + 1)
		node, err := graph.Plan().CloneNode(ctx, id, options.Limits.MaxNodeBytes)
		if err != nil {
			w.err = err
			return
		}
		var observation *exec.NodeObservation
		if options.Analysis != nil {
			observation = &options.Analysis.Nodes[index]
		}
		w.node(graph, id, node, observation)
	})
	w.raw(`,"Pipelines":`)
	w.list(int(graph.PipelineCount()), func(index int) {
		id := fragment.PipelineID(index + 1)
		pipeline, _ := graph.Pipeline(id)
		w.raw(`{"ID":`)
		w.fixed(id)
		w.raw(`,"Iteration":`)
		w.fixed(pipeline.Iteration())
		w.raw(`,"Parallel Eligible":`)
		w.fixed(pipeline.ParallelEligible())
		w.raw(`,"Nodes":`)
		w.list(int(pipeline.NodeCount()), func(i int) { node, _ := pipeline.Node(uint32(i)); w.fixed(node) })
		w.raw("}")
	})
	w.raw(`,"Edges":`)
	w.list(int(graph.EdgeCount()), func(i int) { edge, _ := graph.Edge(fragment.EdgeID(i + 1)); w.fixed(edge) })
	w.raw(`,"Feedback":`)
	w.list(int(graph.FeedbackCount()), func(i int) { feedback, _ := graph.Feedback(uint32(i)); w.fixed(feedback) })
	w.raw(`,"Topological Order":`)
	w.list(int(graph.PipelineCount()), func(i int) { id, _ := graph.Topological(uint32(i)); w.fixed(id) })
	w.raw("}\n")
	return w.err
}

func (w *jsonWriter) node(graph *fragment.FragmentGraph, id physical.NodeID, node physical.NodeSpec, observation *exec.NodeObservation) {
	logical, _ := graph.Plan().Graph(node.Logical.Graph)
	relation, _ := logical.Rel(node.Logical.Rel)
	owner, _ := graph.Owner(id)
	role, _ := graph.Role(id)
	w.raw(`{"Node ID":`)
	w.fixed(id)
	w.raw(`,"Algorithm":`)
	w.text(algorithmName(node.Algorithm))
	w.raw(`,"Logical":`)
	w.fixed(node.Logical)
	w.raw(`,"Effects":`)
	w.fixed(relation.Effects)
	w.raw(`,"Pipeline":`)
	w.fixed(owner)
	w.raw(`,"Role":`)
	w.fixed(role)
	w.raw(`,"Break":`)
	w.fixed(node.Break)
	w.raw(`,"Cost":`)
	w.fixed(node.Cost)
	w.raw(`,"Estimate":`)
	w.fixed(node.Estimate)
	w.raw(`,"Memory":`)
	w.fixed(node.Memory)
	if observation != nil {
		w.raw(`,"Actual Rows":`)
		w.fixed(observation.Rows)
		w.raw(`,"Actual Batches":`)
		w.fixed(observation.Batches)
		w.raw(`,"Actual Loops":`)
		w.fixed(observation.Loops)
		if observation.Nanoseconds.Known {
			w.raw(`,"Actual Nanoseconds":`)
			w.fixed(observation.Nanoseconds.Value)
		}
		storage := observation.Storage
		if storage.ReadBytes.Known || storage.DecodedBytes.Known || storage.BlocksRead.Known || storage.BlocksPruned.Known || storage.RangesPruned.Known {
			w.raw(`,"Storage":{`)
			separator := false
			metric := func(name string, value exec.MeasuredCount) {
				if !value.Known {
					return
				}
				if separator {
					w.raw(",")
				}
				w.text(name)
				w.raw(":")
				w.fixed(value.Value)
				separator = true
			}
			metric("Read Bytes", storage.ReadBytes)
			metric("Decoded Bytes", storage.DecodedBytes)
			metric("Blocks Read", storage.BlocksRead)
			metric("Blocks Pruned", storage.BlocksPruned)
			metric("Ranges Pruned", storage.RangesPruned)
			w.raw("}")
		}
	}
	w.raw(`,"Children":`)
	w.list(len(node.Children), func(i int) { w.fixed(node.Children[i]) })
	w.raw(`,"Output":`)
	w.list(len(node.Output), func(i int) {
		slot := node.Output[i]
		g, _ := graph.Plan().Graph(slot.Graph)
		typ, _ := g.Type(slot.Layout)
		w.raw(`{"Slot":`)
		w.fixed(slot)
		w.raw(`,"Type":`)
		w.fixed(typ)
		w.raw("}")
	})
	w.raw(`,"Required":`)
	w.list(len(node.Required), func(i int) { w.properties(node.Required[i]) })
	w.raw(`,"Delivered":`)
	w.properties(node.Delivered)
	w.raw(`,"Expressions":`)
	w.list(len(node.Expressions), func(i int) { w.fixed(node.Expressions[i]) })
	w.raw(`,"Scalar Subplans":`)
	w.list(len(node.ScalarSubplans), func(i int) { w.fixed(node.ScalarSubplans[i]) })
	w.raw(`,"Statement Dependencies":`)
	w.list(len(node.StatementDependencies), func(i int) { w.fixed(node.StatementDependencies[i]) })
	w.raw(`,"Limit":`)
	w.fixed(node.Limit)
	w.raw(`,"Exchange":`)
	w.fixed(node.Exchange)
	w.raw(`,"Worktable":`)
	w.fixed(node.Worktable)
	if relation.Object != 0 {
		w.raw(`,"Object":`)
		w.fixed(relation.Object)
		w.raw(`,"Generation":`)
		w.fixed(relation.Generation)
		w.raw(`,"Descriptor Version":`)
		w.fixed(relation.DescriptorVersion)
	}
	if node.Scan.Request.Target.ObjectID != 0 {
		w.raw(`,"Scan":`)
		w.scan(node.Scan)
	}
	w.raw("}")
}

func (w *jsonWriter) properties(properties physical.Properties) {
	w.raw(`{"Ordering":`)
	w.list(len(properties.Ordering), func(i int) { w.fixed(properties.Ordering[i]) })
	w.raw(`,"Distribution":{"Kind":`)
	w.text(distributionName(properties.Distribution.Kind))
	w.raw(`,"Partitions":`)
	w.fixed(properties.Distribution.Partitions)
	w.raw(`,"Function":`)
	w.fixed(properties.Distribution.Function)
	w.raw(`,"Epoch":`)
	w.fixed(properties.Distribution.Epoch)
	w.raw(`,"Keys":`)
	w.list(len(properties.Distribution.Keys), func(i int) { w.fixed(properties.Distribution.Keys[i]) })
	w.raw(`},"Rewindability":`)
	w.fixed(properties.Rewindability)
	w.raw(`,"Parameters":`)
	w.list(len(properties.Parameters), func(i int) { w.fixed(properties.Parameters[i]) })
	w.raw("}")
}

func distributionName(kind physical.DistributionKind) string {
	switch kind {
	case physical.SingleNode:
		return "SingleNode"
	case physical.HashPartitioned:
		return "HashPartitioned"
	case physical.RangePartitioned:
		return "RangePartitioned"
	case physical.Replicated:
		return "Replicated"
	default:
		return "Unknown"
	}
}

func algorithmName(algorithm physical.Algorithm) string {
	switch algorithm {
	case physical.TableScan:
		return "TableScan"
	case physical.PrimaryKeyScan:
		return "PrimaryKeyScan"
	case physical.SecondaryScan:
		return "SecondaryScan"
	case physical.Values:
		return "Values"
	case physical.GenerateSeries:
		return "GenerateSeries"
	case physical.GenerateSubscripts:
		return "GenerateSubscripts"
	case physical.Unnest:
		return "Unnest"
	case physical.RowsFrom:
		return "RowsFrom"
	case physical.ProjectSet:
		return "ProjectSet"
	case physical.Filter:
		return "Filter"
	case physical.Project:
		return "Project"
	case physical.LookupJoin:
		return "LookupJoin"
	case physical.NestedLoopJoin:
		return "NestedLoopJoin"
	case physical.Apply:
		return "Apply"
	case physical.HashJoin:
		return "HashJoin"
	case physical.MergeJoin:
		return "MergeJoin"
	case physical.HashAggregate:
		return "HashAggregate"
	case physical.StreamAggregate:
		return "StreamAggregate"
	case physical.Sort:
		return "Sort"
	case physical.TopK:
		return "TopK"
	case physical.Limit:
		return "Limit"
	case physical.HashDistinct:
		return "HashDistinct"
	case physical.StreamDistinct:
		return "StreamDistinct"
	case physical.Window:
		return "Window"
	case physical.CTEMaterialize:
		return "CTEMaterialize"
	case physical.CTEScan:
		return "CTEScan"
	case physical.RecursiveUnion:
		return "RecursiveUnion"
	case physical.WorktableScan:
		return "WorktableScan"
	case physical.LockRows:
		return "LockRows"
	case physical.Modify:
		return "Modify"
	case physical.UnionAll:
		return "UnionAll"
	case physical.Materialize:
		return "Materialize"
	case physical.LocalGather:
		return "LocalGather"
	case physical.LocalOrderedMerge:
		return "LocalOrderedMerge"
	case physical.LocalHashRepartition:
		return "LocalHashRepartition"
	case physical.HashSetOp:
		return "HashSetOp"
	default:
		return "Unknown"
	}
}
