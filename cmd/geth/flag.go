// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ethereumproject/ethash"
	"github.com/ethereumproject/go-ethereum/accounts"
	"github.com/ethereumproject/go-ethereum/common"
	"github.com/ethereumproject/go-ethereum/core"
	"github.com/ethereumproject/go-ethereum/core/state"
	"github.com/ethereumproject/go-ethereum/core/types"
	"github.com/ethereumproject/go-ethereum/crypto"
	"github.com/ethereumproject/go-ethereum/eth"
	"github.com/ethereumproject/go-ethereum/ethdb"
	"github.com/ethereumproject/go-ethereum/event"
	"github.com/ethereumproject/go-ethereum/logger"
	"github.com/ethereumproject/go-ethereum/logger/glog"
	"github.com/ethereumproject/go-ethereum/miner"
	"github.com/ethereumproject/go-ethereum/node"
	"github.com/ethereumproject/go-ethereum/p2p/discover"
	"github.com/ethereumproject/go-ethereum/p2p/nat"
	"github.com/ethereumproject/go-ethereum/pow"
	"github.com/ethereumproject/go-ethereum/whisper"
	"gopkg.in/urfave/cli.v1"
)

func init() {
	cli.AppHelpTemplate = `{{.Name}} {{if .Flags}}[global options] {{end}}command{{if .Flags}} [command options]{{end}} [arguments...]

VERSION:
   {{.Version}}

COMMANDS:
   {{range .Commands}}{{.Name}}{{with .ShortName}}, {{.}}{{end}}{{ "\t" }}{{.Usage}}
   {{end}}{{if .Flags}}
GLOBAL OPTIONS:
   {{range .Flags}}{{.}}
   {{end}}{{end}}
`

	cli.CommandHelpTemplate = `{{.Name}}{{if .Subcommands}} command{{end}}{{if .Flags}} [command options]{{end}} [arguments...]
{{if .Description}}{{.Description}}
{{end}}{{if .Subcommands}}
SUBCOMMANDS:
	{{range .Subcommands}}{{.Name}}{{with .ShortName}}, {{.}}{{end}}{{ "\t" }}{{.Usage}}
	{{end}}{{end}}{{if .Flags}}
OPTIONS:
	{{range .Flags}}{{.}}
	{{end}}{{end}}
`
}

var currentChainID string
var reservedChainIDS = map[string]bool{
	"chaindata": true,
	"dapp":      true,
	"keystore":  true,
	"nodekey":   true,
	"nodes":     true,
	"geth":      true,
}

// getChainConfigIDFromContext gets the --chain=my-custom-net value.
// --testnet flag overrides --chain flag, thus return default testnet value.
// Disallowed words which conflict with in-use data files will log the problem and return "",
// which cause an error.
func getChainConfigIDFromContext(ctx *cli.Context) string {
	chainFlagIsSet := ctx.GlobalIsSet(TestNetFlag.Name) || ctx.GlobalIsSet(ChainIDFlag.Name)
	if chainFlagIsSet && currentChainID != "" {
		panic("Flags --chainconfig and --chain are conflicting. Please use only one.")
	}
	if currentChainID != "" {
		return currentChainID
	}
	if ctx.GlobalBool(TestNetFlag.Name) {
		return core.DefaultTestnetChainConfigID
	}
	if ctx.GlobalIsSet(ChainIDFlag.Name) {
		if id := ctx.GlobalString(ChainIDFlag.Name); id != "" {
			if reservedChainIDS[id] {
				log.Fatal("Reserved words for --chain flag include: 'chaindata', 'dapp', 'keystore', 'nodekey', 'nodes'. Please use a different identifier.")
				return ""
			}
			return id
		} else {
			log.Fatal("Argument to --chain must not be blank.")
			return ""
		}
	}
	return core.DefaultChainConfigID
}

// getChainConfigNameFromContext gets mainnet or testnet defaults if in use.
// If a custom net is in use, it echoes the name of the ChainConfigID.
// It is intended to be a human-readable name for a chain configuration.
func getChainConfigNameFromContext(ctx *cli.Context) string {
	if ctx.GlobalBool(TestNetFlag.Name) {
		return core.DefaultTestnetChainConfigName
	}
	if ctx.GlobalIsSet(ChainIDFlag.Name) {
		return getChainConfigIDFromContext(ctx) // shortcut
	}
	return core.DefaultChainConfigName
}

