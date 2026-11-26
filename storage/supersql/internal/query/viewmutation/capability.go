package viewmutation

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type Capability uint8

const (
	Delete Capability = 1 << 2
	Insert Capability = 1 << 3
	Update Capability = 1 << 4
)

type Catalog interface {
	Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error)
}

func RelationCapabilities(ctx context.Context, snapshot Catalog, descriptor *model.Descriptor, includeTriggers bool, columns []model.ColumnID) (Capability, error) {
	return relationCapabilities(ctx, snapshot, descriptor, includeTriggers, columns, make(map[model.ObjectID]bool), 0)
}

func relationCapabilities(ctx context.Context, snapshot Catalog, descriptor *model.Descriptor, includeTriggers bool, columns []model.ColumnID, visited map[model.ObjectID]bool, depth uint32) (Capability, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if snapshot == nil || descriptor == nil || descriptor.State() != model.StatePublic || depth >= 64 || visited[descriptor.ID()] {
		return 0, model.ErrInvalidDescriptor
	}
	if table, ok := descriptor.Table(); ok && descriptor.Kind() == model.ObjectTable {
		if columns != nil && len(columns) == 0 {
			return Delete, nil
		}
		result := Delete | Insert | Update
		for _, id := range columns {
			found := false
			for _, column := range table.Columns {
				if column.ID == id && column.State == model.StatePublic && !column.SystemIdentity {
					found = true
					break
				}
			}
			if !found {
				result &^= Insert | Update
			}
		}
		return result, nil
	}
	view, ok := descriptor.View()
	if !ok || descriptor.Kind() != model.ObjectView {
		return 0, nil
	}
	var result Capability
	if includeTriggers {
		result = triggerCapabilities(view.Triggers)
	}
	if view.Automatic == nil {
		return result, nil
	}
	mapped := make([]model.ColumnID, 0, len(view.Automatic.Columns))
	allMapped := true
	if columns == nil {
		for _, mapping := range view.Automatic.Columns {
			mapped = append(mapped, mapping.Base)
		}
	}
	if columns != nil {
		for _, column := range columns {
			found := false
			for _, mapping := range view.Automatic.Columns {
				if mapping.View == column {
					mapped = append(mapped, mapping.Base)
					found = true
					break
				}
			}
			allMapped = allMapped && found
		}
	}
	visited[descriptor.ID()] = true
	published, err := snapshot.Descriptor(ctx, view.Automatic.BaseRelation)
	if err != nil {
		delete(visited, descriptor.ID())
		return 0, err
	}
	base := published.Descriptor
	if base == nil || base.OID() != view.Automatic.BaseOID {
		delete(visited, descriptor.ID())
		return 0, model.ErrInvalidDescriptor
	}
	baseColumns := mapped
	baseCapabilities, err := relationCapabilities(ctx, snapshot, base, includeTriggers, baseColumns, visited, depth+1)
	delete(visited, descriptor.ID())
	if err != nil {
		return 0, err
	}
	if baseCapabilities&Delete != 0 {
		result |= Delete
	}
	if len(mapped) != 0 && (columns == nil || allMapped) {
		result |= baseCapabilities & (Insert | Update)
	}
	return result, nil
}

func triggerCapabilities(triggers []model.TriggerDescriptor) Capability {
	var result Capability
	for _, trigger := range triggers {
		if !trigger.EnableMode.Fires(false) || !trigger.ForEachRow || !strings.EqualFold(trigger.Timing, "instead") {
			continue
		}
		for _, event := range trigger.Events {
			switch strings.ToLower(event) {
			case "delete":
				result |= Delete
			case "insert":
				result |= Insert
			case "update":
				result |= Update
			}
		}
	}
	return result
}
