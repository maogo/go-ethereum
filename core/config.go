// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	hexlib "encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ethereumproject/go-ethereum/common"
	"github.com/ethereumproject/go-ethereum/core/state"
	"github.com/ethereumproject/go-ethereum/core/types"
	"github.com/ethereumproject/go-ethereum/core/vm"
	"github.com/ethereumproject/go-ethereum/ethdb"
	"github.com/ethereumproject/go-ethereum/logger"
	"github.com/ethereumproject/go-ethereum/logger/glog"
	"github.com/ethereumproject/go-ethereum/p2p/discover"
)

var (
	ChainConfigNotFoundErr     = errors.New("ChainConfig not found")
	ChainConfigForkNotFoundErr = errors.New("ChainConfig fork not found")

	ErrHashKnownBad  = errors.New("known bad hash")
	ErrHashKnownFork = validateError("known fork hash mismatch")
)

// #chainconfigi
// ChainConfig is the core config which determines the blockchain settings.
//
// ChainConfig is stored in the database on a per block basis. This means
// that any network, identified by its genesis block, can have its own
// set of configuration options.
type ChainConfig struct {
	// Forks holds fork block requirements. See ErrHashKnownFork.
	Forks Forks `json:"forks"`

	// BadHashes holds well known blocks with consensus issues. See ErrHashKnownBad.
	BadHashes []*BadHash `json:"bad_hashes"`

	ChainId *big.Int `json:"chain_id"`
}

type BadHash struct {
	Block *big.Int
	Hash  common.Hash
}

// IsHomestead returns whether num is either equal to the homestead block or greater.
func (c *ChainConfig) IsHomestead(num *big.Int) bool {
	if c.Fork("Homestead").Block == nil || num == nil {
		return false
	}
	return num.Cmp(c.Fork("Homestead").Block) >= 0
}

// IsETF returns whether num is equal to the bailout fork.
func (c *ChainConfig) IsETF(num *big.Int) bool {
	if c.Fork("ETF").Block == nil || num == nil {
		return false
	}
	return num.Cmp(c.Fork("ETF").Block) == 0
}

// IsDiehard returns whether num is greater than or equal to the Diehard block, but less than explosion.
func (c *ChainConfig) IsDiehard(num *big.Int) bool {
	fork := c.Fork("Diehard")
	if fork.Block == nil || num == nil {
		return false
	}
	return num.Cmp(fork.Block) >= 0
}

// IsExplosion returns whether num is either equal to the explosion block or greater.
func (c *ChainConfig) IsExplosion(num *big.Int) bool {
	opts, e := c.GetOptions(num)
	fork := c.GetForkForBlockNum(num)
	if e != nil {
		panic(e) // TODO handle better ?
	}
	if c.GetForkForBlockNum(num).Block == nil || opts.Length == nil || num == nil {
		return false
	}
	block := big.NewInt(0).Add(fork.Block, opts.Length)
	return num.Cmp(block) >= 0
}

// Fork looks up a Fork by its name, assumed to be unique
func (c *ChainConfig) Fork(name string) *Fork {
	for i := range c.Forks {
		if c.Forks[i].Name == name {
			return c.Forks[i]
		}
	}
	return &Fork{}
}

// LookupForkByBlockNum looks up a Fork by its block number, which is assumed to be unique.
// If not Fork is found, empty fork is returned.
func (c *ChainConfig) LookupForkByBlockNum(num *big.Int) *Fork {
	for i := range c.Forks {
		if c.Forks[i].Block.Cmp(num) == 0 {
			return c.Forks[i]
		}
	}
	return &Fork{}
}

// GetForkForBlockNum gets a the most-recent Fork corresponding to a given block number for
// a chain configuration.
func (c *ChainConfig) GetForkForBlockNum(num *big.Int) *Fork {
	sort.Sort(c.Forks)
	var okFork = &Fork{}
	for _, f := range c.Forks {
		if f.Block.Cmp(num) <= 0 {
			okFork = f
		}
	}
	return okFork
}

