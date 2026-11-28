package model

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

func ValidateNamespaceName(n NamespaceName) error {
	if !utf8.ValidString(n.Name) || !utf8.ValidString(n.Signature) {
		return ErrInvalidDescriptor
	}
	if n.Kind < ObjectSchema || n.Kind > ObjectOperator || n.Name == "" || strings.IndexByte(n.Name, 0) >= 0 || strings.IndexByte(n.Signature, 0) >= 0 {
		return ErrInvalidDescriptor
	}
	if !n.Kind.SignatureBearing() && n.Signature != "" {
		return ErrInvalidDescriptor
	}
	if n.Kind != ObjectSchema && n.Parent == 0 {
		return ErrInvalidDescriptor
	}
	return nil
}

func validDescriptorText(value string) bool {
	return utf8.ValidString(value) && strings.IndexByte(value, 0) < 0
}
func validateDependencies(in []Dependency) error {
	for i, d := range in {
		if d.Object == 0 || d.Version == 0 {
			return ErrInvalidDescriptor
		}
		if i == 0 || in[i-1].Object != d.Object {
			continue
		}
		consistentVersion := in[i-1].Version == d.Version
		increasingColumn := in[i-1].Column < d.Column
		if !consistentVersion || !increasingColumn {
			return ErrInvalidDescriptor
		}
	}
	return nil
}
func uniqueIDs[T ~uint32 | ~uint64](ids []T) bool {
	seen := make(map[T]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}
func validateColumns(columns []ColumnDescriptor) error {
	ids := make(map[ColumnID]struct{}, len(columns))
	names := make(map[string]struct{}, len(columns))
	for _, c := range columns {
		if c.ID == 0 || c.Name == "" || c.State > StateDropDeleteOnly || !validDescriptorText(c.Description) {
			return ErrInvalidDescriptor
		}
		if c.StatisticsTarget < 0 || c.StatisticsTarget > 10000 || !c.StatisticsTargetSet && c.StatisticsTarget != 0 || c.SystemIdentity && c.StatisticsTargetSet {
			return ErrInvalidDescriptor
		}
		if _, ok := ids[c.ID]; ok {
			return ErrInvalidDescriptor
		}
		ids[c.ID] = struct{}{}
		if _, ok := names[c.Name]; ok {
			return ErrInvalidDescriptor
		}
		names[c.Name] = struct{}{}
		if err := c.Layout.Validate(); err != nil {
			return err
		}
		inherited := make(map[ColumnInheritance]struct{}, len(c.InheritedFrom))
		for _, source := range c.InheritedFrom {
			if source.Table == 0 || source.Column == 0 || c.SystemIdentity {
				return ErrInvalidDescriptor
			}
			if _, duplicate := inherited[source]; duplicate {
				return ErrInvalidDescriptor
			}
			inherited[source] = struct{}{}
		}
		hasDefault := c.Default != nil || c.ConstantDefault != nil
		if c.SystemIdentity {
			validType := c.Layout.Type.ID == 17 && c.Layout.Type.Storage == TypeBytes && len(c.Layout.Children) == 0
			hasExpression := hasDefault || c.Generated != nil || c.IdentitySequence != 0
			if !validType || hasExpression || c.Nullable {
				return ErrInvalidDescriptor
			}
		}
		if c.IdentitySequence != 0 {
			integer := c.Layout.Type.ID == 20 || c.Layout.Type.ID == 21 || c.Layout.Type.ID == 23
			invalidType := !integer || c.Layout.Type.Storage != TypeInt64
			if invalidType || c.Nullable || hasDefault {
				return ErrInvalidDescriptor
			}
		}
		hasAttributeDefault := c.Default != nil || c.Generated != nil
		conflictingGeneration := c.Generated != nil && (hasDefault || c.IdentitySequence != 0)
		if conflictingGeneration || (c.DefaultOID != 0) != hasAttributeDefault {
			return ErrInvalidDescriptor
		}
		missingIdentity := c.IdentityAlways && c.IdentitySequence == 0
		missingGeneration := c.GeneratedStored && c.Generated == nil
		if missingIdentity || missingGeneration {
			return ErrInvalidDescriptor
		}
		if c.ConstantDefault != nil && c.ConstantDefault.Type != c.Layout.Type.Storage {
			return ErrInvalidDescriptor
		}
	}
	return nil
}
func validateKeys(keys []KeyElement, columns []ColumnDescriptor) error {
	var seen map[ColumnID]struct{}
	if columns != nil {
		seen = make(map[ColumnID]struct{}, len(keys))
	}
	for _, key := range keys {
		if (key.Column == 0) == (key.Expression == nil) || key.Type.ID == 0 || !key.Type.Storage.Valid() || key.Collation != key.Type.Collation {
			return ErrInvalidDescriptor
		}
		if key.StatisticsTarget < 0 || key.StatisticsTarget > 10000 || !key.StatisticsTargetSet && key.StatisticsTarget != 0 || key.Expression == nil && key.StatisticsTargetSet {
			return ErrInvalidDescriptor
		}
		layout := TypeLayout{Type: key.Type, Children: key.Children}
		if err := layout.Validate(); err != nil {
			return ErrInvalidDescriptor
		}
		if key.Column == 0 {
			continue
		}
		if columns != nil {
			if _, ok := seen[key.Column]; ok {
				return ErrInvalidDescriptor
			}
			seen[key.Column] = struct{}{}
			found := false
			for _, c := range columns {
				if c.ID == key.Column {
					found = TypeLayoutsEqual(c.Layout, layout)
					break
				}
			}
			if !found {
				return ErrInvalidDescriptor
			}
		}
	}
	return nil
}
func validateConstraints(in []ConstraintDescriptor, columns []ColumnDescriptor) error {
	ids := make(map[ObjectID]struct{}, len(in))
	names := make(map[string]struct{}, len(in))
	for _, c := range in {
		validIdentity := c.ID != 0 && c.Name != "" && validDescriptorText(c.Description)
		validKind := c.Kind >= ConstraintCheck && c.Kind <= ConstraintTrigger
		validColumns := uniqueIDs(c.Columns) && uniqueIDs(c.ReferencedColumns) &&
			uniqueIDs(c.OnDeleteColumns) && uniqueIDs(c.OnUpdateColumns)
		if !validIdentity || !validKind || !validColumns || c.InitiallyDeferred && !c.Deferrable {
			return ErrInvalidDescriptor
		}
		if _, ok := ids[c.ID]; ok {
			return ErrInvalidDescriptor
		}
		ids[c.ID] = struct{}{}
		if _, ok := names[c.Name]; ok {
			return ErrInvalidDescriptor
		}
		names[c.Name] = struct{}{}
		if (c.Kind == ConstraintCheck || c.Kind == ConstraintNotNull) && (c.Deferrable || c.InitiallyDeferred) {
			return ErrInvalidDescriptor
		}
		if c.Mutation && (c.Kind != ConstraintPrimaryKey && c.Kind != ConstraintUnique && c.Kind != ConstraintExclude || c.Validated || c.Deferrable || c.InitiallyDeferred) {
			return ErrInvalidDescriptor
		}
		if c.NoInherit && c.Kind != ConstraintCheck && c.Kind != ConstraintNotNull {
			return ErrInvalidDescriptor
		}
		hasParent := c.ParentTable != 0 || c.ParentConstraint != 0
		if hasParent && (c.ParentTable == 0 || c.ParentConstraint == 0 || c.InheritanceCount == 0 || c.NoInherit) {
			return ErrInvalidDescriptor
		}
		if !hasParent && c.InheritanceCount != 0 {
			return ErrInvalidDescriptor
		}
		foreignFields := c.ReferencedObject != 0 || len(c.ReferencedColumns) != 0 ||
			c.ReferencedConstraint != 0 || c.ReferencedIndex != 0 ||
			len(c.ParentForeignEqualityOperators) != 0 || len(c.ParentEqualityOperators) != 0 ||
			len(c.ForeignEqualityOperators) != 0 ||
			c.OnDelete != "" || len(c.OnDeleteColumns) != 0 ||
			c.OnUpdate != "" || len(c.OnUpdateColumns) != 0 || c.Match != ""
		if c.Kind != ConstraintForeignKey && foreignFields {
			return ErrInvalidDescriptor
		}
		if c.Kind != ConstraintExclude && len(c.ExcludeOperators) != 0 {
			return ErrInvalidDescriptor
		}
		switch c.Kind {
		case ConstraintCheck:
			if c.Expression == nil || c.Index != 0 || !c.Enforced && c.Validated {
				return ErrInvalidDescriptor
			}
		case ConstraintNotNull:
			domainConstraint := len(columns) == 0 && len(c.Columns) == 0
			tableConstraint := len(c.Columns) == 1
			// NOT NULL is never unenforced: the pinned grammar rejects NOT
			// ENFORCED for it outright (NOT NULL constraints cannot be marked NOT
			// ENFORCED, gram.y processCASbits with a NULL is_enforced output), so
			// Enforced is a constant. Validated may be false, which is the
			// reachable "NOT NULL ... NOT VALID" state.
			if !domainConstraint && !tableConstraint || c.Expression != nil || c.Index != 0 || !c.Enforced {
				return ErrInvalidDescriptor
			}
		case ConstraintForeignKey:
			matchingArity := len(c.Columns) == len(c.ReferencedColumns) &&
				len(c.Columns) == len(c.ParentForeignEqualityOperators) &&
				len(c.Columns) == len(c.ParentEqualityOperators) &&
				len(c.Columns) == len(c.ForeignEqualityOperators)
			if c.ReferencedObject == 0 || c.ReferencedConstraint == 0 || c.ReferencedIndex == 0 ||
				len(c.Columns) == 0 || !matchingArity || c.Expression != nil || c.Index != 0 ||
				!c.Enforced && c.Validated ||
				!validForeignKeyAction(c.OnDelete, c.OnDeleteColumns, c.Columns) ||
				!validForeignKeyAction(c.OnUpdate, c.OnUpdateColumns, c.Columns) ||
				c.Match != "simple" && c.Match != "full" {
				return ErrInvalidDescriptor
			}
			for i := range c.Columns {
				if c.ParentForeignEqualityOperators[i] == 0 || c.ParentEqualityOperators[i] == 0 || c.ForeignEqualityOperators[i] == 0 {
					return ErrInvalidDescriptor
				}
			}
		case ConstraintUnique:
			if len(c.Columns) == 0 || c.Expression != nil || c.Index == 0 || !c.Enforced || !c.Validated && !c.Mutation {
				return ErrInvalidDescriptor
			}
		case ConstraintPrimaryKey:
			if len(c.Columns) == 0 || c.Expression != nil || !c.Enforced || !c.Validated && !c.Mutation {
				return ErrInvalidDescriptor
			}
		case ConstraintExclude:
			if c.Index == 0 || c.Expression != nil || len(c.Columns) == 0 ||
				len(c.Columns) != len(c.ExcludeOperators) || !c.Enforced || !c.Validated && !c.Mutation {
				return ErrInvalidDescriptor
			}
			for _, operator := range c.ExcludeOperators {
				if operator == 0 {
					return ErrInvalidDescriptor
				}
			}
		case ConstraintTrigger:
			if len(c.Columns) != 0 || c.Expression != nil || c.Index != 0 || !c.Enforced || !c.Validated {
				return ErrInvalidDescriptor
			}
		}
		for _, id := range c.Columns {
			index := slices.IndexFunc(columns, func(column ColumnDescriptor) bool { return column.ID == id })
			if index < 0 {
				return ErrInvalidDescriptor
			}
			if c.Kind == ConstraintNotNull && c.Enforced && columns[index].Nullable {
				return ErrInvalidDescriptor
			}
		}
	}
	return nil
}

func validForeignKeyAction(action string, setColumns, constrained []ColumnID) bool {
	switch action {
	case "no action", "restrict", "cascade":
		return len(setColumns) == 0
	case "set null", "set default":
		for _, column := range setColumns {
			if !slices.Contains(constrained, column) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func validateDescriptor(d DescriptorSpec) error {
	if d.ID == 0 || d.OID == 0 || d.Version == 0 || d.State > StateDropDeleteOnly || !validDescriptorText(d.Description) {
		return ErrInvalidDescriptor
	}
	if err := ValidateNamespaceName(d.Name); err != nil {
		return err
	}
	if err := validateDependencies(d.Dependencies); err != nil {
		return err
	}
	if d.Table != nil && d.Table.TypedOf != 0 {
		if d.Name.Kind != ObjectTable || !slices.ContainsFunc(d.Dependencies, func(dependency Dependency) bool {
			return dependency.Object == d.Table.TypedOf && dependency.Column == 0
		}) {
			return ErrInvalidDescriptor
		}
	}
	count := 0
	for _, present := range []bool{d.Schema != nil, d.Table != nil, d.Index != nil, d.Sequence != nil, d.View != nil, d.Routine != nil, d.Type != nil, d.CompositeRelation != nil, d.TextSearchTemplate != nil, d.TextSearchDictionary != nil, d.TextSearchConfiguration != nil, d.Cast != nil, d.Operator != nil, d.Aggregate != nil} {
		if present {
			count++
		}
	}
	if d.Name.Kind == ObjectMaterializedView {
		if count != 2 || d.Table == nil || d.View == nil {
			return ErrInvalidDescriptor
		}
	} else if count != 1 {
		return ErrInvalidDescriptor
	}
	physical := d.Name.Kind == ObjectTable || d.Name.Kind == ObjectIndex || d.Name.Kind == ObjectSequence || d.Name.Kind == ObjectMaterializedView
	if physical {
		if d.Physical.Generation == 0 || d.Physical.EncodingVersion == 0 {
			return ErrInvalidDescriptor
		}
	} else if d.Physical != (PhysicalIdentity{}) {
		return ErrInvalidDescriptor
	}
	switch d.Name.Kind {
	case ObjectSchema:
		if d.Schema == nil || d.Schema.Owner == 0 {
			return ErrInvalidDescriptor
		}
	case ObjectTable:
		return validateTableMetadata(d.Table, d.ID)
	case ObjectIndex:
		return validateIndexMetadata(d.ID, d.Index)
	case ObjectSequence:
		return validateSequenceMetadata(d.Sequence)
	case ObjectView:
		return validateViewMetadata(d.View, false)
	case ObjectMaterializedView:
		if err := validateTableMetadata(d.Table, d.ID); err != nil {
			return err
		}
		if err := validateViewMetadata(d.View, true); err != nil {
			return err
		}
		if d.View.Persistence != "" && d.Table.Persistence != "" && d.View.Persistence != d.Table.Persistence {
			return ErrInvalidDescriptor
		}
		return nil
	case ObjectRoutine:
		if d.Aggregate != nil {
			return validateAggregateMetadata(d.Aggregate)
		}
		return validateRoutineMetadata(d.Routine)
	case ObjectType:
		if d.Type == nil || d.Type.Layout.Type.ID != TypeID(d.OID) {
			return ErrInvalidDescriptor
		}
		if d.Type.Relation != 0 && !slices.ContainsFunc(d.Dependencies, func(dependency Dependency) bool {
			return dependency.Object == d.Type.Relation && dependency.Column == 0
		}) {
			return ErrInvalidDescriptor
		}
		return validateTypeMetadata(d.Type)
	case ObjectCompositeRelation:
		if d.CompositeRelation == nil || d.CompositeRelation.Owner == 0 || d.CompositeRelation.RowType == 0 {
			return ErrInvalidDescriptor
		}
		for _, column := range d.CompositeRelation.Columns {
			if column.SystemIdentity {
				return ErrInvalidDescriptor
			}
		}
		for _, column := range d.CompositeRelation.Columns {
			if column.ID > d.CompositeRelation.FieldIDHighWatermark {
				return ErrInvalidDescriptor
			}
		}
		return validateColumns(d.CompositeRelation.Columns)
	case ObjectTextSearchTemplate:
		return validateTextSearchTemplate(d.TextSearchTemplate)
	case ObjectTextSearchDictionary:
		return validateTextSearchDictionary(d.TextSearchDictionary)
	case ObjectTextSearchConfiguration:
		return validateTextSearchConfiguration(d.TextSearchConfiguration)
	case ObjectCast:
		return validateCastMetadata(d.Cast)
	case ObjectOperator:
		return validateOperatorMetadata(d.Operator)
	default:
		return ErrInvalidDescriptor
	}
	return nil
}

func validateTextSearchTemplate(template *TextSearchTemplateMetadata) error {
	if template == nil || template.Owner == 0 || template.Init == 0 || template.Lexize == 0 || template.Init == template.Lexize {
		return ErrInvalidDescriptor
	}
	return nil
}

func validateTextSearchDictionary(dictionary *TextSearchDictionaryMetadata) error {
	if dictionary == nil || dictionary.Owner == 0 || !utf8.ValidString(dictionary.InitOptions) || strings.IndexByte(dictionary.InitOptions, 0) >= 0 {
		return ErrInvalidDescriptor
	}
	if dictionary.StopWordsLanguage != "" && !HasNativeTextSearchStopWords(dictionary.StopWordsLanguage) {
		return ErrInvalidDescriptor
	}
	for _, name := range []string{dictionary.DictionaryFile, dictionary.AffixFile, dictionary.SynonymsFile, dictionary.ThesaurusFile} {
		if !ValidTextSearchDataName(name) {
			return ErrInvalidDescriptor
		}
	}
	switch dictionary.Kind {
	case TextSearchSimpleDictionary:
		if dictionary.Template != 3727 || dictionary.Language != "" {
			return ErrInvalidDescriptor
		}
	case TextSearchSnowballDictionary:
		if dictionary.Template == 0 || !ValidTextSearchSnowballLanguage(dictionary.Language) {
			return ErrInvalidDescriptor
		}
	case TextSearchIspellDictionary:
		if dictionary.Template == 0 || dictionary.DictionaryFile == "" || dictionary.AffixFile == "" || dictionary.Language != "" {
			return ErrInvalidDescriptor
		}
	case TextSearchSynonymDictionary:
		if dictionary.Template == 0 || dictionary.SynonymsFile == "" {
			return ErrInvalidDescriptor
		}
	case TextSearchThesaurusDictionary:
		if dictionary.Template == 0 || dictionary.ThesaurusFile == "" || dictionary.SubDictionary == 0 {
			return ErrInvalidDescriptor
		}
	default:
		return ErrInvalidDescriptor
	}
	for i, word := range dictionary.StopWords {
		if word == "" || !utf8.ValidString(word) || strings.IndexByte(word, 0) >= 0 || word != strings.ToLower(word) || i != 0 && dictionary.StopWords[i-1] >= word {
			return ErrInvalidDescriptor
		}
	}
	return nil
}

func validateTextSearchConfiguration(configuration *TextSearchConfigurationMetadata) error {
	if configuration == nil || configuration.Owner == 0 || configuration.Parser != 3722 {
		return ErrInvalidDescriptor
	}
	last := uint16(0)
	for _, mapping := range configuration.Mappings {
		if mapping.TokenType <= last || mapping.TokenType > 23 || !uniqueIDs(mapping.Dictionaries) {
			return ErrInvalidDescriptor
		}
		last = mapping.TokenType
	}
	return nil
}

func validateCastMetadata(cast *CastMetadata) error {
	if cast == nil || cast.Owner == 0 {
		return ErrInvalidDescriptor
	}
	if err := cast.Source.Validate(); err != nil {
		return err
	}
	if err := cast.Target.Validate(); err != nil {
		return err
	}
	if cast.Source.Type.ID == 0 || cast.Target.Type.ID == 0 || cast.Source.Type.ID == cast.Target.Type.ID {
		return ErrInvalidDescriptor
	}
	switch cast.Context {
	case 'i', 'a', 'e':
	default:
		return ErrInvalidDescriptor
	}
	switch cast.Method {
	case 'f':
		if cast.Function == 0 {
			return ErrInvalidDescriptor
		}
	case 'b', 'i':
		if cast.Function != 0 {
			return ErrInvalidDescriptor
		}
	default:
		return ErrInvalidDescriptor
	}
	return nil
}

func validAggregateFinalModify(modify AggregateFinalModify) bool {
	return modify >= AggregateFinalReadOnly && modify <= AggregateFinalReadWrite
}

func validateOperatorMetadata(operator *OperatorMetadata) error {
	if operator == nil || operator.Owner == 0 || operator.Name == "" || strings.IndexByte(operator.Name, 0) >= 0 {
		return ErrInvalidDescriptor
	}
	if operator.Left.Type.ID != 0 {
		if err := operator.Left.ValidateDeclaredLayout(); err != nil {
			return err
		}
	}
	if operator.Right.Type.ID != 0 {
		if err := operator.Right.ValidateDeclaredLayout(); err != nil {
			return err
		}
	}
	if operator.Left.Type.ID == 0 && operator.Right.Type.ID == 0 {
		return ErrInvalidDescriptor
	}
	if err := operator.Result.Validate(); err != nil {
		return err
	}
	if operator.Result.Type.ID == 0 {
		return ErrInvalidDescriptor
	}
	if operator.Function == 0 {
		return ErrInvalidDescriptor
	}
	// RESTRICT and JOIN are independent and optional, and HASHES/MERGES are
	// declaration flags: PostgreSQL permits any combination, so none of these
	// is a descriptor invariant.
	return nil
}

// PseudoTypeArrayElement maps a pseudo-type ARRAY identity onto its element
// pseudo-type, which is the value PostgreSQL records in provariadic for a
// VARIADIC aggregate argument. The pinned catalog declares these pairs as
// distinct pseudo-types with no array relationship (Element and Array are both
// zero for every 'p' entry), so the pairing is recorded here rather than
// inferred from the catalog. All three pairs were confirmed against the pinned
// 18.4 type catalog.
func PseudoTypeArrayElement(id TypeID) (TypeID, bool) {
	switch id {
	case 2277: // anyarray -> anyelement
		return 2283, true
	case 5078: // anycompatiblearray -> anycompatible
		return 5077, true
	case 4538: // anycompatiblemultirange -> anycompatiblerange
		return 5080, true
	}
	return 0, false
}

func validateAggregateMetadata(aggregate *AggregateMetadata) error {
	if aggregate == nil || aggregate.Owner == 0 {
		return ErrInvalidDescriptor
	}
	if aggregate.Kind < AggregateOrdinary || aggregate.Kind > AggregateHypotheticalSet {
		return ErrInvalidDescriptor
	}
	// A zero-argument aggregate is legal: count(*) is declared with no input
	// arguments and is keyed by PostgreSQL's "*" signature. The argument count
	// is bounded by the descriptor limits that already size every layout, and by
	// the binder's call-arity limit, exactly as a routine's parameter list is;
	// there is no separate aggregate-specific cap.
	for i := range aggregate.Arguments {
		if err := aggregate.Arguments[i].ValidateDeclaredLayout(); err != nil {
			return err
		}
		if aggregate.Arguments[i].Type.ID == 0 {
			return ErrInvalidDescriptor
		}
	}
	if len(aggregate.ArgumentNames) != 0 && len(aggregate.ArgumentNames) != len(aggregate.Arguments) {
		return ErrInvalidDescriptor
	}
	for _, name := range aggregate.ArgumentNames {
		if !validDescriptorText(name) {
			return ErrInvalidDescriptor
		}
	}
	if aggregate.Variadic != 0 {
		// VARIADIC applies to the final argument. A concrete argument declares the
		// array of the variadic element, while a pseudo-type array declares the
		// element pseudo-type itself: PostgreSQL records provariadic = anyelement
		// for `VARIADIC anyarray` and anycompatible for `VARIADIC
		// anycompatiblearray`, and keeps `any` as its own provariadic.
		if len(aggregate.Arguments) == 0 {
			return ErrInvalidDescriptor
		}
		last := aggregate.Arguments[len(aggregate.Arguments)-1]
		element, pseudoArray := PseudoTypeArrayElement(last.Type.ID)
		if pseudoArray {
			if last.Type.Storage != TypeAny || len(last.Children) != 0 || aggregate.Variadic != element {
				return ErrInvalidDescriptor
			}
		} else if last.Type.ID == 2276 {
			if last.Type.Storage != TypeAny || len(last.Children) != 0 || aggregate.Variadic != 2276 {
				return ErrInvalidDescriptor
			}
		} else if last.Type.Storage != TypeArray || len(last.Children) != 1 || last.Children[0].Type.ID != aggregate.Variadic {
			return ErrInvalidDescriptor
		}
	}
	if aggregate.DirectArguments > uint16(len(aggregate.Arguments)) {
		return ErrInvalidDescriptor
	}
	// The parser stores an ordered-set or hypothetical-set declaration as the
	// direct arguments followed by the aggregated (ORDER BY) arguments, so the
	// direct count is a prefix length rather than a ratio: pinned rank() carries
	// one direct and one aggregated argument under a VARIADIC dedup, appearing
	// here as DirectArguments == len(Arguments) == 1, while mode() carries none.
	// A hypothetical aggregate compares direct arguments against aggregated
	// ones, so it must declare at least one direct argument.
	if aggregate.Kind == AggregateHypotheticalSet && aggregate.DirectArguments == 0 {
		return ErrInvalidDescriptor
	}
	if aggregate.Kind == AggregateOrdinary && aggregate.DirectArguments != 0 {
		return ErrInvalidDescriptor
	}
	if aggregate.Transition == 0 {
		return ErrInvalidDescriptor
	}
	// The declared transition state keeps PostgreSQL's aggtranstype semantics: a
	// pseudo-type state such as `internal` or `anyelement` is recorded by identity
	// and resolved from the actual call's argument layouts at binding time.
	if err := aggregate.TransitionLayout.ValidateDeclaredLayout(); err != nil {
		return err
	}
	if aggregate.TransitionLayout.Type.ID == 0 {
		return ErrInvalidDescriptor
	}
	// The result is likewise a declared identity: a polymorphic final function
	// may publish anyelement or anyarray, which only the call can instantiate.
	if err := aggregate.Result.ValidateDeclaredLayout(); err != nil {
		return err
	}
	if aggregate.Result.Type.ID == 0 {
		return ErrInvalidDescriptor
	}
	if aggregate.TransitionSpace < 0 {
		return ErrInvalidDescriptor
	}
	if !validAggregateFinalModify(aggregate.FinalModify) {
		return ErrInvalidDescriptor
	}
	// MovingFinalModify is meaningful only alongside moving-aggregate functions;
	// a declaration without them keeps the zero value.
	if aggregate.MovingTransition == 0 && aggregate.MovingFinalModify != 0 {
		return ErrInvalidDescriptor
	}
	hasMoving := aggregate.MovingTransition != 0
	if hasMoving {
		if aggregate.MovingInverse == 0 {
			return ErrInvalidDescriptor
		}
		if err := aggregate.MovingTransitionLayout.ValidateDeclaredLayout(); err != nil {
			return err
		}
		if aggregate.MovingTransitionLayout.Type.ID != aggregate.TransitionLayout.Type.ID {
			return ErrInvalidDescriptor
		}
		// The moving final modifier is meaningful only when a moving final
		// function exists; without one it must stay at the zero value.
		if aggregate.MovingFinal == 0 && aggregate.MovingFinalModify != 0 {
			return ErrInvalidDescriptor
		}
		if aggregate.MovingFinal != 0 && !validAggregateFinalModify(aggregate.MovingFinalModify) {
			return ErrInvalidDescriptor
		}
		if aggregate.MovingTransitionSpace < 0 {
			return ErrInvalidDescriptor
		}
	} else if aggregate.MovingInverse != 0 || aggregate.MovingFinal != 0 ||
		aggregate.MovingTransitionLayout.Type.ID != 0 || aggregate.MovingTransitionSpace != 0 ||
		aggregate.MovingInitialValue != "" || aggregate.HasMovingInitialValue || aggregate.MovingFinalModify != 0 {
		return ErrInvalidDescriptor
	}
	// INITCOND = '' is a legal explicit empty initial value, so presence is
	// carried solely by the flag: absence forbids text, presence permits any.
	if !aggregate.HasInitialValue && aggregate.InitialValue != "" {
		return ErrInvalidDescriptor
	}
	if !aggregate.HasMovingInitialValue && aggregate.MovingInitialValue != "" {
		return ErrInvalidDescriptor
	}
	if !validDescriptorText(aggregate.InitialValue) || !validDescriptorText(aggregate.MovingInitialValue) {
		return ErrInvalidDescriptor
	}
	if aggregate.Parallel != "unsafe" && aggregate.Parallel != "restricted" && aggregate.Parallel != "safe" {
		return ErrInvalidDescriptor
	}
	if aggregate.Volatility != "immutable" && aggregate.Volatility != "stable" && aggregate.Volatility != "volatile" {
		return ErrInvalidDescriptor
	}
	return nil
}

func validateViewMetadata(view *ViewMetadata, materialized bool) error {
	if view == nil || view.Owner == 0 || view.Definition == "" || view.Query.SemanticVersion != 1 || view.Query.SQL != "" || len(view.Query.Encoding) == 0 {
		return ErrInvalidDescriptor
	}
	if view.CheckOption != "" && view.CheckOption != "local" && view.CheckOption != "cascaded" {
		return ErrInvalidDescriptor
	}
	if view.Persistence != "" && view.Persistence != "p" && view.Persistence != "t" {
		return ErrInvalidDescriptor
	}
	if materialized {
		if len(view.Triggers) != 0 || len(view.Columns) == 0 {
			return ErrInvalidDescriptor
		}
	} else if view.Populated {
		return ErrInvalidDescriptor
	}
	if err := validateColumns(view.Columns); err != nil {
		return err
	}
	if view.Automatic != nil {
		if materialized || view.Automatic.BaseRelation == 0 || view.Automatic.BaseOID == 0 {
			return ErrInvalidDescriptor
		}
		var previous ColumnID
		for _, mapping := range view.Automatic.Columns {
			if mapping.View <= previous || mapping.Base == 0 || !descriptorHasColumn(view.Columns, mapping.View) {
				return ErrInvalidDescriptor
			}
			previous = mapping.View
		}
	}
	if view.CheckOption != "" && (view.Automatic == nil || len(view.Automatic.Columns) == 0) {
		return ErrInvalidDescriptor
	}
	return validateTriggers(view.Triggers, view.Columns, true, nil, 0)
}

func validateTableMetadata(t *TableMetadata, tableID ObjectID) error {
	if t == nil || len(t.Columns) == 0 {
		return ErrInvalidDescriptor
	}
	if t.Persistence != "" && t.Persistence != "p" && t.Persistence != "t" {
		return ErrInvalidDescriptor
	}
	if t.OnCommit != "" && t.OnCommit != "preserve" && t.OnCommit != "delete" && t.OnCommit != "drop" {
		return ErrInvalidDescriptor
	}
	if t.Persistence != "t" && t.OnCommit != "" {
		return ErrInvalidDescriptor
	}
	if t.TypedOf == 0 && len(t.TypedColumns) != 0 {
		return ErrInvalidDescriptor
	}
	if t.TypedOf != 0 {
		fields := make(map[ColumnID]struct{}, len(t.TypedColumns))
		columns := make(map[ColumnID]struct{}, len(t.TypedColumns))
		for _, mapping := range t.TypedColumns {
			if mapping.Field == 0 || mapping.Column == 0 {
				return ErrInvalidDescriptor
			}
			if _, duplicate := fields[mapping.Field]; duplicate {
				return ErrInvalidDescriptor
			}
			if _, duplicate := columns[mapping.Column]; duplicate {
				return ErrInvalidDescriptor
			}
			fields[mapping.Field] = struct{}{}
			columns[mapping.Column] = struct{}{}
		}
		visible := 0
		for _, column := range t.Columns {
			if column.SystemIdentity {
				continue
			}
			visible++
			if _, mapped := columns[column.ID]; !mapped {
				return ErrInvalidDescriptor
			}
		}
		if visible != len(t.TypedColumns) {
			return ErrInvalidDescriptor
		}
	}
	if t.ColumnIDHighWatermark == 0 {
		return ErrInvalidDescriptor
	}
	for _, column := range t.Columns {
		if column.ID > t.ColumnIDHighWatermark {
			return ErrInvalidDescriptor
		}
		if column.SystemIdentity && (len(t.PrimaryKey) != 1 || t.PrimaryKey[0].Column != column.ID) {
			return ErrInvalidDescriptor
		}
	}
	if err := validateColumns(t.Columns); err != nil {
		return err
	}
	for _, column := range t.Columns {
		for _, source := range column.InheritedFrom {
			if source.Table == tableID {
				return ErrInvalidDescriptor
			}
		}
	}
	if err := validateKeys(t.PrimaryKey, t.Columns); err != nil {
		return err
	}
	if (t.PartitionStrategy == 0) != (len(t.PartitionKeys) == 0) {
		return ErrInvalidDescriptor
	}
	if t.PartitionDefault != 0 && (t.PartitionStrategy == 0 || t.PartitionDefault == tableID || t.InheritanceChildren == 0) {
		return ErrInvalidDescriptor
	}
	if t.PartitionStrategy != 0 {
		if t.PartitionStrategy != 'h' && t.PartitionStrategy != 'l' && t.PartitionStrategy != 'r' {
			return ErrInvalidDescriptor
		}
		if err := validateKeys(t.PartitionKeys, t.Columns); err != nil {
			return err
		}
		for _, key := range t.PartitionKeys {
			if key.OperatorClass == 0 {
				return ErrInvalidDescriptor
			}
		}
	}
	if t.PartitionBound == nil {
		if t.PartitionState != 0 {
			return ErrInvalidDescriptor
		}
	} else {
		if len(t.Parents) != 1 || t.Parents[0] == tableID || t.PartitionState != PartitionAttached && t.PartitionState != PartitionDetachPending {
			return ErrInvalidDescriptor
		}
		if err := validatePartitionBound(t.PartitionBound); err != nil {
			return err
		}
	}
	if !uniqueIDs(t.Indexes) || !uniqueIDs(t.Parents) {
		return ErrInvalidDescriptor
	}
	if err := validateExtendedStatistics(t.ExtendedStatistics, t.Columns); err != nil {
		return err
	}
	if err := validateConstraints(t.Constraints, t.Columns); err != nil {
		return err
	}
	return validateTriggers(t.Triggers, t.Columns, false, t.Constraints, tableID)
}

func validatePartitionBound(bound *PartitionBound) error {
	if bound == nil || bound.Strategy != 'h' && bound.Strategy != 'l' && bound.Strategy != 'r' ||
		bound.Definition == "" || !validDescriptorText(bound.Definition) {
		return ErrInvalidDescriptor
	}
	if bound.Default {
		if bound.Strategy == 'h' {
			return ErrInvalidDescriptor
		}
		if bound.Modulus != 0 || bound.Remainder != 0 || len(bound.ListValues) != 0 || len(bound.Lower) != 0 || len(bound.Upper) != 0 {
			return ErrInvalidDescriptor
		}
		return nil
	}
	validateDatum := func(datum PartitionDatum, allowSentinel bool) error {
		if err := datum.Layout.Validate(); err != nil {
			return err
		}
		switch datum.Kind {
		case PartitionDatumValue:
			if datum.Value.Type != datum.Layout.Type.Storage {
				return ErrInvalidDescriptor
			}
		case PartitionDatumMin, PartitionDatumMax:
			emptyValue := datum.Value.Type == TypeAny && !datum.Value.IsNull && datum.Value.IntVal == 0 &&
				datum.Value.FloatVal == 0 && datum.Value.StrVal == "" && len(datum.Value.BytesVal) == 0 &&
				len(datum.Value.VectorVal) == 0 && len(datum.Value.ArrayVal) == 0 &&
				len(datum.Value.Dimensions) == 0 && datum.Value.IntervalVal == (Interval{})
			if !allowSentinel || !emptyValue {
				return ErrInvalidDescriptor
			}
		default:
			return ErrInvalidDescriptor
		}
		return nil
	}
	sameDatumLayout := func(left, right PartitionDatum) bool {
		return TypeLayoutsEqual(left.Layout, right.Layout)
	}
	switch bound.Strategy {
	case 'h':
		if bound.Modulus == 0 || bound.Remainder >= bound.Modulus || len(bound.ListValues) != 0 || len(bound.Lower) != 0 || len(bound.Upper) != 0 {
			return ErrInvalidDescriptor
		}
	case 'l':
		if bound.Modulus != 0 || bound.Remainder != 0 || len(bound.ListValues) == 0 || len(bound.Lower) != 0 || len(bound.Upper) != 0 {
			return ErrInvalidDescriptor
		}
		for i, datum := range bound.ListValues {
			if err := validateDatum(datum, false); err != nil {
				return err
			}
			if i > 0 && !sameDatumLayout(bound.ListValues[0], datum) {
				return ErrInvalidDescriptor
			}
		}
	case 'r':
		if bound.Modulus != 0 || bound.Remainder != 0 || len(bound.ListValues) != 0 || len(bound.Lower) == 0 || len(bound.Lower) != len(bound.Upper) {
			return ErrInvalidDescriptor
		}
		for _, endpoint := range [][]PartitionDatum{bound.Lower, bound.Upper} {
			var sentinel PartitionDatumKind
			for i, datum := range endpoint {
				if err := validateDatum(datum, true); err != nil {
					return err
				}
				if !sameDatumLayout(bound.Lower[i], bound.Upper[i]) {
					return ErrInvalidDescriptor
				}
				if datum.Kind == PartitionDatumValue {
					if sentinel != 0 || datum.Value.IsNull {
						return ErrInvalidDescriptor
					}
					continue
				}
				if sentinel != 0 && sentinel != datum.Kind {
					return ErrInvalidDescriptor
				}
				sentinel = datum.Kind
			}
		}
	}
	return nil
}

func descriptorHasColumn(columns []ColumnDescriptor, id ColumnID) bool {
	for _, column := range columns {
		if column.ID == id {
			return true
		}
	}
	return false
}

func validateExtendedStatistics(statistics []ExtendedStatisticDescriptor, columns []ColumnDescriptor) error {
	oids := make(map[OID]struct{}, len(statistics))
	names := make(map[struct {
		namespace ObjectID
		name      string
	}]struct{}, len(statistics))
	for _, statistic := range statistics {
		validName := statistic.Name != "" && utf8.ValidString(statistic.Name) && strings.IndexByte(statistic.Name, 0) < 0
		if statistic.OID == 0 || statistic.Owner == 0 || statistic.Namespace == 0 || !validName || !validDescriptorText(statistic.Description) || statistic.TargetSet && (statistic.Target < 0 || statistic.Target > 10000) || !statistic.TargetSet && statistic.Target != 0 || len(statistic.Keys)+len(statistic.Expressions) < 2 || len(statistic.Kinds) == 0 {
			return ErrInvalidDescriptor
		}
		if _, duplicate := oids[statistic.OID]; duplicate {
			return ErrInvalidDescriptor
		}
		identity := struct {
			namespace ObjectID
			name      string
		}{namespace: statistic.Namespace, name: statistic.Name}
		if _, duplicate := names[identity]; duplicate {
			return ErrInvalidDescriptor
		}
		oids[statistic.OID] = struct{}{}
		names[identity] = struct{}{}
		last := ColumnID(0)
		for _, key := range statistic.Keys {
			if key <= last || !descriptorHasColumn(columns, key) {
				return ErrInvalidDescriptor
			}
			last = key
		}
		var kinds [256]bool
		for _, kind := range statistic.Kinds {
			if kinds[kind] || kind != 'd' && kind != 'f' && kind != 'm' && kind != 'e' {
				return ErrInvalidDescriptor
			}
			kinds[kind] = true
		}
		if kinds['e'] != (len(statistic.Expressions) != 0) {
			return ErrInvalidDescriptor
		}
		for _, expression := range statistic.Expressions {
			if expression == nil {
				return ErrInvalidDescriptor
			}
		}
	}
	return nil
}

func validateTriggers(triggers []TriggerDescriptor, columns []ColumnDescriptor, view bool, constraints []ConstraintDescriptor, tableID ObjectID) error {
	ids := make(map[ObjectID]struct{}, len(triggers))
	names := make(map[string]struct{}, len(triggers))
	for _, tr := range triggers {
		validIdentity := tr.ID != 0 && tr.OID != 0 && tr.Function != 0 && tr.Name != "" && tr.SchemaName != "" && utf8.ValidString(tr.Name) && utf8.ValidString(tr.SchemaName) && strings.IndexByte(tr.Name, 0) < 0 && strings.IndexByte(tr.SchemaName, 0) < 0 && validDescriptorText(tr.Description)
		validTiming := tr.Timing == "before" || tr.Timing == "after" || tr.Timing == "instead"
		validEnableMode := tr.EnableMode >= TriggerDisabled && tr.EnableMode <= TriggerEnabledAlways
		if !validIdentity || !validTiming || !validEnableMode || len(tr.Events) == 0 || !uniqueIDs(tr.Columns) {
			return ErrInvalidDescriptor
		}
		if tr.InitiallyDeferred && !tr.Deferrable {
			return ErrInvalidDescriptor
		}
		if (tr.Deferrable || tr.InitiallyDeferred) && !tr.Constraint {
			return ErrInvalidDescriptor
		}
		if tr.Constraint && (tr.Timing != "after" || !tr.ForEachRow) {
			return ErrInvalidDescriptor
		}
		if tr.Constraint {
			if tr.ConstraintTable != tableID || tr.ConstraintID == 0 {
				return ErrInvalidDescriptor
			}
			index := slices.IndexFunc(constraints, func(constraint ConstraintDescriptor) bool { return constraint.ID == tr.ConstraintID })
			if index < 0 || constraints[index].Kind != ConstraintTrigger {
				return ErrInvalidDescriptor
			}
		} else if tr.ConstraintTable != 0 || tr.ConstraintID != 0 || tr.ConstraintRelation != 0 || tr.ConstraintIndex != 0 {
			return ErrInvalidDescriptor
		}
		if tr.Timing == "instead" && (!tr.ForEachRow || tr.Constraint) {
			return ErrInvalidDescriptor
		}
		if view {
			if tr.ForEachRow && tr.Timing != "instead" {
				return ErrInvalidDescriptor
			}
		} else if tr.Timing == "instead" {
			return ErrInvalidDescriptor
		}
		hasUpdate := false
		eventNames := make(map[string]struct{}, len(tr.Events))
		for _, event := range tr.Events {
			switch event {
			case "insert", "delete", "update", "truncate":
			default:
				return ErrInvalidDescriptor
			}
			if _, exists := eventNames[event]; exists {
				return ErrInvalidDescriptor
			}
			eventNames[event] = struct{}{}
			hasUpdate = hasUpdate || event == "update"
			if event == "truncate" && tr.ForEachRow {
				return ErrInvalidDescriptor
			}
		}
		if len(tr.Columns) != 0 && !hasUpdate {
			return ErrInvalidDescriptor
		}
		hasTransition := tr.OldTransitionTable != "" || tr.NewTransitionTable != ""
		if hasTransition && (tr.Timing != "after" || tr.Constraint || len(tr.Columns) != 0) {
			return ErrInvalidDescriptor
		}
		if tr.Timing == "instead" && (tr.When != nil || hasTransition || slices.Contains(tr.Events, "truncate")) {
			return ErrInvalidDescriptor
		}
		if tr.OldTransitionTable != "" && !slices.Contains(tr.Events, "update") && !slices.Contains(tr.Events, "delete") {
			return ErrInvalidDescriptor
		}
		if tr.NewTransitionTable != "" && !slices.Contains(tr.Events, "update") && !slices.Contains(tr.Events, "insert") {
			return ErrInvalidDescriptor
		}
		if tr.When != nil && (tr.When.SemanticVersion != 1 || tr.When.SQL == "" || !validDescriptorText(tr.When.SQL) || len(tr.When.Encoding) == 0) {
			return ErrInvalidDescriptor
		}
		for _, argument := range tr.Arguments {
			if !validDescriptorText(argument) {
				return ErrInvalidDescriptor
			}
		}
		for _, transition := range []string{tr.OldTransitionTable, tr.NewTransitionTable} {
			if transition != "" && !validDescriptorText(transition) {
				return ErrInvalidDescriptor
			}
		}
		if tr.OldTransitionTable != "" && tr.OldTransitionTable == tr.NewTransitionTable {
			return ErrInvalidDescriptor
		}
		if _, ok := ids[tr.ID]; ok {
			return ErrInvalidDescriptor
		}
		ids[tr.ID] = struct{}{}
		if _, ok := names[tr.Name]; ok {
			return ErrInvalidDescriptor
		}
		names[tr.Name] = struct{}{}
		for _, id := range tr.Columns {
			if !containsColumn(columns, id) {
				return ErrInvalidDescriptor
			}
		}
	}
	return nil
}

func containsColumn(columns []ColumnDescriptor, id ColumnID) bool {
	for _, column := range columns {
		if column.ID == id {
			return true
		}
	}
	return false
}

func validateIndexMetadata(id ObjectID, index *IndexMetadata) error {
	if index == nil || index.Table == 0 || len(index.Keys) == 0 || index.DeferredUnique && !index.Unique {
		return ErrInvalidDescriptor
	}
	if index.ParentIndex == id || index.ParentIndex == index.Table {
		return ErrInvalidDescriptor
	}
	if index.Valid() && (!index.Ready() || !index.Live()) {
		return ErrInvalidDescriptor
	}
	if pending := index.PendingConstraint; pending != nil {
		invalid := pending.ID == 0 || pending.Index != id || pending.Name == "" || !validDescriptorText(pending.Name) ||
			!validDescriptorText(pending.Description) || len(pending.Columns) == 0 || !uniqueIDs(pending.Columns) ||
			pending.Expression != nil || pending.Mutation || !pending.Enforced || !pending.Validated ||
			pending.InitiallyDeferred && !pending.Deferrable
		if invalid {
			return ErrInvalidDescriptor
		}
		switch pending.Kind {
		case ConstraintPrimaryKey, ConstraintUnique:
			if len(pending.ExcludeOperators) != 0 {
				return ErrInvalidDescriptor
			}
		case ConstraintExclude:
			if len(pending.ExcludeOperators) != len(pending.Columns) || slices.Contains(pending.ExcludeOperators, OperatorID(0)) {
				return ErrInvalidDescriptor
			}
		default:
			return ErrInvalidDescriptor
		}
	}
	if index.PendingConstraint == nil && len(index.PendingDependencies) != 0 {
		return ErrInvalidDescriptor
	}
	if err := validateDependencies(index.PendingDependencies); err != nil {
		return err
	}
	for _, column := range index.Include {
		if column == 0 {
			return ErrInvalidDescriptor
		}
	}
	for _, key := range index.Keys {
		if key.Expression == nil && key.ExpressionName != "" ||
			key.Expression != nil && (key.ExpressionName == "" || !validDescriptorText(key.ExpressionName)) {
			return ErrInvalidDescriptor
		}
	}
	return validateKeys(index.Keys, nil)
}

func validateSequenceMetadata(s *SequenceMetadata) error {
	if s == nil || s.Identity == 0 || s.Incarnation == 0 {
		return ErrInvalidDescriptor
	}
	if s.Persistence != "" && s.Persistence != "p" && s.Persistence != "t" {
		return ErrInvalidDescriptor
	}
	return ValidateSequenceConfiguration(*s)
}

// ValidateSequenceConfiguration checks declaration options without allocating sequence state.
func ValidateSequenceConfiguration(s SequenceMetadata) error {
	validID := s.Type.ID == 20 || s.Type.ID == 21 || s.Type.ID == 23
	validType := validID && s.Type.Storage == TypeInt64
	validProgression := s.Increment != 0 && s.Cache > 0
	validRange := s.Min < s.Max && s.Start >= s.Min && s.Start <= s.Max
	pairedOwner := (s.OwnerTable == 0) == (s.OwnerColumn == 0)
	if !validType || !validProgression || !validRange || !pairedOwner {
		return ErrInvalidDescriptor
	}
	if s.Type.ID == 21 && (s.Min < math.MinInt16 || s.Max > math.MaxInt16) {
		return ErrInvalidDescriptor
	}
	if s.Type.ID == 23 && (s.Min < math.MinInt32 || s.Max > math.MaxInt32) {
		return ErrInvalidDescriptor
	}
	return nil
}

func validateRoutineMetadata(routine *RoutineMetadata) error {
	if routine == nil || routine.Owner == 0 || routine.LanguageOID == 0 || routine.SecurityDefiner {
		return ErrInvalidDescriptor
	}
	validLanguage := routine.Language == "internal" && routine.LanguageOID == 12 ||
		routine.Language == "c" && routine.LanguageOID == 13 ||
		routine.Language == "sql" && routine.LanguageOID == 14 ||
		routine.Language == "plpgsql" && routine.LanguageOID == 13563
	if !validLanguage {
		return ErrInvalidDescriptor
	}
	if routine.Volatility != "immutable" && routine.Volatility != "stable" && routine.Volatility != "volatile" {
		return ErrInvalidDescriptor
	}
	if routine.Parallel != "unsafe" && routine.Parallel != "restricted" && routine.Parallel != "safe" {
		return ErrInvalidDescriptor
	}
	for _, value := range []string{routine.Language, routine.Volatility, routine.Parallel, routine.Source, routine.Binary, routine.ExternalCode} {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return ErrInvalidDescriptor
		}
	}
	internal := routine.Language == "internal"
	external := internal || routine.Language == "c"
	bodyEmpty := routine.Body.SemanticVersion == 0 && routine.Body.SQL == "" && len(routine.Body.Encoding) == 0 && len(routine.Body.Dependencies) == 0
	if external {
		invalidBinding := internal && routine.Binary != "" || routine.Language == "c" && routine.Binary == ""
		if routine.ExternalCode == "" || !bodyEmpty || routine.Source != "" || invalidBinding {
			return ErrInvalidDescriptor
		}
	} else if routine.ExternalCode != "" || routine.Binary != "" || routine.Body.SemanticVersion != 1 || routine.Body.SQL == "" && len(routine.Body.Encoding) == 0 {
		return ErrInvalidDescriptor
	}
	if routine.Atomic && routine.Language != "sql" {
		return ErrInvalidDescriptor
	}
	if routine.Support == 0 != (routine.SupportFunction == 0) {
		return ErrInvalidDescriptor
	}
	if err := validateRoutineLayout(routine.Result, internal, false); err != nil {
		return err
	}
	if routine.Cost <= 0 || math.IsNaN(routine.Cost) || math.IsInf(routine.Cost, 0) {
		return ErrInvalidDescriptor
	}
	if routine.ReturnsSet {
		if routine.Rows <= 0 || math.IsNaN(routine.Rows) || math.IsInf(routine.Rows, 0) {
			return ErrInvalidDescriptor
		}
	} else if routine.Rows != 0 {
		return ErrInvalidDescriptor
	}
	if routine.Procedure && routine.ReturnsSet {
		return ErrInvalidDescriptor
	}
	settingNames := make(map[string]struct{}, len(routine.Settings))
	for _, setting := range routine.Settings {
		name := strings.ToLower(setting.Name)
		if name == "" || !utf8.ValidString(setting.Name) || strings.IndexByte(setting.Name, 0) >= 0 || !utf8.ValidString(setting.Value) || strings.IndexByte(setting.Value, 0) >= 0 || setting.Reset && setting.Value != "" {
			return ErrInvalidDescriptor
		}
		if _, duplicate := settingNames[name]; duplicate {
			return ErrInvalidDescriptor
		}
		settingNames[name] = struct{}{}
	}
	names := make(map[string]struct{}, len(routine.Parameters))
	defaultSeen := false
	variadicSeen := false
	for i, p := range routine.Parameters {
		if err := validateRoutineLayout(p.Layout, internal, true); err != nil {
			return err
		}
		switch p.Mode {
		case "in", "out", "inout", "table":
		case "variadic":
			if variadicSeen || i != len(routine.Parameters)-1 || p.Layout.Type.Storage != TypeArray {
				return ErrInvalidDescriptor
			}
			variadicSeen = true
		default:
			return ErrInvalidDescriptor
		}
		if p.Name != "" {
			if !utf8.ValidString(p.Name) || strings.IndexByte(p.Name, 0) >= 0 {
				return ErrInvalidDescriptor
			}
			if _, exists := names[p.Name]; exists {
				return ErrInvalidDescriptor
			}
			names[p.Name] = struct{}{}
		}
		input := p.Mode == "in" || p.Mode == "inout" || p.Mode == "variadic"
		if p.Default != nil {
			if !input || variadicSeen || external {
				return ErrInvalidDescriptor
			}
			defaultSeen = true
		} else if defaultSeen && input {
			return ErrInvalidDescriptor
		}
	}
	return nil
}

// validateRoutineLayout validates one routine parameter or result layout.
// Routine signatures carry declared pseudo-type identities (anyelement
// arguments, void/trigger/event_trigger results): they validate as declaration
// markers rather than physical layouts. A parameter position narrows the set
// to IsRoutineParameterPseudoTypeID — void, trigger, and the handler
// pseudo-types are result-only. `internal` additionally admits its bounded
// byte payload, which is that pseudo-type's established state representation;
// the marker spelling is restricted to `internal` routines so an ordinary
// signature cannot carry internal state.
func validateRoutineLayout(layout TypeLayout, internal, parameter bool) error {
	if IsPseudoTypeID(layout.Type.ID) {
		if layout.Type.ID != 2281 {
			if parameter && !IsRoutineParameterPseudoTypeID(layout.Type.ID) {
				return ErrInvalidTypeLayout
			}
			return layout.validateDeclaredMarker()
		}
		if internal && layout.Type.Storage == TypeAny && layout.Type.Modifier == -1 && len(layout.Children) == 0 && len(layout.FieldNames) == 0 {
			return nil
		}
		if layout.Type.Storage == TypeBytes && layout.Type.Modifier == -1 && len(layout.Children) == 0 && len(layout.FieldNames) == 0 {
			return nil
		}
		return ErrInvalidTypeLayout
	}
	// A routine's composite result (the record assembled from multiple OUT
	// parameters) may embed marker children; every other node still validates
	// physically.
	return layout.ValidateDeclaredLayout()
}

func validateTypeMetadata(metadata *TypeMetadata) error {
	if metadata == nil {
		return ErrInvalidDescriptor
	}
	if err := metadata.Layout.Validate(); err != nil {
		return err
	}
	kind := metadata.DeclaredKind()
	if !metadata.Kind.Valid() {
		return ErrInvalidDescriptor
	}
	if metadata.Array == 0 && kind != TypeKindArray && kind != TypeKindMultirange && kind != TypeKindShell {
		return ErrInvalidDescriptor
	}
	if metadata.BaseLayout.Type.ID != 0 {
		if err := metadata.BaseLayout.Validate(); err != nil {
			return err
		}
	}
	if len(metadata.ElementOIDs) != 0 || metadata.Kind == TypeKindEnum {
		if len(metadata.ElementOIDs) != len(metadata.Elements) {
			return ErrInvalidDescriptor
		}
		labels := make(map[string]struct{}, len(metadata.Elements))
		elements := make(map[OID]struct{}, len(metadata.ElementOIDs))
		for i, oid := range metadata.ElementOIDs {
			label := metadata.Elements[i]
			if oid == 0 || !utf8.ValidString(label) || strings.IndexByte(label, 0) >= 0 {
				return ErrInvalidDescriptor
			}
			if _, exists := labels[label]; exists {
				return ErrInvalidDescriptor
			}
			if _, exists := elements[oid]; exists {
				return ErrInvalidDescriptor
			}
			labels[label] = struct{}{}
			elements[oid] = struct{}{}
		}
		unsafe := make(map[OID]struct{}, len(metadata.UnsafeElementOIDs))
		for _, oid := range metadata.UnsafeElementOIDs {
			if _, exists := elements[oid]; !exists {
				return ErrInvalidDescriptor
			}
			if _, exists := unsafe[oid]; exists {
				return ErrInvalidDescriptor
			}
			unsafe[oid] = struct{}{}
		}
	} else if len(metadata.UnsafeElementOIDs) != 0 || len(metadata.Elements) != 0 {
		return ErrInvalidDescriptor
	}
	if err := validateColumns(metadata.Fields); err != nil {
		return err
	}
	switch kind {
	case TypeKindDomain:
		if metadata.BaseLayout.Type.ID == 0 || metadata.Layout.Type.Storage != metadata.BaseLayout.Type.Storage || len(metadata.Elements) != 0 || len(metadata.Fields) != 0 || metadata.Range != nil {
			return ErrInvalidDescriptor
		}
	case TypeKindEnum:
		if metadata.Layout.Type.Storage != TypeInt64 || metadata.Base != 0 || metadata.BaseLayout.Type.ID != 0 || len(metadata.Fields) != 0 || metadata.Range != nil {
			return ErrInvalidDescriptor
		}
	case TypeKindComposite:
		if metadata.Layout.Type.Storage != TypeComposite || metadata.Base != 0 || metadata.BaseLayout.Type.ID != 0 || len(metadata.Elements) != 0 || metadata.Range != nil || len(metadata.Fields) != len(metadata.Layout.Children) {
			return ErrInvalidDescriptor
		}
		for i := range metadata.Fields {
			if !TypeLayoutsEqual(metadata.Fields[i].Layout, metadata.Layout.Children[i]) {
				return ErrInvalidDescriptor
			}
			if metadata.FieldIDHighWatermark != 0 && metadata.Fields[i].ID > metadata.FieldIDHighWatermark {
				return ErrInvalidDescriptor
			}
		}
	case TypeKindArray:
		if metadata.Layout.Type.Storage != TypeArray || metadata.Base == 0 || metadata.BaseLayout.Type.ID == 0 || metadata.Array != 0 || len(metadata.Layout.Children) != 1 || !TypeLayoutsEqual(metadata.Layout.Children[0], metadata.BaseLayout) || len(metadata.Elements) != 0 || len(metadata.Fields) != 0 || metadata.Range != nil || metadata.Default != nil || metadata.NotNull || len(metadata.Constraints) != 0 {
			return ErrInvalidDescriptor
		}
	case TypeKindShell:
		if metadata.Layout.Type.Storage != TypeBytes || metadata.Array != 0 || metadata.Base != 0 || metadata.BaseLayout.Type.ID != 0 || len(metadata.Elements) != 0 || len(metadata.Fields) != 0 || metadata.Range != nil {
			return ErrInvalidDescriptor
		}
	case TypeKindRange, TypeKindMultirange:
		if metadata.Layout.Type.Storage != TypeBytes || metadata.Base != 0 || metadata.BaseLayout.Type.ID != 0 || len(metadata.Elements) != 0 || len(metadata.Fields) != 0 || metadata.Range == nil || metadata.Default != nil || metadata.NotNull || len(metadata.Constraints) != 0 {
			return ErrInvalidDescriptor
		}
		if err := metadata.Range.Subtype.Validate(); err != nil {
			return err
		}
		if metadata.Range.OperatorClass == 0 || metadata.Range.Multirange == 0 || metadata.Range.Range == 0 || (metadata.Range.Canonical == 0) != (metadata.Range.CanonicalFunction == 0) || (metadata.Range.Difference == 0) != (metadata.Range.DifferenceFunction == 0) {
			return ErrInvalidDescriptor
		}
	default:
		return ErrInvalidDescriptor
	}
	if kind != TypeKindComposite && len(metadata.Fields) != 0 {
		return ErrInvalidDescriptor
	}
	if kind != TypeKindComposite && metadata.Relation != 0 {
		return ErrInvalidDescriptor
	}
	if kind != TypeKindDomain && (metadata.Default != nil || metadata.NotNull || len(metadata.Constraints) != 0) {
		return ErrInvalidDescriptor
	}
	return validateConstraints(metadata.Constraints, metadata.Fields)
}