// mustMakeDataDir retrieves the currently requested data directory, terminating
// if none (or the empty string) is specified.
// --> <home>/<EthereumClassic>(defaulty) or --datadir
func mustMakeDataDir(ctx *cli.Context) string {
	if path := ctx.GlobalString(DataDirFlag.Name); path != "" {
		return path
	}
	log.Fatal("Cannot determine data directory, please set manually (--datadir)")
	return ""
}

// MustMakeChainDataDir retrieves the currently requested data directory including chain-specific subdirectory.
// A subdir of the datadir is used for each chain configuration ("/mainnet", "/testnet", "/my-custom-net").
// --> <home>/<EthereumClassic>/<mainnet|testnet|custom-net>, per --chain
func MustMakeChainDataDir(ctx *cli.Context) string {
	return common.EnsureAbsolutePath(mustMakeDataDir(ctx), getChainConfigIDFromContext(ctx))
}

// MakeIPCPath creates an IPC path configuration from the set command line flags,
// returning an empty string if IPC was explicitly disabled, or the set path.
func MakeIPCPath(ctx *cli.Context) string {
	if ctx.GlobalBool(IPCDisabledFlag.Name) {
		return ""
	}
	return ctx.GlobalString(IPCPathFlag.Name)
}

// MakeNodeKey creates a node key from set command line flags, either loading it
// from a file or as a specified hex value. If neither flags were provided, this
// method returns nil and an emphemeral key is to be generated.
func MakeNodeKey(ctx *cli.Context) *ecdsa.PrivateKey {
	var (
		hex  = ctx.GlobalString(NodeKeyHexFlag.Name)
		file = ctx.GlobalString(NodeKeyFileFlag.Name)

		key *ecdsa.PrivateKey
		err error
	)
	switch {
	case file != "" && hex != "":
		log.Fatalf("Options %q and %q are mutually exclusive", NodeKeyFileFlag.Name, NodeKeyHexFlag.Name)

	case file != "":
		if key, err = crypto.LoadECDSA(file); err != nil {
			log.Fatalf("Option %q: %v", NodeKeyFileFlag.Name, err)
		}

	case hex != "":
		if key, err = crypto.HexToECDSA(hex); err != nil {
			log.Fatalf("Option %q: %v", NodeKeyHexFlag.Name, err)
		}
	}
	return key
}

// MakeBootstrapNodesFromContext creates a list of bootstrap nodes from the command line
// flags, reverting to pre-configured ones if none have been specified.
func MakeBootstrapNodesFromContext(ctx *cli.Context) []*discover.Node {
	// Return pre-configured nodes if none were manually requested
	if !ctx.GlobalIsSet(BootnodesFlag.Name) {

		// --testnet flag overrides --config flag
		if ctx.GlobalBool(TestNetFlag.Name) {
			return TestNetBootNodes
		}
		return HomesteadBootNodes
	}
	return core.ParseBootstrapNodeStrings(strings.Split(ctx.GlobalString(BootnodesFlag.Name), ","))
}

// MakeListenAddress creates a TCP listening address string from set command
// line flags.
func MakeListenAddress(ctx *cli.Context) string {
	return fmt.Sprintf(":%d", ctx.GlobalInt(ListenPortFlag.Name))
}

// MakeNAT creates a port mapper from set command line flags.
func MakeNAT(ctx *cli.Context) nat.Interface {
	natif, err := nat.Parse(ctx.GlobalString(NATFlag.Name))
	if err != nil {
		log.Fatalf("Option %s: %v", NATFlag.Name, err)
	}
	return natif
}

// MakeRPCModules splits input separated by a comma and trims excessive white
// space from the substrings.
func MakeRPCModules(input string) []string {
	result := strings.Split(input, ",")
	for i, r := range result {
		result[i] = strings.TrimSpace(r)
	}
	return result
}

// MakeHTTPRpcHost creates the HTTP RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
func MakeHTTPRpcHost(ctx *cli.Context) string {
	if !ctx.GlobalBool(RPCEnabledFlag.Name) {
		return ""
	}
	return ctx.GlobalString(RPCListenAddrFlag.Name)
}

// MakeWSRpcHost creates the WebSocket RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
func MakeWSRpcHost(ctx *cli.Context) string {
	if !ctx.GlobalBool(WSEnabledFlag.Name) {
		return ""
	}
	return ctx.GlobalString(WSListenAddrFlag.Name)
}