// GetForksThroughBlockNum get all forks up to but not exceeding a given block number
// for a calling ChainConfig
func (c *ChainConfig) GetForksThroughBlockNum(num *big.Int) Forks {
	var applicableForks Forks
	sort.Sort(c.Forks)
	for _, fork := range c.Forks {
		if fork.Block.Cmp(num) <= 0 {
			applicableForks = append(applicableForks, fork)
		}
	}
	return applicableForks
}

func (c *ChainConfig) HeaderCheck(h *types.Header) error {
	for _, fork := range c.Forks {
		if fork.Block.Cmp(h.Number) != 0 {
			continue
		}
		if !fork.RequiredHash.IsEmpty() && fork.RequiredHash != h.Hash() {
			return ErrHashKnownFork
		}
	}

	for _, bad := range c.BadHashes {
		if bad.Block.Cmp(h.Number) != 0 {
			continue
		}
		if bad.Hash == h.Hash() {
			return ErrHashKnownBad
		}
	}

	return nil
}

func (c *ChainConfig) GetSigner(blockNumber *big.Int) types.Signer {
	if c.IsDiehard(blockNumber) {
		return types.NewChainIdSigner(c.ChainId)
	}
	return types.BasicSigner{}
}

// GasTable returns the gas table corresponding to the current fork
// The returned GasTable's fields shouldn't, under any circumstances, be changed.
func (c *ChainConfig) GasTable(num *big.Int) *vm.GasTable {
	t := &vm.GasTable{
		ExtcodeSize:     big.NewInt(20),
		ExtcodeCopy:     big.NewInt(20),
		Balance:         big.NewInt(20),
		SLoad:           big.NewInt(50),
		Calls:           big.NewInt(40),
		Suicide:         big.NewInt(0),
		ExpByte:         big.NewInt(10),
		CreateBySuicide: nil,
	}

	opts, err := c.GetOptions(num)
	if err != nil {
		panic(err)
	}
	if opts.GasTable != nil {
		return opts.GasTable
	}
	return t
}

// ExternalChainConfig holds necessary data for externalizing a given blockchain configuration.
type ExternalChainConfig struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Genesis     *GenesisDump     `json:"genesis"`
	ChainConfig *ChainConfig     `json:"chainConfig"`
	Bootstrap   []*discover.Node `json:"bootstrap"`
}

// WriteToJSONFile writes a given config to a specified file path.
// It doesn't run any checks on the file path so make sure that's already squeaky clean.
func (c *ExternalChainConfig) WriteToJSONFile(path string) error {
	jsonConfig, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return fmt.Errorf("Could not marshal json from chain config: %v", err)
	}

	if err := ioutil.WriteFile(path, jsonConfig, 0644); err != nil {
		return fmt.Errorf("Could not write external chain config file: %v", err)
	}
	return nil
}

// ReadChainConfigFromJSONFile reads a given json file into a *ChainConfig.
// Again, no checks are made on the file path.
func ReadChainConfigFromJSONFile(path string) (*ExternalChainConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read external chain configuration file: %s", err)
	}
	defer f.Close()

	var config = &ExternalChainConfig{}
	if json.NewDecoder(f).Decode(config); err != nil {
		return nil, fmt.Errorf("%s: %s", path, err)
	}
	return config, nil
}

type Fork struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Block is the block number where the hard-fork commences on
	// the Ethereum network.
	Block *big.Int `json:"block"`
	// Used to improve sync for a known network split
	RequiredHash common.Hash `json:"requiredHash"`
	// Configurable features.
	Features []*ForkFeature `json:"features"`
}

// Forks implements sort interface, sorting by block number
type Forks []*Fork
func (fs Forks) Len() int { return len(fs) }
func (fs Forks) Less(i, j int) bool {
	iF := fs[i]
	jF := fs[j]
	return iF.Block.Cmp(jF.Block) < 0
}
func (fs Forks) Swap(i, j int) {
	fs[i], fs[j] = fs[j], fs[i]
}

