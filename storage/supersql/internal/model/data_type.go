package model

func (t DataType) Valid() bool {
	switch t {
	case TypeInt64, TypeFloat64, TypeTimestamp, TypeBytes, TypeString,
		TypeVectorF32, TypeJSONB, TypeArray, TypeBoolean, TypeUUID,
		TypeNumeric, TypeComposite, TypeInterval:
		return true
	}
	return false
}