// MakeDatabaseHandles raises out the number of allowed file handles per process
// for Geth and returns half of the allowance to assign to the database.
func MakeDatabaseHandles() int {
	if err := raiseFdLimit(2048); err != nil {
		log.Fatal("Failed to raise file descriptor allowance: ", err)
	}
	limit, err := getFdLimit()
	if err != nil {
		log.Fatal("Failed to retrieve file descriptor allowance: ", err)
	}
	if limit > 2048 { // cap database file descriptors even if more is available
		limit = 2048
	}
	return limit / 2 // Leave half for networking and other stuff
}

// MakeAccountManager creates an account manager from set command line flags.
func MakeAccountManager(ctx *cli.Context) *accounts.Manager {
	// Create the keystore crypto primitive, light if requested
	scryptN := accounts.StandardScryptN
	scryptP := accounts.StandardScryptP
	if ctx.GlobalBool(LightKDFFlag.Name) {
		scryptN = accounts.LightScryptN
		scryptP = accounts.LightScryptP
	}
	datadir := MustMakeChainDataDir(ctx)

	keydir := filepath.Join(datadir, "keystore")
	if path := ctx.GlobalString(KeyStoreDirFlag.Name); path != "" {
		keydir = path
	}

	m, err := accounts.NewManager(keydir, scryptN, scryptP)
	if err != nil {
		glog.Fatalf("init account manager at %q: %s", keydir, err)
	}
	return m
}

// MakeAddress converts an account specified directly as a hex encoded string or
// a key index in the key store to an internal account representation.
func MakeAddress(accman *accounts.Manager, account string) (accounts.Account, error) {
	// If the specified account is a valid address, return it
	if common.IsHexAddress(account) {
		return accounts.Account{Address: common.HexToAddress(account)}, nil
	}
	// Otherwise try to interpret the account as a keystore index
	index, err := strconv.Atoi(account)
	if err != nil {
		return accounts.Account{}, fmt.Errorf("invalid account address or index %q", account)
	}
	return accman.AccountByIndex(index)
}

// MakeEtherbase retrieves the etherbase either from the directly specified
// command line flags or from the keystore if CLI indexed.
func MakeEtherbase(accman *accounts.Manager, ctx *cli.Context) common.Address {
	accounts := accman.Accounts()
	if !ctx.GlobalIsSet(EtherbaseFlag.Name) && len(accounts) == 0 {
		glog.V(logger.Error).Infoln("WARNING: No etherbase set and no accounts found as default")
		return common.Address{}
	}
	etherbase := ctx.GlobalString(EtherbaseFlag.Name)
	if etherbase == "" {
		return common.Address{}
	}
	// If the specified etherbase is a valid address, return it
	account, err := MakeAddress(accman, etherbase)
	if err != nil {
		log.Fatalf("Option %q: %v", EtherbaseFlag.Name, err)
	}
	return account.Address
}

// MakePasswordList reads password lines from the file specified by --password.
func MakePasswordList(ctx *cli.Context) []string {
	path := ctx.GlobalString(PasswordFileFlag.Name)
	if path == "" {
		return nil
	}
	text, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal("Failed to read password file: ", err)
	}
	lines := strings.Split(string(text), "\n")
	// Sanitise DOS line endings.
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