// ForkFeatures are designed to decouple the implementation feature upgrades from Forks themselves.
// For example, there are several 'set-gasprice' features, each using a different gastable.
// In this case, the last-to-iterate (via `range`) ForkFeature with ID 'set-gasprice' in a given Fork will be used, but it's
// obviously best practice to only have one 'set-gasprice' ForkFeature per Fork anyway.
// Another example pertains to EIP/ECIP upgrades (so-called "hard-forks"). These are
// political or economic decisions made by or in the interest of the community, and impacting
// the implementation of the Ethereum Classic protocol. In these cases, given ID's will be more descriptive, ie
//  "homestead", "diehard", "eip155", "ecip1010", etc... ;-)
type ForkFeature struct {
	ID      string `json:"id"`
	Options ChainFeatureConfigOptions `json:"options"` // no * because they have to be iterable(?)
}

// These are the raw key-value configuration options made available
// by an external JSON file.
type ChainFeatureConfigOptions map[string]interface{}

// FeatureOptions establishes the current concrete possibilities for arbitrary key-value pairs in configuration
// options. These are options that are supported by the Ethereum protocol as it follows given Forks/+Features
// of a given blockchain configuration.
// See go-ethereum/core/data_features.go for exemplary defaults.
type FeatureOptions struct {
	GasTable     *vm.GasTable `json:"gasTable"` // Gas Price table
	Length       *big.Int     `json:"length"`   // Length of fork, if limited
	ChainID      *big.Int     `json:"chainId"`
	Difficulty   string       `json:"difficulty"` // id of eip/ecip difficulty algorithm
	// TODO Derive Oracle contracts from fork struct (Version, Registrar, Release)
}

// GetOptions gets relevant chain configuration options for a given block number num.
// The impact of this on the code is that queries to chain configuration will no longer
// be made with regard to Fork name, but with regard to Block number.
// GetOptions must parse arbitrary key-value pairs to available (ie non-unknown) fields, types, and values,
// and return an error (which must be handled "hard") if it handles an unknown/unparseable option
// key or value.
// Must get unique options up to, but not beyond block number, prioritizing most-recent options
// in cases of a single key being configured more than once for different Forks/+Features
func (c *ChainConfig) GetOptions(num *big.Int) (*FeatureOptions, error) {
	// find relevant fork
	forks := c.GetForksThroughBlockNum(num) // these will be sorted
	if forks.Len() == 0 {
		return &FeatureOptions{}, nil
	}

	return forks.decodeAndFlattenOptions()
}

// merge merges one "incoming" set of FeatureOptions *onto* another base,
// where the incoming feature option takes precedence.
// it's usefulness in this application depends on "incoming" being chronologically later than "base", ie to override with the new
func (base *FeatureOptions) merge(incoming *FeatureOptions) error {
	if incoming.GasTable != nil {
		base.GasTable = incoming.GasTable
	}
	if incoming.Length != nil {
		base.Length = incoming.Length
	}
	if incoming.ChainID != nil {
		base.ChainID = incoming.ChainID
	}
	if incoming.Difficulty != "" {
		base.Difficulty = incoming.Difficulty
	}
	// error me?
	return nil
}

// decodeAndFlattenOptions decode and aggregates all configured options on a Fork
// it is the iterative form of decodeOptions()
// it assume forks have been sorted chronologically (by block number), ie via GetForksThroughBlockNum
func (fs Forks) decodeAndFlattenOptions() (*FeatureOptions, error) {
	var decodedOpts = &FeatureOptions{}
	// parse
	for _, fork := range fs {
		if fork.Features != nil {
			// fork has n features
			for _, feat := range fork.Features {
				featOpts, e := feat.Options.decodeOptions()
				if e != nil {
					return nil, e
				}
				// merge newest options on top of old feature options
				if e := decodedOpts.merge(featOpts); e != nil {
					return nil, e
				}
			}
		}
	}
	return decodedOpts, nil
}

