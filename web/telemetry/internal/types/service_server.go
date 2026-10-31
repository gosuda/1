package types

type ServerService interface {
	GenerateID() (int64, error)
	GenerateIDString() (string, error)
}