// migrateExistingDirToClassicNamingScheme renames default base data directory ".../Ethereum" to ".../EthereumClassic", pending os customs, etc... ;-)
///
// Check for preexisting **Un-classic** data directory, ie "/home/path/to/Ethereum".
// If it exists, check if the data therein belongs to Classic blockchain (ie not configged as "ETF"),
// and rename it to fit Classic naming convention ("/home/path/to/EthereumClassic") if that dir doesn't already exist.
// This case only applies to Default, ie when a user **doesn't** provide a custom --datadir flag;
// a user should be able to override a specified data dir if they want.
func migrateExistingDirToClassicNamingScheme(ctx *cli.Context) error {

	ethDataDirPath := common.DefaultUnclassicDataDir()
	etcDataDirPath := common.DefaultDataDir()

	// only if default <EthereumClassic>/ datadir doesn't already exist
	if _, err := os.Stat(etcDataDirPath); err == nil {
		// classic data dir already exists
		log.Printf("INFO: Using existing ETClassic data directory at: %v.\n", etcDataDirPath)
		return nil
	}

	ethChainDBPath := filepath.Join(ethDataDirPath, "chaindata")
	if ctx.GlobalIsSet(TestNetFlag.Name) {
		ethChainDBPath = filepath.Join(ethDataDirPath, "testnet", "chaindata")
	}

	// only if ETHdatadir chaindb path DOES already exist, so return nil if it doesn't;
	// otherwise NewLDBDatabase will create an empty one there.
	// note that this uses the 'old' non-subdirectory way of holding default data.
	// it must be called before migrating to subdirectories
	// NOTE: Since ETH stores chaindata by default in Ethereum/geth/..., this path
	// will not exist if the existing data belongs to ETH, so it works as a valid check for us as well.
	if _, err := os.Stat(ethChainDBPath); os.IsNotExist(err) {
		log.Printf(`
		INFO: No existing default chaindata dir found at: %v \n
		  Using default data directory at: %v.\n`,
			ethChainDBPath, etcDataDirPath)
		return nil
	}

	// check if there is existing etf blockchain data in unclassic default dir (ie /<home>/Ethereum)
	chainDB, err := ethdb.NewLDBDatabase(ethChainDBPath, 0, 0)
	if err != nil {
		log.Fatalf(`
		WARNING: Error checking blockchain version for existing Ethereum chaindata database at: %v \n
		  Using default data directory at: %v`,
			err, etcDataDirPath)
		return nil
	}

	defer chainDB.Close()

	// Only move if defaulty ETC (mainnet or testnet).
	// Get head block if testnet, fork block if mainnet.
	b := core.GetBlock(chainDB, core.DefaultConfig.ForkByName("The DAO Hard Fork").RequiredHash)
	if ctx.GlobalIsSet(TestNetFlag.Name) {
		b = core.GetBlock(chainDB, core.GetHeadFastBlockHash(chainDB))
	}

	// Use default configuration to check if known fork, if block 1920000 exists.
	// If block1920000 doesn't exist, given above checks for directory structure expectations,
	// I think it's safe to assume that the chaindata directory is just too 'young', where it hasn't
	// synced until block 1920000, and therefore can be migrated.
	conf := core.DefaultConfig
	if ctx.GlobalIsSet(TestNetFlag.Name) {
		conf = core.TestConfig
	}

	if b == nil || (b != nil && conf.HeaderCheck(b.Header()) == nil) {
		log.Printf(`
		INFO/WARNING: Found existing default 'Ethereum' data directory with default ETC chaindata. \n
		  Migrating it from: %v, to: %v \n
		  To specify a different data directory use the '--datadir' flag.`,
			ethDataDirPath, etcDataDirPath)
		return os.Rename(ethDataDirPath, etcDataDirPath)
	}

	log.Printf(`INFO: Existing default Ethereum database at: %v isn't an Ethereum Classic default blockchain.\n
	  Will not migrate. \n
	  Using ETC chaindata database at: %v \n`,
		ethDataDirPath, etcDataDirPath)
	return nil
}