func mustStringToLowerAlphaOnly(s string) string {
	nonAlpha := regexp.MustCompile("[^a-zA-Z]")
	onlyAlpha := nonAlpha.ReplaceAllString(s, "") // replace non-alphas with ""
	return strings.ToLower(onlyAlpha)
}

// decodeOptions decodes arbitrary key-value data (JSON) to useable struct
// ForkFeature.Options -> FeatureOptions
func (f ChainFeatureConfigOptions) decodeOptions() (*FeatureOptions, error) {
	var opts = &FeatureOptions{}
	for key, val := range f {
		saneKey := mustStringToLowerAlphaOnly(key)
		if saneKey  == "gastable" {
			// regex.ReplaceAllLiteralString(src, repl string) string
			var gs = &vm.GasTable{}
			stringGasTableVal := val.(string) // type assertion, Go will panic if fail
			json.Unmarshal([]byte(stringGasTableVal), &gs)

			if !gs.IsEmpty() {
				opts.GasTable = gs
			} else {
				opts.GasTable = DefaultGasTableMap[stringGasTableVal]
			}

		} else if saneKey == "length" { 
			i, ok := new(big.Int).SetString(val.(string), 0)
			if !ok {
				return nil, fmt.Errorf("Error configuring chain length parameter: %v", val)
			}
			opts.Length = i

		} else if saneKey == "chainid" { 
			i, ok := new(big.Int).SetString(val.(string), 0)
			if !ok {
				return nil, fmt.Errorf("Error configuring chain id parameter: %v", val)
			}
			opts.ChainID = i

		} else if saneKey == "difficulty" {
			opts.Difficulty = val.(string)

		} else {
			return nil, fmt.Errorf("Chain configuration contained invalid parameter: key: %v, val: %v", key, val)
		}
	}
	return opts, nil
}

// WriteGenesisBlock writes the genesis block to the database as block number 0
func WriteGenesisBlock(chainDb ethdb.Database, genesis *GenesisDump) (*types.Block, error) {
	statedb, err := state.New(common.Hash{}, chainDb)
	if err != nil {
		return nil, err
	}

	for addrHex, account := range genesis.Alloc {
		var addr common.Address
		if err := addrHex.Decode(addr[:]); err != nil {
			return nil, fmt.Errorf("malformed addres %q: %s", addrHex, err)
		}

		balance, ok := new(big.Int).SetString(account.Balance, 0)
		if !ok {
			return nil, fmt.Errorf("malformed account %q balance %q", addrHex, account.Balance)
		}
		statedb.AddBalance(addr, balance)

		code, err := account.Code.Bytes()
		if err != nil {
			return nil, fmt.Errorf("malformed account %q code: %s", addrHex, err)
		}
		statedb.SetCode(addr, code)

		for key, value := range account.Storage {
			var k, v common.Hash
			if err := key.Decode(k[:]); err != nil {
				return nil, fmt.Errorf("malformed account %q key: %s", addrHex, err)
			}
			if err := value.Decode(v[:]); err != nil {
				return nil, fmt.Errorf("malformed account %q value: %s", addrHex, err)
			}
			statedb.SetState(addr, k, v)
		}
	}
	root, stateBatch := statedb.CommitBatch()

	header, err := genesis.Header()
	if err != nil {
		return nil, err
	}
	header.Root = root

	block := types.NewBlock(header, nil, nil, nil)

	if block := GetBlock(chainDb, block.Hash()); block != nil {
		glog.V(logger.Info).Infoln("Genesis block already in chain. Writing canonical number")
		err := WriteCanonicalHash(chainDb, block.Hash(), block.NumberU64())
		if err != nil {
			return nil, err
		}
		return block, nil
	}

	if err := stateBatch.Write(); err != nil {
		return nil, fmt.Errorf("cannot write state: %v", err)
	}
	if err := WriteTd(chainDb, block.Hash(), header.Difficulty); err != nil {
		return nil, err
	}
	if err := WriteBlock(chainDb, block); err != nil {
		return nil, err
	}
	if err := WriteBlockReceipts(chainDb, block.Hash(), nil); err != nil {
		return nil, err
	}
	if err := WriteCanonicalHash(chainDb, block.Hash(), block.NumberU64()); err != nil {
		return nil, err
	}
	if err := WriteHeadBlockHash(chainDb, block.Hash()); err != nil {
		return nil, err
	}

	return block, nil
}

