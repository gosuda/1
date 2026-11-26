// Code generated - DO NOT EDIT.
// This file is a generated binding and any manual changes will be lost.

package permit2

import (
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// Reference imports to suppress errors if they are not otherwise used.
var (
	_ = errors.New
	_ = big.NewInt
	_ = strings.NewReader
	_ = ethereum.NotFound
	_ = bind.Bind
	_ = common.Big1
	_ = types.BloomLookup
	_ = event.NewSubscription
	_ = abi.ConvertType
)

// Struct1 is an auto generated low-level Go binding around an user-defined struct.
type Struct1 struct {
	Permitted Struct0
	Nonce     *big.Int
	Deadline  *big.Int
}

// Struct0 is an auto generated low-level Go binding around an user-defined struct.
type Struct0 struct {
	Token  common.Address
	Amount *big.Int
}

// Permit2MetaData contains all meta data concerning the Permit2 contract.
var Permit2MetaData = &bind.MetaData{
	ABI: "[{\"name\":\"settle\",\"type\":\"function\",\"inputs\":[{\"name\":\"permit\",\"type\":\"tuple\",\"components\":[{\"name\":\"permitted\",\"type\":\"tuple\",\"components\":[{\"name\":\"token\",\"type\":\"address\"},{\"name\":\"amount\",\"type\":\"uint256\"}]},{\"name\":\"nonce\",\"type\":\"uint256\"},{\"name\":\"deadline\",\"type\":\"uint256\"}]},{\"name\":\"owner\",\"type\":\"address\"},{\"name\":\"witness\",\"type\":\"tuple\",\"components\":[{\"name\":\"to\",\"type\":\"address\"},{\"name\":\"validAfter\",\"type\":\"uint256\"}]},{\"name\":\"signature\",\"type\":\"bytes\"}],\"outputs\":[],\"stateMutability\":\"nonpayable\"},{\"name\":\"balanceOf\",\"type\":\"function\",\"inputs\":[{\"name\":\"account\",\"type\":\"address\"}],\"outputs\":[{\"name\":\"balance\",\"type\":\"uint256\"}],\"stateMutability\":\"view\"}]",
}

// Permit2ABI is the input ABI used to generate the binding from.
// Deprecated: Use Permit2MetaData.ABI instead.
var Permit2ABI = Permit2MetaData.ABI

// Permit2 is an auto generated Go binding around an Ethereum contract.
type Permit2 struct {
	Permit2Caller     // Read-only binding to the contract
	Permit2Transactor // Write-only binding to the contract
	Permit2Filterer   // Log filterer for contract events
}

// Permit2Caller is an auto generated read-only Go binding around an Ethereum contract.
type Permit2Caller struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// Permit2Transactor is an auto generated write-only Go binding around an Ethereum contract.
type Permit2Transactor struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// Permit2Filterer is an auto generated log filtering Go binding around an Ethereum contract events.
type Permit2Filterer struct {
	contract *bind.BoundContract // Generic contract wrapper for the low level calls
}

// Permit2Session is an auto generated Go binding around an Ethereum contract,
// with pre-set call and transact options.
type Permit2Session struct {
	Contract     *Permit2          // Generic contract binding to set the session for
	CallOpts     bind.CallOpts     // Call options to use throughout this session
	TransactOpts bind.TransactOpts // Transaction auth options to use throughout this session
}

// Permit2CallerSession is an auto generated read-only Go binding around an Ethereum contract,
// with pre-set call options.
type Permit2CallerSession struct {
	Contract *Permit2Caller // Generic contract caller binding to set the session for
	CallOpts bind.CallOpts  // Call options to use throughout this session
}

// Permit2TransactorSession is an auto generated write-only Go binding around an Ethereum contract,
// with pre-set transact options.
type Permit2TransactorSession struct {
	Contract     *Permit2Transactor // Generic contract transactor binding to set the session for
	TransactOpts bind.TransactOpts  // Transaction auth options to use throughout this session
}

// Permit2Raw is an auto generated low-level Go binding around an Ethereum contract.
type Permit2Raw struct {
	Contract *Permit2 // Generic contract binding to access the raw methods on
}

// Permit2CallerRaw is an auto generated low-level read-only Go binding around an Ethereum contract.
type Permit2CallerRaw struct {
	Contract *Permit2Caller // Generic read-only contract binding to access the raw methods on
}

// Permit2TransactorRaw is an auto generated low-level write-only Go binding around an Ethereum contract.
type Permit2TransactorRaw struct {
	Contract *Permit2Transactor // Generic write-only contract binding to access the raw methods on
}

// NewPermit2 creates a new instance of Permit2, bound to a specific deployed contract.
func NewPermit2(address common.Address, backend bind.ContractBackend) (*Permit2, error) {
	contract, err := bindPermit2(address, backend, backend, backend)
	if err != nil {
		return nil, err
	}
	return &Permit2{Permit2Caller: Permit2Caller{contract: contract}, Permit2Transactor: Permit2Transactor{contract: contract}, Permit2Filterer: Permit2Filterer{contract: contract}}, nil
}

// NewPermit2Caller creates a new read-only instance of Permit2, bound to a specific deployed contract.
func NewPermit2Caller(address common.Address, caller bind.ContractCaller) (*Permit2Caller, error) {
	contract, err := bindPermit2(address, caller, nil, nil)
	if err != nil {
		return nil, err
	}
	return &Permit2Caller{contract: contract}, nil
}

// NewPermit2Transactor creates a new write-only instance of Permit2, bound to a specific deployed contract.
func NewPermit2Transactor(address common.Address, transactor bind.ContractTransactor) (*Permit2Transactor, error) {
	contract, err := bindPermit2(address, nil, transactor, nil)
	if err != nil {
		return nil, err
	}
	return &Permit2Transactor{contract: contract}, nil
}

// NewPermit2Filterer creates a new log filterer instance of Permit2, bound to a specific deployed contract.
func NewPermit2Filterer(address common.Address, filterer bind.ContractFilterer) (*Permit2Filterer, error) {
	contract, err := bindPermit2(address, nil, nil, filterer)
	if err != nil {
		return nil, err
	}
	return &Permit2Filterer{contract: contract}, nil
}

// bindPermit2 binds a generic wrapper to an already deployed contract.
func bindPermit2(address common.Address, caller bind.ContractCaller, transactor bind.ContractTransactor, filterer bind.ContractFilterer) (*bind.BoundContract, error) {
	parsed, err := Permit2MetaData.GetAbi()
	if err != nil {
		return nil, err
	}
	return bind.NewBoundContract(address, *parsed, caller, transactor, filterer), nil
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Permit2 *Permit2Raw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _Permit2.Contract.Permit2Caller.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Permit2 *Permit2Raw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Permit2.Contract.Permit2Transactor.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Permit2 *Permit2Raw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Permit2.Contract.Permit2Transactor.contract.Transact(opts, method, params...)
}

// Call invokes the (constant) contract method with params as input values and
// sets the output to result. The result type might be a single field for simple
// returns, a slice of interfaces for anonymous returns and a struct for named
// returns.
func (_Permit2 *Permit2CallerRaw) Call(opts *bind.CallOpts, result *[]interface{}, method string, params ...interface{}) error {
	return _Permit2.Contract.contract.Call(opts, result, method, params...)
}

// Transfer initiates a plain transaction to move funds to the contract, calling
// its default method if one is available.
func (_Permit2 *Permit2TransactorRaw) Transfer(opts *bind.TransactOpts) (*types.Transaction, error) {
	return _Permit2.Contract.contract.Transfer(opts)
}

// Transact invokes the (paid) contract method with params as input values.
func (_Permit2 *Permit2TransactorRaw) Transact(opts *bind.TransactOpts, method string, params ...interface{}) (*types.Transaction, error) {
	return _Permit2.Contract.contract.Transact(opts, method, params...)
}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(address account) view returns(uint256 balance)
func (_Permit2 *Permit2Caller) BalanceOf(opts *bind.CallOpts, account common.Address) (*big.Int, error) {
	var out []interface{}
	err := _Permit2.contract.Call(opts, &out, "balanceOf", account)

	if err != nil {
		return *new(*big.Int), err
	}

	out0 := *abi.ConvertType(out[0], new(*big.Int)).(**big.Int)

	return out0, err

}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(address account) view returns(uint256 balance)
func (_Permit2 *Permit2Session) BalanceOf(account common.Address) (*big.Int, error) {
	return _Permit2.Contract.BalanceOf(&_Permit2.CallOpts, account)
}

// BalanceOf is a free data retrieval call binding the contract method 0x70a08231.
//
// Solidity: function balanceOf(address account) view returns(uint256 balance)
func (_Permit2 *Permit2CallerSession) BalanceOf(account common.Address) (*big.Int, error) {
	return _Permit2.Contract.BalanceOf(&_Permit2.CallOpts, account)
}

// Settle is a paid mutator transaction binding the contract method 0x13cd3b53.
//
// Solidity: function settle(((address,uint256),uint256,uint256) permit, address owner, (address,uint256) witness, bytes signature) returns()
func (_Permit2 *Permit2Transactor) Settle(opts *bind.TransactOpts, permit Struct1, owner common.Address, witness Struct0, signature []byte) (*types.Transaction, error) {
	return _Permit2.contract.Transact(opts, "settle", permit, owner, witness, signature)
}

// Settle is a paid mutator transaction binding the contract method 0x13cd3b53.
//
// Solidity: function settle(((address,uint256),uint256,uint256) permit, address owner, (address,uint256) witness, bytes signature) returns()
func (_Permit2 *Permit2Session) Settle(permit Struct1, owner common.Address, witness Struct0, signature []byte) (*types.Transaction, error) {
	return _Permit2.Contract.Settle(&_Permit2.TransactOpts, permit, owner, witness, signature)
}

// Settle is a paid mutator transaction binding the contract method 0x13cd3b53.
//
// Solidity: function settle(((address,uint256),uint256,uint256) permit, address owner, (address,uint256) witness, bytes signature) returns()
func (_Permit2 *Permit2TransactorSession) Settle(permit Struct1, owner common.Address, witness Struct0, signature []byte) (*types.Transaction, error) {
	return _Permit2.Contract.Settle(&_Permit2.TransactOpts, permit, owner, witness, signature)
}