// migrateToChainSubdirIfNecessary migrates ".../EthereumClassic/nodes|chaindata|...|nodekey" --> ".../EthereumClassic/mainnet/nodes|chaindata|...|nodekey"
func migrateToChainSubdirIfNecessary(ctx *cli.Context) error {
	name := getChainConfigIDFromContext(ctx) // "mainnet", "morden", "custom"

	// return ok if custom
	//if ctx.GlobalIsSet(ChainIDFlag.Name) {
	//	return nil
	//}

	datapath := mustMakeDataDir(ctx) // ".../EthereumClassic/ | --datadir"
	if datapath == "" {
		return fmt.Errorf("Could not determine base data dir: %v", datapath)
	}

	subdirPath := MustMakeChainDataDir(ctx) // ie, <EthereumClassic>/mainnet

	// check if default subdir "mainnet" exits
	// NOTE: this assumes that if the migration has been run once, the "mainnet" dir will exist and will have necessary datum inside it
	subdirPathInfo, err := os.Stat(subdirPath)
	if err == nil {
		// dir already exists
		return nil
	}
	if subdirPathInfo != nil && !subdirPathInfo.IsDir() {
		return fmt.Errorf("Found file named '%v' in EthereumClassic datadir, which conflicts with default chain directory naming convention: %v", name, subdirPath)
	}

	// 3.3 testnet uses subdir '/testnet'
	if name == core.DefaultTestnetChainConfigID {
		exTestDir := filepath.Join(subdirPath, "../testnet")
		exTestDirInfo, e := os.Stat(exTestDir)
		if e != nil && os.IsNotExist(e) {
			return nil // ex testnet dir doesn't exist
		}
		if !exTestDirInfo.IsDir() {
			return nil // don't interfere with user *file* that won't be relevant for geth
		}
		return os.Rename(exTestDir, subdirPath)
	}

	// mkdir -p ".../mainnet"
	if err := os.MkdirAll(subdirPath, 0755); err != nil {
		return err
	}

	// move if existing (nodekey, dapp/, keystore/, chaindata/, nodes/) into new subdirectories
	for _, dir := range []string{"dapp", "keystore", "chaindata", "nodes"} {

		dirPath := filepath.Join(datapath, dir)

		dirInfo, e := os.Stat(dirPath)
		if e != nil && os.IsNotExist(e) {
			continue // dir doesn't exist
		}
		if !dirInfo.IsDir() {
			continue // don't interfere with user *file* that won't be relevant for geth
		}

		dirPathUnderSubdir := filepath.Join(subdirPath, dir)
		if err := os.Rename(dirPath, dirPathUnderSubdir); err != nil {
			return err
		}
	}

	// ensure nodekey exists and is file (loop lets us stay consistent in form here, an keep options open for easy other files to include)
	for _, file := range []string{"nodekey", "geth.ipc"} {
		filePath := filepath.Join(datapath, file)

		// ensure exists and is a file
		fileInfo, e := os.Stat(filePath)
		if e != nil && os.IsNotExist(e) {
			continue
		}
		if fileInfo.IsDir() {
			continue // don't interfere with user dirs that won't be relevant for geth
		}

		filePathUnderSubdir := filepath.Join(subdirPath, file)
		if err := os.Rename(filePath, filePathUnderSubdir); err != nil {
			return err
		}
	}
	return nil
}

// makeName makes the node name, which can be (in part) customized by the IdentityFlag
func makeNodeName(version string, ctx *cli.Context) string {
	name := fmt.Sprintf("Geth/%s/%s/%s", version, runtime.GOOS, runtime.Version())
	if identity := ctx.GlobalString(IdentityFlag.Name); len(identity) > 0 {
		name += "/" + identity
	}
	return name
}

