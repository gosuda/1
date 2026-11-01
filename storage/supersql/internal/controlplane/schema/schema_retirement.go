package schema

import (
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func descriptorGeneration(descriptor *model.Descriptor) storage.ObjectGeneration {
	return storage.ObjectGeneration{ObjectID: descriptor.ID(), Generation: descriptor.Physical().Generation}
}

func transitionRetirements(definition JobDefinition, state JobState) ([]storage.ObjectGeneration, error) {
	var descriptors []*model.Descriptor
	switch state {
	case JobPublic:
		switch definition.Operation {
		case SchemaDropIndex:
			descriptors = definition.SourceIndexes
		case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey:
			descriptors = append([]*model.Descriptor{definition.SourceTable}, definition.SourceIndexes...)
		case SchemaReindex:
			descriptors = append(descriptors, definition.SourceTable)
			for i, source := range definition.SourceIndexes {
				if i >= len(definition.TargetIndexes) || source.Physical() != definition.TargetIndexes[i].Physical() {
					descriptors = append(descriptors, source)
				}
			}
		}
	case JobReverting:
		switch definition.Operation {
		case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey:
			descriptors = append([]*model.Descriptor{definition.TargetTable}, definition.TargetIndexes...)
		case SchemaReindex:
			descriptors = append(descriptors, definition.TargetTable)
			for i, target := range definition.TargetIndexes {
				if i >= len(definition.SourceIndexes) || target.Physical() != definition.SourceIndexes[i].Physical() {
					descriptors = append(descriptors, target)
				}
			}
		}
	}
	retired := make([]storage.ObjectGeneration, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor == nil {
			return nil, errJobDefinition
		}
		retired = append(retired, descriptorGeneration(descriptor))
	}
	return normalizeRetirements(retired)
}