type GenesisAccount struct {
	Address common.Address `json:"address"`
	Balance *big.Int       `json:"balance"`
}

func WriteGenesisBlockForTesting(db ethdb.Database, accounts ...GenesisAccount) *types.Block {
	dump := GenesisDump{
		GasLimit:   "0x47E7C4",
		Difficulty: "0x020000",
		Alloc:      make(map[hex]*GenesisDumpAlloc, len(accounts)),
	}

	for _, a := range accounts {
		dump.Alloc[hex(hexlib.EncodeToString(a.Address[:]))] = &GenesisDumpAlloc{
			Balance: a.Balance.String(),
		}
	}

	block, err := WriteGenesisBlock(db, &dump)
	if err != nil {
		panic(err)
	}
	return block
}

// GenesisDump is the geth JSON format.
// https://github.com/ethereumproject/wiki/wiki/Ethereum-Chain-Spec-Format#subformat-genesis
type GenesisDump struct {
	Nonce      prefixedHex `json:"nonce"`
	Timestamp  prefixedHex `json:"timestamp"`
	ParentHash prefixedHex `json:"parentHash"`
	ExtraData  prefixedHex `json:"extraData"`
	GasLimit   prefixedHex `json:"gasLimit"`
	Difficulty prefixedHex `json:"difficulty"`
	Mixhash    prefixedHex `json:"mixhash"`
	Coinbase   prefixedHex `json:"coinbase"`

	// Alloc maps accounts by their address.
	Alloc map[hex]*GenesisDumpAlloc `json:"alloc"`
}

// GenesisDumpAlloc is a GenesisDump.Alloc entry.
type GenesisDumpAlloc struct {
	Code    prefixedHex `json:"code"`
	Storage map[hex]hex `json:"storage"`
	Balance string      `json:"balance"` // decimal string
}

// MakeGenesisDump makes a genesis dump
func MakeGenesisDump(chaindb ethdb.Database) (*GenesisDump, error) {

	genesis := GetBlock(chaindb, GetCanonicalHash(chaindb, 0))
	if genesis == nil {
		return nil, nil
	}

	// Settings.
	genesisHeader := genesis.Header()
	nonce := fmt.Sprintf(`0x%x`, genesisHeader.Nonce)
	time := common.BigToHash(genesisHeader.Time).Hex()
	parentHash := genesisHeader.ParentHash.Hex()
	extra := common.ToHex(genesisHeader.Extra)
	gasLimit := common.BigToHash(genesisHeader.GasLimit).Hex()
	difficulty := common.BigToHash(genesisHeader.Difficulty).Hex()
	mixHash := genesisHeader.MixDigest.Hex()
	coinbase := genesisHeader.Coinbase.Hex()

	var dump = &GenesisDump{
		Nonce:      prefixedHex(nonce), // common.ToHex(n)), // common.ToHex(
		Timestamp:  prefixedHex(time),
		ParentHash: prefixedHex(parentHash),
		ExtraData:  prefixedHex(extra),
		GasLimit:   prefixedHex(gasLimit),
		Difficulty: prefixedHex(difficulty),
		Mixhash:    prefixedHex(mixHash),
		Coinbase:   prefixedHex(coinbase),
		//Alloc: ,
	}

	// State allocations.
	genState, err := state.New(genesis.Root(), chaindb)
	if err != nil {
		return nil, err
	}
	stateDump := genState.RawDump()

	stateAccounts := stateDump.Accounts
	dump.Alloc = make(map[hex]*GenesisDumpAlloc, len(stateAccounts))

	for address, acct := range stateAccounts {
		if common.IsHexAddress(address) {
			dump.Alloc[hex(address)] = &GenesisDumpAlloc{
				Balance: acct.Balance,
			}
		} else {
			return nil, fmt.Errorf("Invalid address in genesis state: %v", address)
		}
	}
	return dump, nil
}

