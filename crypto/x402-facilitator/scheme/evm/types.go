package evm

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/crypto/sha3"

	"github.com/gosuda/x402-facilitator/types"
)

func NewEVMPayload(chain, token, from, to string, value string, signer types.Signer) (*EVMPayload, error) {
	valueBig, ok := big.NewInt(0).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("invalid value: %s", value)
	}
	authorization := NewAuthorization(from, to, valueBig)
	domain := GetDomainConfig(chain, token)
	if domain == nil {
		return nil, fmt.Errorf("domain config not found for chain %s and token %s", chain, token)
	}
	signature, err := SignEip3009(authorization, domain, signer)
	if err != nil {
		return nil, err
	}
	return &EVMPayload{
		Signature:     signature,
		Authorization: authorization,
	}, nil

}

// ExactEvmPayloadAuthorization represents the payload for an exact EVM payment ERC-3009
// authorization EIP-712 typed data message
type EVMPayload struct {
	Signature     string         `json:"signature"`
	Authorization *Authorization `json:"authorization"`
}

func NewAuthorization(from, to string, value *big.Int) *Authorization {
	now := time.Now().Unix()
	authorization := &Authorization{
		From:        common.HexToAddress(from),
		To:          common.HexToAddress(to),
		Value:       value,
		ValidAfter:  big.NewInt(0),
		ValidBefore: big.NewInt(now + 3600), // 1 hour
		Nonce:       GenerateEIP3009Nonce(),
	}
	return authorization
}

// TransferWithAuthorization represents the payload for an EIP-3009
// authorization EIP-712 typed data message
type Authorization struct {
	From        common.Address
	To          common.Address
	Value       *big.Int
	ValidAfter  *big.Int
	ValidBefore *big.Int
	Nonce       [32]byte
}

type authorizationWire struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

func (a Authorization) MarshalJSON() ([]byte, error) {
	return json.Marshal(authorizationWire{
		From:        a.From.Hex(),
		To:          a.To.Hex(),
		Value:       decimalBigInt(a.Value),
		ValidAfter:  decimalBigInt(a.ValidAfter),
		ValidBefore: decimalBigInt(a.ValidBefore),
		Nonce:       hex32(a.Nonce),
	})
}

func (a *Authorization) UnmarshalJSON(data []byte) error {
	var w authorizationWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	a.From = common.HexToAddress(w.From)
	a.To = common.HexToAddress(w.To)
	var err error
	if a.Value, err = parseDecimalBigInt(w.Value); err != nil {
		return fmt.Errorf("authorization.value: %w", err)
	}
	if a.ValidAfter, err = parseDecimalBigInt(w.ValidAfter); err != nil {
		return fmt.Errorf("authorization.validAfter: %w", err)
	}
	if a.ValidBefore, err = parseDecimalBigInt(w.ValidBefore); err != nil {
		return fmt.Errorf("authorization.validBefore: %w", err)
	}
	if a.Nonce, err = parseHex32(w.Nonce); err != nil {
		return fmt.Errorf("authorization.nonce: %w", err)
	}
	return nil
}