// MakeSystemNode sets up a local node, configures the services to launch and
// assembles the P2P protocol stack.
func MakeSystemNode(version string, ctx *cli.Context) *node.Node {
	name := makeNodeName(version, ctx)

	// global settings

	if ctx.GlobalIsSet(ExtraDataFlag.Name) {
		s := ctx.GlobalString(ExtraDataFlag.Name)
		if len(s) > types.HeaderExtraMax {
			log.Fatalf("%s flag %q exceeds size limit of %d", ExtraDataFlag.Name, s, types.HeaderExtraMax)
		}
		miner.HeaderExtra = []byte(s)
	}

	// Makes sufficient configuration from JSON file or DB pending flags.
	// Delegates flag usage.
	config := mustMakeSufficientConfiguration(ctx)

	// Data migrations...

	// Rename existing default datadir <home>/<Ethereum>/ to <home>/<EthereumClassic>.
	// Only do this if --datadir flag is not specified AND <home>/<EthereumClassic> does NOT already exist (only migrate once and only for defaulty).
	// If it finds an 'Ethereum' directory, it will check if it contains default ETC or ETHF chain data.
	// If it contains ETC data, it will rename the dir. If ETHF data, if will do nothing.
	if !ctx.GlobalIsSet(DataDirFlag.Name) &&
		// Allows to use --chainconfig flag as long configuration specifies mainnet or morden
		(config.ID == core.DefaultTestnetChainConfigID || config.ID == core.DefaultChainConfigID) {
		if migrationError := migrateExistingDirToClassicNamingScheme(ctx); migrationError != nil {
			log.Fatalf("Failed to migrate existing Classic database: %v", migrationError)
		}
	}
	// Move existing mainnet data to pertinent chain-named subdir scheme (ie ethereum-classic/mainnet).
	// This should only happen if the given (newly defined in this protocol) subdir doesn't exist,
	// and the dirs&files (nodekey, dapp, keystore, chaindata, nodes) do exist,
	// and the user is using the Default configuration (ie "mainnet").
	if subdirMigrateErr := migrateToChainSubdirIfNecessary(ctx); subdirMigrateErr != nil {
		log.Fatalf("An error occurred migrating existing data to named subdir : %v", subdirMigrateErr)
	}

	// Avoid conflicting network flags
	networks, netFlags := 0, []cli.BoolFlag{DevModeFlag, TestNetFlag}
	for _, flag := range netFlags {
		if ctx.GlobalBool(flag.Name) {
			networks++
		}
	}
	if networks > 1 {
		log.Fatalf("The %v flags are mutually exclusive", netFlags)
	}

	// Configure the node's service container
	stackConf := &node.Config{
		DataDir:         MustMakeChainDataDir(ctx),
		PrivateKey:      MakeNodeKey(ctx),
		Name:            name,
		NoDiscovery:     ctx.GlobalBool(NoDiscoverFlag.Name),
		BootstrapNodes:  config.ParsedBootstrap,
		ListenAddr:      MakeListenAddress(ctx),
		NAT:             MakeNAT(ctx),
		MaxPeers:        ctx.GlobalInt(MaxPeersFlag.Name),
		MaxPendingPeers: ctx.GlobalInt(MaxPendingPeersFlag.Name),
		IPCPath:         MakeIPCPath(ctx),
		HTTPHost:        MakeHTTPRpcHost(ctx),
		HTTPPort:        ctx.GlobalInt(RPCPortFlag.Name),
		HTTPCors:        ctx.GlobalString(RPCCORSDomainFlag.Name),
		HTTPModules:     MakeRPCModules(ctx.GlobalString(RPCApiFlag.Name)),
		WSHost:          MakeWSRpcHost(ctx),
		WSPort:          ctx.GlobalInt(WSPortFlag.Name),
		WSOrigins:       ctx.GlobalString(WSAllowedOriginsFlag.Name),
		WSModules:       MakeRPCModules(ctx.GlobalString(WSApiFlag.Name)),
	}

	// Configure the Ethereum service
	accman := MakeAccountManager(ctx)

	ethConf := &eth.Config{
		ChainConfig:             config.ChainConfig,
		FastSync:                ctx.GlobalBool(FastSyncFlag.Name),
		BlockChainVersion:       ctx.GlobalInt(BlockchainVersionFlag.Name),
		DatabaseCache:           ctx.GlobalInt(CacheFlag.Name),
		DatabaseHandles:         MakeDatabaseHandles(),
		NetworkId:               ctx.GlobalInt(NetworkIdFlag.Name),
		AccountManager:          accman,
		Etherbase:               MakeEtherbase(accman, ctx),
		MinerThreads:            ctx.GlobalInt(MinerThreadsFlag.Name),
		NatSpec:                 ctx.GlobalBool(NatspecEnabledFlag.Name),
		DocRoot:                 ctx.GlobalString(DocRootFlag.Name),
		GasPrice:                new(big.Int),
		GpoMinGasPrice:          new(big.Int),
		GpoMaxGasPrice:          new(big.Int),
		GpoFullBlockRatio:       ctx.GlobalInt(GpoFullBlockRatioFlag.Name),
		GpobaseStepDown:         ctx.GlobalInt(GpobaseStepDownFlag.Name),
		GpobaseStepUp:           ctx.GlobalInt(GpobaseStepUpFlag.Name),
		GpobaseCorrectionFactor: ctx.GlobalInt(GpobaseCorrectionFactorFlag.Name),
		SolcPath:                ctx.GlobalString(SolcPathFlag.Name),
		AutoDAG:                 ctx.GlobalBool(AutoDAGFlag.Name) || ctx.GlobalBool(MiningEnabledFlag.Name),
	}

	if _, ok := ethConf.GasPrice.SetString(ctx.GlobalString(GasPriceFlag.Name), 0); !ok {
		log.Fatalf("malformed %s flag value %q", GasPriceFlag.Name, ctx.GlobalString(GasPriceFlag.Name))
	}
	if _, ok := ethConf.GpoMinGasPrice.SetString(ctx.GlobalString(GpoMinGasPriceFlag.Name), 0); !ok {
		log.Fatalf("malformed %s flag value %q", GpoMinGasPriceFlag.Name, ctx.GlobalString(GpoMinGasPriceFlag.Name))
	}
	if _, ok := ethConf.GpoMaxGasPrice.SetString(ctx.GlobalString(GpoMaxGasPriceFlag.Name), 0); !ok {
		log.Fatalf("malformed %s flag value %q", GpoMaxGasPriceFlag.Name, ctx.GlobalString(GpoMaxGasPriceFlag.Name))
	}

	// Configure the Whisper service
	shhEnable := ctx.GlobalBool(WhisperEnabledFlag.Name)

	// Override any default configs in dev mode or the test net
	switch {
	case ctx.GlobalBool(TestNetFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			ethConf.NetworkId = 2
		}
		ethConf.Genesis = core.TestNetGenesis
		state.StartingNonce = 1048576 // (2**20)

	case ctx.GlobalBool(DevModeFlag.Name):
		// Override the base network stack configs
		if !ctx.GlobalIsSet(DataDirFlag.Name) {
			stackConf.DataDir = filepath.Join(os.TempDir(), "/ethereum_dev_mode")
		}
		if !ctx.GlobalIsSet(MaxPeersFlag.Name) {
			stackConf.MaxPeers = 0
		}
		if !ctx.GlobalIsSet(ListenPortFlag.Name) {
			stackConf.ListenAddr = ":0"
		}
		// Override the Ethereum protocol configs
		ethConf.Genesis = core.TestNetGenesis
		if !ctx.GlobalIsSet(GasPriceFlag.Name) {
			ethConf.GasPrice = new(big.Int)
		}
		if !ctx.GlobalIsSet(WhisperEnabledFlag.Name) {
			shhEnable = true
		}
		ethConf.PowTest = true
	}
	// Assemble and return the protocol stack
	stack, err := node.New(stackConf)
	if err != nil {
		log.Fatal("Failed to create the protocol stack: ", err)
	}
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		return eth.New(ctx, ethConf)
	}); err != nil {
		log.Fatal("Failed to register the Ethereum service: ", err)
	}
	if shhEnable {
		if err := stack.Register(func(*node.ServiceContext) (node.Service, error) { return whisper.New(), nil }); err != nil {
			log.Fatal("Failed to register the Whisper service: ", err)
		}
	}

	if ctx.GlobalBool(Unused1.Name) {
		glog.V(logger.Info).Infoln(fmt.Sprintf("Geth started with --%s flag, which is unused by Geth Classic and can be omitted", Unused1.Name))
	}

	return stack
}