// ReadGenesisFromJSONFile allows the use a genesis file in JSON format.
// Implemented in `init` command via initGenesis method.
func ReadGenesisFromJSONFile(jsonFilePath string) (dump *GenesisDump, err error) {
	f, err := os.Open(jsonFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read genesis file: %s", err)
	}
	defer f.Close()

	dump = new(GenesisDump)
	if json.NewDecoder(f).Decode(dump); err != nil {
		return nil, fmt.Errorf("%s: %s", jsonFilePath, err)
	}
	return dump, nil
}

// Header returns the mapping.
func (g *GenesisDump) Header() (*types.Header, error) {
	var h types.Header

	var err error
	if err = g.Nonce.Decode(h.Nonce[:]); err != nil {
		return nil, fmt.Errorf("malformed nonce: %s", err)
	}
	if h.Time, err = g.Timestamp.Int(); err != nil {
		return nil, fmt.Errorf("malformed timestamp: %s", err)
	}
	if err = g.ParentHash.Decode(h.ParentHash[:]); err != nil {
		return nil, fmt.Errorf("malformed parentHash: %s", err)
	}
	if h.Extra, err = g.ExtraData.Bytes(); err != nil {
		return nil, fmt.Errorf("malformed extraData: %s", err)
	}
	if h.GasLimit, err = g.GasLimit.Int(); err != nil {
		return nil, fmt.Errorf("malformed gasLimit: %s", err)
	}
	if h.Difficulty, err = g.Difficulty.Int(); err != nil {
		return nil, fmt.Errorf("malformed difficulty: %s", err)
	}
	if err = g.Mixhash.Decode(h.MixDigest[:]); err != nil {
		return nil, fmt.Errorf("malformed mixhash: %s", err)
	}
	if err := g.Coinbase.Decode(h.Coinbase[:]); err != nil {
		return nil, fmt.Errorf("malformed coinbase: %s", err)
	}

	return &h, nil
}

// hex is a hexadecimal string.
type hex string

// Decode fills buf when h is not empty.
func (h hex) Decode(buf []byte) error {
	if len(h) != 2*len(buf) {
		return fmt.Errorf("want %d hexadecimals", 2*len(buf))
	}

	_, err := hexlib.Decode(buf, []byte(h))
	return err
}

// prefixedHex is a hexadecimal string with an "0x" prefix.
type prefixedHex string

var errNoHexPrefix = errors.New("want 0x prefix")

// Decode fills buf when h is not empty.
func (h prefixedHex) Decode(buf []byte) error {
	i := len(h)
	if i == 0 {
		return nil
	}
	if i == 1 || h[0] != '0' || h[1] != 'x' {
		return errNoHexPrefix
	}
	if i == 2 {
		return nil
	}
	if i != 2*len(buf)+2 {
		return fmt.Errorf("want %d hexadecimals with 0x prefix", 2*len(buf))
	}

	_, err := hexlib.Decode(buf, []byte(h[2:]))
	return err
}

func (h prefixedHex) Bytes() ([]byte, error) {
	l := len(h)
	if l == 0 {
		return nil, nil
	}
	if l == 1 || h[0] != '0' || h[1] != 'x' {
		return nil, errNoHexPrefix
	}
	if l == 2 {
		return nil, nil
	}

	bytes := make([]byte, l/2-1)
	_, err := hexlib.Decode(bytes, []byte(h[2:]))
	return bytes, err
}

func (h prefixedHex) Int() (*big.Int, error) {
	bytes, err := h.Bytes()
	if err != nil {
		return nil, err
	}

	return new(big.Int).SetBytes(bytes), nil
}