var (
	// EIP-3009 domain separator
	AuthorizationTypeHash = Keccak256([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
)

func (a Authorization) ToMessageHash() []byte {
	encoded := bytes.Join([][]byte{
		AuthorizationTypeHash,
		padAddress(a.From),
		padAddress(a.To),
		padBigInt(a.Value),
		padBigInt(a.ValidAfter),
		padBigInt(a.ValidBefore),
		a.Nonce[:], // already 32 bytes
	}, nil)
	return Keccak256(encoded)
}

func NewDomainConfig(name, version string, chainID *big.Int, verifyingContract string) *DomainConfig {
	return &DomainConfig{
		Name:              name,
		Version:           version,
		ChainID:           chainID,
		VerifyingContract: common.HexToAddress(verifyingContract),
	}
}

// DomainConfig represents the domain configuration for EIP-712
// typed data messages
type DomainConfig struct {
	Name              string
	Version           string
	ChainID           *big.Int
	VerifyingContract common.Address
}

var (
	// EIP-712 domain separator
	DomainTypeHash = Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
)

func (d DomainConfig) ToMessageHash() []byte {
	nameHash := Keccak256([]byte(d.Name))
	versionHash := Keccak256([]byte(d.Version))
	chainID := padBigInt(d.ChainID)
	contract := padAddress(d.VerifyingContract)

	return Keccak256(
		DomainTypeHash,
		nameHash,
		versionHash,
		chainID,
		contract,
	)
}

func GetAddrssFromPrivateKey(privateKey []byte) (common.Address, error) {
	if len(privateKey) != 32 {
		return common.Address{}, errors.New("invalid private key length")
	}

	privKey := secp256k1.PrivKeyFromBytes(privateKey)
	uncompressed := privKey.PubKey().SerializeUncompressed()
	address := common.BytesToAddress(Keccak256(uncompressed[1:])[12:])

	return address, nil
}

func GenerateEIP3009Nonce() [32]byte {
	var nonce [32]byte
	rand.Read(nonce[:])
	return nonce
}

func Keccak256(data ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, b := range data {
		h.Write(b)
	}
	return h.Sum(nil)
}

func padAddress(addr common.Address) []byte {
	return append(make([]byte, 12), addr[:]...)
}

func padBigInt(n *big.Int) []byte {
	return leftPadBytes(n.Bytes(), 32)
}

func leftPadBytes(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	padded := make([]byte, size)
	copy(padded[size-len(b):], b)
	return padded
}

func decimalBigInt(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

func parseDecimalBigInt(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("empty decimal")
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	return n, nil
}

func hex32(b [32]byte) string {
	return "0x" + hex.EncodeToString(b[:])
}

func parseHex32(s string) ([32]byte, error) {
	var out [32]byte
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("invalid hex: %w", err)
	}
	if len(b) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// Utility to convert hex string to Address
func ParseAddress(hexStr string) (common.Address, error) {
	var a common.Address
	hexStr = strings.TrimPrefix(hexStr, "0x")
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 20 {
		return a, errors.New("invalid address")
	}
	copy(a[:], b)
	return a, nil
}

// Permit2TokenPermissions represents the permitted token and amount for Permit2.
type Permit2TokenPermissions struct {
	Token  common.Address `json:"token"`
	Amount *big.Int       `json:"amount"`
}

type permit2TokenPermissionsWire struct {
	Token  string `json:"token"`
	Amount string `json:"amount"`
}

func (p Permit2TokenPermissions) MarshalJSON() ([]byte, error) {
	return json.Marshal(permit2TokenPermissionsWire{
		Token:  p.Token.Hex(),
		Amount: decimalBigInt(p.Amount),
	})
}

func (p *Permit2TokenPermissions) UnmarshalJSON(data []byte) error {
	var w permit2TokenPermissionsWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	p.Token = common.HexToAddress(w.Token)
	var err error
	if p.Amount, err = parseDecimalBigInt(w.Amount); err != nil {
		return fmt.Errorf("permit2TokenPermissions.amount: %w", err)
	}
	return nil
}

// Permit2Witness represents witness data for x402ExactPermit2Proxy.
type Permit2Witness struct {
	To         common.Address `json:"to"`
	ValidAfter *big.Int       `json:"validAfter"`
}

type permit2WitnessWire struct {
	To         string `json:"to"`
	ValidAfter string `json:"validAfter"`
}

func (pw Permit2Witness) MarshalJSON() ([]byte, error) {
	return json.Marshal(permit2WitnessWire{
		To:         pw.To.Hex(),
		ValidAfter: decimalBigInt(pw.ValidAfter),
	})
}

func (pw *Permit2Witness) UnmarshalJSON(data []byte) error {
	var w permit2WitnessWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	pw.To = common.HexToAddress(w.To)
	var err error
	if pw.ValidAfter, err = parseDecimalBigInt(w.ValidAfter); err != nil {
		return fmt.Errorf("permit2Witness.validAfter: %w", err)
	}
	return nil
}

// Permit2Authorization represents the PermitWitnessTransferFrom parameters.
type Permit2Authorization struct {
	From      common.Address          `json:"from"`
	Permitted Permit2TokenPermissions `json:"permitted"`
	Spender   common.Address          `json:"spender"`
	Nonce     *big.Int                `json:"nonce"`
	Deadline  *big.Int                `json:"deadline"`
	Witness   Permit2Witness          `json:"witness"`
}

type permit2AuthorizationWire struct {
	From      string                  `json:"from"`
	Permitted Permit2TokenPermissions `json:"permitted"`
	Spender   string                  `json:"spender"`
	Nonce     string                  `json:"nonce"`
	Deadline  string                  `json:"deadline"`
	Witness   Permit2Witness          `json:"witness"`
}

func (a Permit2Authorization) MarshalJSON() ([]byte, error) {
	return json.Marshal(permit2AuthorizationWire{
		From:      a.From.Hex(),
		Permitted: a.Permitted,
		Spender:   a.Spender.Hex(),
		Nonce:     decimalBigInt(a.Nonce),
		Deadline:  decimalBigInt(a.Deadline),
		Witness:   a.Witness,
	})
}

func (a *Permit2Authorization) UnmarshalJSON(data []byte) error {
	var w permit2AuthorizationWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	a.From = common.HexToAddress(w.From)
	a.Permitted = w.Permitted
	a.Spender = common.HexToAddress(w.Spender)
	var err error
	if a.Nonce, err = parseDecimalBigInt(w.Nonce); err != nil {
		return fmt.Errorf("permit2Authorization.nonce: %w", err)
	}
	if a.Deadline, err = parseDecimalBigInt(w.Deadline); err != nil {
		return fmt.Errorf("permit2Authorization.deadline: %w", err)
	}
	a.Witness = w.Witness
	return nil
}

// Permit2Payload represents a Permit2 payment payload.
type Permit2Payload struct {
	Signature            string                `json:"signature"`
	Permit2Authorization *Permit2Authorization `json:"permit2Authorization"`
}

var (
	// Permit2 EIP-712 type hashes
	Permit2WitnessTypeHash                   = Keccak256([]byte("Witness(address to,uint256 validAfter)"))
	Permit2TokenPermissionsTypeHash          = Keccak256([]byte("TokenPermissions(address token,uint256 amount)"))
	Permit2PermitWitnessTransferFromTypeHash = Keccak256([]byte("PermitWitnessTransferFrom(TokenPermissions permitted,address spender,uint256 nonce,uint256 deadline,Witness witness)TokenPermissions(address token,uint256 amount)Witness(address to,uint256 validAfter)"))

	// Permit2 EIP-712 domain separator (no version field)
	Permit2DomainTypeHash = Keccak256([]byte("EIP712Domain(string name,uint256 chainId,address verifyingContract)"))
)

func (a Permit2Authorization) ToMessageHash() []byte {
	permittedHash := Keccak256(bytes.Join([][]byte{
		Permit2TokenPermissionsTypeHash,
		padAddress(a.Permitted.Token),
		padBigInt(a.Permitted.Amount),
	}, nil))
	witnessHash := Keccak256(bytes.Join([][]byte{
		Permit2WitnessTypeHash,
		padAddress(a.Witness.To),
		padBigInt(a.Witness.ValidAfter),
	}, nil))
	encoded := bytes.Join([][]byte{
		Permit2PermitWitnessTransferFromTypeHash,
		permittedHash,
		padAddress(a.Spender),
		padBigInt(a.Nonce),
		padBigInt(a.Deadline),
		witnessHash,
	}, nil)
	return Keccak256(encoded)
}

// Permit2DomainConfig represents the domain configuration for Permit2
// EIP-712 typed data messages (no version field)
type Permit2DomainConfig struct {
	Name              string
	ChainID           *big.Int
	VerifyingContract common.Address
}

func (d Permit2DomainConfig) ToMessageHash() []byte {
	nameHash := Keccak256([]byte(d.Name))
	chainID := padBigInt(d.ChainID)
	contract := padAddress(d.VerifyingContract)

	return Keccak256(
		Permit2DomainTypeHash,
		nameHash,
		chainID,
		contract,
	)
}

func NewPermit2Payload(chain, token, from, to string, value string, signer types.Signer) (*Permit2Payload, error) {
	valueBig, ok := big.NewInt(0).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("invalid value: %s", value)
	}

	tokenAddr := GetTokenAddress(chain, token)
	if tokenAddr == (common.Address{}) {
		return nil, fmt.Errorf("token address not found for chain %s and token %s", chain, token)
	}

	chainID := GetChainID(chain)
	if chainID == nil {
		return nil, fmt.Errorf("unsupported chain: %s", chain)
	}

	now := time.Now().Unix()
	nonce := GeneratePermit2Nonce()

	auth := &Permit2Authorization{
		From: common.HexToAddress(from),
		Permitted: Permit2TokenPermissions{
			Token:  tokenAddr,
			Amount: valueBig,
		},
		Spender:  X402ExactPermit2ProxyAddress,
		Nonce:    nonce,
		Deadline: big.NewInt(now + 3600), // 1 hour
		Witness: Permit2Witness{
			To:         common.HexToAddress(to),
			ValidAfter: big.NewInt(0),
		},
	}

	signature, err := SignPermit2(auth, chainID, signer)
	if err != nil {
		return nil, err
	}

	return &Permit2Payload{
		Signature:            signature,
		Permit2Authorization: auth,
	}, nil
}

// GeneratePermit2Nonce generates a random uint256 nonce for Permit2.
func GeneratePermit2Nonce() *big.Int {
	nonce := make([]byte, 32)
	rand.Read(nonce)
	return new(big.Int).SetBytes(nonce)
}

// IsPermit2PayloadJSON checks if a JSON payload contains permit2Authorization.
func IsPermit2PayloadJSON(data json.RawMessage) bool {
	var probe struct {
		Permit2Authorization json.RawMessage `json:"permit2Authorization"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	// Reject null, empty, or missing permit2Authorization
	return len(probe.Permit2Authorization) > 0 &&
		string(probe.Permit2Authorization) != "null"
}

func ParseSignature(sigHex string) ([]byte, error) {
	sigHex = strings.TrimPrefix(sigHex, "0x")
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}

	if len(sig) != 65 {
		return nil, errors.New("invalid signature length")
	}
	if sig[64] < 27 {
		sig[64] += 27
	}
	if sig[64] != 27 && sig[64] != 28 {
		return nil, errors.New("invalid signature v value")
	}
	return sig, nil
}