// MustMakeChainConfig reads the chain configuration from the database in ctx.Datadir.
func MustMakeChainConfig(ctx *cli.Context) *core.ChainConfig {

	db := MakeChainDatabase(ctx)
	defer db.Close()
	glog.V(logger.Info).Info(fmt.Sprintf("Did read chain configuration from database: \x1b[32m%s\x1b[39m", MustMakeChainDataDir(ctx)+"/chaindata"))

	return MustMakeChainConfigFromDb(ctx, db)
}

// mustMakeSufficientConfiguration makes a sufficent chain configuration (id, chainconfig, nodes,...) from
// either JSON file path, DB, or fails hard.
// Forces users to provide a full and complete config file if any is specified.
// Delegates flags to determine which source to use for configuration setup.
func mustMakeSufficientConfiguration(ctx *cli.Context) *core.SufficientChainConfig {

	config := &core.SufficientChainConfig{}

	// If external JSON --chainconfig specified.
	if ctx.GlobalIsSet(UseChainConfigFlag.Name) {
		glog.V(logger.Info).Info(fmt.Sprintf("Using custom chain configuration file: \x1b[32m%s\x1b[39m",
			filepath.Clean(ctx.GlobalString(UseChainConfigFlag.Name))))

		// Returns surely valid suff chain config.
		config, err := core.ReadExternalChainConfig(ctx.GlobalString(UseChainConfigFlag.Name))
		if err != nil {
			panic(err)
		}

		// Check for flagged bootnodes.
		if ctx.GlobalIsSet(BootnodesFlag.Name) {
			panic("Conflicting --chainconfig and --bootnodes flags. Please use either but not both.")
		}

		currentChainID = config.ID // Set global var.
		logChainConfiguration(ctx, config.ChainConfig)


		chainDB := MakeChainDatabase(ctx)
		block, err := core.WriteGenesisBlock(chainDB, config.Genesis)
		if err != nil {
			log.Fatalf("failed to write genesis block: %v", err)
		} else {
			log.Printf("successfully wrote genesis block and allocations: %x", block.Hash())
		}
		chainDB.Close()

		return config
	}

	// Initialise chain configuration before handling migrations or setting up node.

	config.ID = getChainConfigIDFromContext(ctx)
	config.Name = getChainConfigNameFromContext(ctx)
	config.ChainConfig = MustMakeChainConfig(ctx).SortForks()
	config.ParsedBootstrap = MakeBootstrapNodesFromContext(ctx)

	return config
}

func logChainConfiguration(ctx *cli.Context, config *core.ChainConfig) {
	separator := strings.Repeat("-", 110) // for display formatting

	glog.V(logger.Warn).Info(separator)
	glog.V(logger.Warn).Info(fmt.Sprintf("Starting Geth Classic \x1b[32m%s\x1b[39m", ctx.App.Version))

	glog.V(logger.Warn).Info(fmt.Sprintf("%v blockchain upgrades associated with this genesis block:", len(config.Forks)))

	for i := range config.Forks {
		glog.V(logger.Warn).Info(fmt.Sprintf(" %7v %v", config.Forks[i].Block, config.Forks[i].Name))
		if !config.Forks[i].RequiredHash.IsEmpty() {
			glog.V(logger.Warn).Info(fmt.Sprintf("         with block %v", config.Forks[i].RequiredHash.Hex()))
		}
	}

	if ctx.GlobalBool(TestNetFlag.Name) {
		glog.V(logger.Warn).Info("Geth is configured to use the \x1b[33mEthereum (ETC) Testnet\x1b[39m blockchain!")
	} else {
		glog.V(logger.Warn).Info("Geth is configured to use the \x1b[32mEthereum (ETC) Classic\x1b[39m blockchain!")
	}
	glog.V(logger.Warn).Info(separator)
}

// MustMakeChainConfigFromDb reads the chain configuration from the given database.
func MustMakeChainConfigFromDb(ctx *cli.Context, db ethdb.Database) *core.ChainConfig {

	c := core.DefaultConfig
	if ctx.GlobalBool(TestNetFlag.Name) {
		c = core.TestConfig
	}

	logChainConfiguration(ctx, c)

	genesis := core.GetBlock(db, core.GetCanonicalHash(db, 0))
	genesisHash := ""
	if genesis != nil {
		genesisHash = genesis.Hash().Hex()
	}
	if genesisHash != "" {
		glog.V(logger.Warn).Info(fmt.Sprintf("Loading blockchain: \x1b[36mgenesis\x1b[39m block \x1b[36m%s\x1b[39m.", genesisHash))
	}

	return c
}

// MakeChainDatabase open an LevelDB using the flags passed to the client and will hard crash if it fails.
func MakeChainDatabase(ctx *cli.Context) ethdb.Database {
	var (
		datadir = MustMakeChainDataDir(ctx)
		cache   = ctx.GlobalInt(CacheFlag.Name)
		handles = MakeDatabaseHandles()
	)

	chainDb, err := ethdb.NewLDBDatabase(filepath.Join(datadir, "chaindata"), cache, handles)
	if err != nil {
		log.Fatal("Could not open database: ", err)
	}
	return chainDb
}

// MakeChain creates a chain manager from set command line flags.
func MakeChain(ctx *cli.Context) (chain *core.BlockChain, chainDb ethdb.Database) {
	var err error
	chainDb = MakeChainDatabase(ctx)

	chainConfig := MustMakeChainConfigFromDb(ctx, chainDb)

	pow := pow.PoW(core.FakePow{})
	if !ctx.GlobalBool(FakePoWFlag.Name) {
		pow = ethash.New()
	}
	chain, err = core.NewBlockChain(chainDb, chainConfig, pow, new(event.TypeMux))
	if err != nil {
		log.Fatal("Could not start chainmanager: ", err)
	}
	return chain, chainDb
}

// MakeConsolePreloads retrieves the absolute paths for the console JavaScript
// scripts to preload before starting.
func MakeConsolePreloads(ctx *cli.Context) []string {
	// Skip preloading if there's nothing to preload
	if ctx.GlobalString(PreloadJSFlag.Name) == "" {
		return nil
	}
	// Otherwise resolve absolute paths and return them
	preloads := []string{}

	assets := ctx.GlobalString(JSpathFlag.Name)
	for _, file := range strings.Split(ctx.GlobalString(PreloadJSFlag.Name), ",") {
		preloads = append(preloads, common.EnsureAbsolutePath(assets, strings.TrimSpace(file)))
	}
	return preloads
}
