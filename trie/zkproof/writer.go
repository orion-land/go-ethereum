package zkproof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/scroll-tech/go-ethereum/core/vm"
	"math/big"

	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/common/hexutil"
	"github.com/scroll-tech/go-ethereum/core/types"
	zkt "github.com/scroll-tech/go-ethereum/core/types/zktrie"
	"github.com/scroll-tech/go-ethereum/ethdb/memorydb"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/trie"
)

type proofList [][]byte

func (n *proofList) Put(key []byte, value []byte) error {
	*n = append(*n, value)
	return nil
}

func (n *proofList) Delete(key []byte) error {
	panic("not supported")
}

func addressToKey(addr common.Address) *zkt.Hash {
	var preImage zkt.Byte32
	copy(preImage[:], addr.Bytes())

	h, err := preImage.Hash()
	if err != nil {
		log.Error("hash failure", "preImage", hexutil.Encode(preImage[:]))
		return nil
	}
	return zkt.NewHashFromBigInt(h)
}

//resume the proof bytes into db and return the leaf node
func resumeProofs(proof []hexutil.Bytes, db *memorydb.Database) *trie.Node {
	for _, buf := range proof {

		n, err := trie.DecodeSMTProof(buf)
		if err != nil {
			log.Warn("decode proof string fail", "error", err)
		} else if n != nil {
			k, err := n.Key()
			if err != nil {
				log.Warn("node has no valid key", "error", err)
			} else {
				//notice: must consistent with trie/merkletree.go
				bt := k[:]
				db.Put(bt, buf)
				if n.Type == trie.NodeTypeLeaf || n.Type == trie.NodeTypeEmpty {
					return n
				}
			}
		}

	}

	return nil
}

// we have a trick here which suppose the proof array include all middle nodes along the
// whole path in sequence, from root to leaf
func decodeProofForMPTPath(proof proofList, path *SMTPath) {

	var lastNode *trie.Node
	keyPath := big.NewInt(0)
	path.KeyPathPart = (*hexutil.Big)(keyPath)

	keyCounter := big.NewInt(1)

	for _, buf := range proof {
		n, err := trie.DecodeSMTProof(buf)
		if err != nil {
			log.Warn("decode proof string fail", "error", err)
		} else if n != nil {
			k, err := n.Key()
			if err != nil {
				log.Warn("node has no valid key", "error", err)
				return
			}
			if lastNode == nil {
				//notice: use little-endian represent inside Hash ([:] or Bytes2())
				path.Root = k[:]
			} else {
				if bytes.Equal(k[:], lastNode.ChildL[:]) {
					path.Path = append(path.Path, SMTPathNode{
						Value:   k[:],
						Sibling: lastNode.ChildR[:],
					})
				} else if bytes.Equal(k[:], lastNode.ChildR[:]) {
					path.Path = append(path.Path, SMTPathNode{
						Value:   k[:],
						Sibling: lastNode.ChildL[:],
					})
					keyPath.Add(keyPath, keyCounter)
				} else {
					panic("Unexpected proof form")
				}
				keyCounter.Mul(keyCounter, big.NewInt(2))
			}
			switch n.Type {
			case trie.NodeTypeMiddle:
				lastNode = n
			case trie.NodeTypeLeaf:
				vhash, _ := n.ValueKey()
				path.Leaf = &SMTPathNode{
					//here we just return the inner represent of hash (little endian, reversed byte order to common hash)
					Value:   vhash[:],
					Sibling: n.NodeKey[:],
				}
				//sanity check
				keyPart := keyPath.Bytes()
				for i, b := range keyPart {
					ri := len(keyPart) - i
					cb := path.Leaf.Sibling[ri-1] //notice the output is little-endian
					if b&cb != b {
						panic(fmt.Errorf("path key not match: part is %x but key is %x", keyPart, []byte(path.Leaf.Sibling[:])))
					}
				}

				return
			case trie.NodeTypeEmpty:
				return
			default:
				panic(fmt.Errorf("unknown node type %d", n.Type))
			}
		}
	}

	panic("Unexpected finished here")
}

type zktrieProofWriter struct {
	db                  *trie.ZktrieDatabase
	tracingZktrie       *trie.ZkTrie
	tracingStorageTries map[common.Address]*trie.ZkTrie
	tracingAccounts     map[common.Address]*types.StateAccount
}

func NewZkTrieProofWriter(storage *types.StorageTrace) (*zktrieProofWriter, error) {

	underlayerDb := memorydb.New()
	zkDb := trie.NewZktrieDatabase(underlayerDb)

	accounts := make(map[common.Address]*types.StateAccount)

	// resuming proof bytes to underlayerDb
	for addrs, proof := range storage.Proofs {
		if n := resumeProofs(proof, underlayerDb); n != nil {
			addr := common.HexToAddress(addrs)
			if n.Type == trie.NodeTypeEmpty {
				accounts[addr] = nil
			} else if acc, err := types.UnmarshalStateAccount(n.Data()); err == nil {
				if bytes.Equal(n.NodeKey[:], addressToKey(addr)[:]) {
					accounts[addr] = acc
				} else {
					// should still mark the address as being trace (data not existed yet)
					accounts[addr] = nil
				}

			} else {
				return nil, fmt.Errorf("decode account bytes fail: %s, raw data [%x]", err, n.Data())
			}

		} else {
			return nil, fmt.Errorf("can not resume proof for address %s", addrs)
		}
	}

	storages := make(map[common.Address]*trie.ZkTrie)

	for addrs, stgLists := range storage.StorageProofs {

		addr := common.HexToAddress(addrs)
		accState, existed := accounts[addr]
		if !existed {
			// trace is malformed but currently we just warn about that
			log.Warn("no account state found for this addr, mal records", "address", addrs)
			continue
		} else if accState == nil {
			// create an empty zktrie for uninit address
			storages[addr], _ = trie.NewZkTrie(common.Hash{}, zkDb)
			continue
		}

		for keys, proof := range stgLists {

			if n := resumeProofs(proof, underlayerDb); n != nil {
				var err error
				storages[addr], err = trie.NewZkTrie(accState.Root, zkDb)
				if err != nil {
					return nil, fmt.Errorf("zktrie create failure for storage in addr <%s>: %s", err, addrs)
				}

			} else {
				return nil, fmt.Errorf("can not resume proof for storage %s@%s", keys, addrs)
			}

		}
	}

	zktrie, err := trie.NewZkTrie(
		storage.RootBefore,
		trie.NewZktrieDatabase(underlayerDb),
	)
	if err != nil {
		return nil, fmt.Errorf("zktrie create failure: %s", err)
	}

	// sanity check
	if !bytes.Equal(zktrie.Hash().Bytes(), storage.RootBefore.Bytes()) {
		return nil, fmt.Errorf("unmatch init trie hash: expected %x but has %x", storage.RootBefore.Bytes(), zktrie.Hash().Bytes())
	}

	return &zktrieProofWriter{
		db:                  zkDb,
		tracingZktrie:       zktrie,
		tracingAccounts:     accounts,
		tracingStorageTries: storages,
	}, nil
}

const (
	posSSTOREBefore = 0
	posCREATE       = 0
	posCREATEAfter  = 1
	posCALL         = 2
	posSTATICCALL   = 0
)

func getAccountState(l *types.StructLogRes, pos int) *types.AccountWrapper {
	if exData := l.ExtraData; exData == nil {
		return nil
	} else if len(exData.StateList) < pos {
		return nil
	} else {
		return exData.StateList[pos]
	}
}

func copyAccountState(st *types.AccountWrapper) *types.AccountWrapper {

	var stg *types.StorageWrapper
	if st.Storage != nil {
		stg = &types.StorageWrapper{
			Key:   st.Storage.Key,
			Value: st.Storage.Value,
		}
	}

	return &types.AccountWrapper{
		Nonce:    st.Nonce,
		Balance:  (*hexutil.Big)(big.NewInt(0).Set(st.Balance.ToInt())),
		CodeHash: st.CodeHash,
		Address:  st.Address,
		Storage:  stg,
	}
}

func getAccountDataFromLogState(state *types.AccountWrapper) *types.StateAccount {
	return &types.StateAccount{
		Nonce:    state.Nonce,
		Balance:  (*big.Int)(state.Balance),
		CodeHash: state.CodeHash.Bytes(),
	}
}

// for sanity check
func verifyAccount(addr common.Address, data *types.StateAccount, leaf *SMTPathNode) error {

	if leaf == nil {
		if data != nil {
			return fmt.Errorf("path has no corresponding leaf for account")
		} else {
			return nil
		}
	}

	addrKey := addressToKey(addr)
	if !bytes.Equal(addrKey[:], leaf.Sibling) {
		if data != nil {
			return fmt.Errorf("unmatch leaf node in address: %s", addr)
		}
	} else if data != nil {
		h, err := data.Hash()
		//log.Info("sanity check acc before", "addr", addr.String(), "key", leaf.Sibling.Text(16), "hash", h.Text(16))

		if err != nil {
			return fmt.Errorf("fail to hash account: %v", err)
		}
		if !bytes.Equal(zkt.NewHashFromBigInt(h)[:], leaf.Value) {
			return fmt.Errorf("unmatch data in leaf for address %s", addr)
		}
	}
	return nil
}

// for sanity check
func verifyStorage(key *zkt.Byte32, data *zkt.Byte32, leaf *SMTPathNode) error {

	emptyData := bytes.Equal(data[:], common.Hash{}.Bytes())

	if leaf == nil {
		if !emptyData {
			return fmt.Errorf("path has no corresponding leaf for storage")
		} else {
			return nil
		}
	}

	keyHash, err := key.Hash()
	if err != nil {
		return err
	}

	if !bytes.Equal(zkt.NewHashFromBigInt(keyHash)[:], leaf.Sibling) {
		if !emptyData {
			return fmt.Errorf("unmatch leaf node in storage: %x", key[:])
		}
	} else {
		h, err := data.Hash()
		//log.Info("sanity check acc before", "addr", addr.String(), "key", leaf.Sibling.Text(16), "hash", h.Text(16))

		if err != nil {
			return fmt.Errorf("fail to hash data: %v", err)
		}
		if !bytes.Equal(zkt.NewHashFromBigInt(h)[:], leaf.Value) {
			return fmt.Errorf("unmatch data in leaf for storage %x", key[:])
		}
	}
	return nil
}

// update traced account state, and return the corresponding trace object which
// is still opened for more infos
// the updated accData state is obtained by a closure which enable it being derived from current status
func (w *zktrieProofWriter) traceAccountUpdate(addr common.Address, updateAccData func(*types.StateAccount) *types.StateAccount) (*StorageTrace, error) {

	out := new(StorageTrace)
	//account trie
	out.Address = addr.Bytes()
	out.AccountPath = [2]*SMTPath{{}, {}}
	//fill dummy
	out.AccountUpdate = [2]*StateAccount{}

	accDataBefore, existed := w.tracingAccounts[addr]
	if !existed {
		//sanity check
		panic(fmt.Errorf("code do not add initialized status for account %s", addr))
	}

	var proof proofList
	if err := w.tracingZktrie.Prove(addr.Bytes32(), 0, &proof); err != nil {
		return nil, fmt.Errorf("prove BEFORE state for <%x> fail: %s", addr.Bytes(), err)
	}

	decodeProofForMPTPath(proof, out.AccountPath[0])
	if err := verifyAccount(addr, accDataBefore, out.AccountPath[0].Leaf); err != nil {
		panic(fmt.Errorf("code fail to trace account status correctly: %s", err))
	}
	if accDataBefore != nil {
		// we have ensured the nBefore has a key corresponding to the query one
		out.AccountKey = out.AccountPath[0].Leaf.Sibling
		out.AccountUpdate[0] = &StateAccount{
			Nonce:    int(accDataBefore.Nonce),
			Balance:  (*hexutil.Big)(big.NewInt(0).Set(accDataBefore.Balance)),
			CodeHash: accDataBefore.CodeHash,
		}
	}

	accData := updateAccData(accDataBefore)
	if accData != nil {
		out.AccountUpdate[1] = &StateAccount{
			Nonce:    int(accData.Nonce),
			Balance:  (*hexutil.Big)(big.NewInt(0).Set(accData.Balance)),
			CodeHash: accData.CodeHash,
		}
	}

	if accData != nil {
		if err := w.tracingZktrie.TryUpdateAccount(addr.Bytes32(), accData); err != nil {
			return nil, fmt.Errorf("update zktrie account state fail: %s", err)
		}
		w.tracingAccounts[addr] = accData
	} else {
		if err := w.tracingZktrie.TryDelete(addr.Bytes32()); err != nil {
			return nil, fmt.Errorf("delete zktrie account state fail: %s", err)
		}
		delete(w.tracingAccounts, addr)
	}

	proof = proofList{}
	if err := w.tracingZktrie.Prove(addr.Bytes32(), 0, &proof); err != nil {
		return nil, fmt.Errorf("prove AFTER state fail: %s", err)
	}

	decodeProofForMPTPath(proof, out.AccountPath[1])
	if err := verifyAccount(addr, accData, out.AccountPath[1].Leaf); err != nil {
		panic(fmt.Errorf("state AFTER has no valid account: %s", err))
	}
	if accData != nil {
		if out.AccountKey == nil {
			out.AccountKey = out.AccountPath[1].Leaf.Sibling[:]
		}
		//now accountKey must has been filled
	}

	return out, nil
}

// update traced storage state, and return the corresponding trace object
func (w *zktrieProofWriter) traceStorageUpdate(addr common.Address, key, value []byte) (*StorageTrace, error) {

	trie := w.tracingStorageTries[addr]
	if trie == nil {
		return nil, fmt.Errorf("no trace storage trie for %s", addr)
	}

	statePath := [2]*SMTPath{{}, {}}
	stateUpdate := [2]*StateStorage{}

	storeKey := zkt.NewByte32FromBytesPaddingZero(common.BytesToHash(key).Bytes())
	storeValueBefore := trie.Get(storeKey[:])
	storeValue := zkt.NewByte32FromBytes(value)

	if storeValueBefore != nil && !bytes.Equal(storeValueBefore[:], common.Hash{}.Bytes()) {
		stateUpdate[0] = &StateStorage{
			Key:   storeKey.Bytes(),
			Value: storeValueBefore,
		}
	}

	var storageBeforeProof, storageAfterProof proofList
	if err := trie.Prove(storeKey.Bytes(), 0, &storageBeforeProof); err != nil {
		return nil, fmt.Errorf("prove BEFORE storage state fail: %s", err)
	}

	decodeProofForMPTPath(storageBeforeProof, statePath[0])
	if err := verifyStorage(storeKey, zkt.NewByte32FromBytes(storeValueBefore), statePath[0].Leaf); err != nil {
		panic(fmt.Errorf("storage BEFORE has no valid data: %s (%v)", err, statePath[0]))
	}

	if !bytes.Equal(storeValue.Bytes(), common.Hash{}.Bytes()) {
		if err := trie.TryUpdate(storeKey.Bytes(), storeValue.Bytes()); err != nil {
			return nil, fmt.Errorf("update zktrie storage fail: %s", err)
		}
		stateUpdate[1] = &StateStorage{
			Key:   storeKey.Bytes(),
			Value: storeValue.Bytes(),
		}
	} else {
		if err := trie.TryDelete(storeKey.Bytes()); err != nil {
			return nil, fmt.Errorf("delete zktrie storage fail: %s", err)
		}
	}

	if err := trie.Prove(storeKey.Bytes(), 0, &storageAfterProof); err != nil {
		return nil, fmt.Errorf("prove AFTER storage state fail: %s", err)
	}
	decodeProofForMPTPath(storageAfterProof, statePath[1])
	if err := verifyStorage(storeKey, storeValue, statePath[1].Leaf); err != nil {
		panic(fmt.Errorf("storage AFTER has no valid data: %s (%v)", err, statePath[1]))
	}

	out, err := w.traceAccountUpdate(addr,
		func(acc *types.StateAccount) *types.StateAccount {
			if acc == nil {
				panic("unexpected")
			}

			//sanity check
			if accRootFromState := zkt.ReverseByteOrder(statePath[0].Root); !bytes.Equal(acc.Root[:], accRootFromState) {
				panic(fmt.Errorf("unexpected storage root before: [%s] vs [%x]", acc.Root, accRootFromState))
			}
			return &types.StateAccount{
				Nonce:    acc.Nonce,
				Balance:  acc.Balance,
				CodeHash: acc.CodeHash,
				Root:     common.BytesToHash(zkt.ReverseByteOrder(statePath[1].Root)),
			}
		})
	if err != nil {
		return nil, fmt.Errorf("update account %s in SSTORE fail: %s", addr, err)
	}

	if stateUpdate[1] != nil {
		out.StateKey = statePath[1].Leaf.Sibling
	} else if stateUpdate[0] != nil {
		out.StateKey = statePath[0].Leaf.Sibling
	} else {
		// it occurs when we are handling SLOAD with non-exist value
		// still no pretty idea, had to touch the internal behavior in zktrie ....
		if h, err := storeKey.Hash(); err != nil {
			return nil, fmt.Errorf("hash storekey fail: %s", err)
		} else {
			out.StateKey = zkt.NewHashFromBigInt(h)[:]
		}
	}

	out.StatePath = statePath
	out.StateUpdate = stateUpdate
	return out, nil
}

func (w *zktrieProofWriter) HandleNewState(accountState *types.AccountWrapper) (*StorageTrace, error) {

	if accountState.Storage != nil {
		storeAddr := hexutil.MustDecode(accountState.Storage.Key)
		storeValue := hexutil.MustDecode(accountState.Storage.Value)
		return w.traceStorageUpdate(accountState.Address, storeAddr, storeValue)
	} else {

		accData := getAccountDataFromLogState(accountState)

		out, err := w.traceAccountUpdate(accountState.Address, func(accBefore *types.StateAccount) *types.StateAccount {
			if accBefore != nil {
				accData.Root = accBefore.Root
			}
			return accData
		})
		if err != nil {
			return nil, fmt.Errorf("update account state %s fail: %s", accountState.Address, err)
		}
		hash, err := zkt.NewHashFromBytes(accData.Root[:])
		if err != nil {
			return nil, fmt.Errorf("malform of state root in account %s", accountState.Address)
		}
		out.CommonStateRoot = hash[:]
		return out, nil
	}

}

func handleLogs(od opOrderer, currentContract common.Address, logs []*types.StructLogRes) {
	logStack := []int{0}
	contractStack := map[int]common.Address{}
	skipDepth := 0
	callEnterAddress := currentContract

	// now trace every OP which could cause changes on state:
	for i, sLog := range logs {

		//trace log stack by depth rather than scanning specified op
		if sl := len(logStack); sl < sLog.Depth {
			logStack = append(logStack, i)
			//update currentContract according to previous op
			contractStack[sl] = currentContract
			currentContract = callEnterAddress
		} else if sl > sLog.Depth {
			logStack = logStack[:sl-1]
			currentContract = contractStack[sLog.Depth]
			resumePos := logStack[len(logStack)-1]
			calledLog := logs[resumePos]

			//no need to handle fail calling
			if !(calledLog.ExtraData != nil && calledLog.ExtraData.CallFailed) {
				//reentry the last log which "cause" the calling, some handling may needed
				switch vm.OpCode(calledLog.Op) {
				case vm.CREATE, vm.CREATE2:
					//addr, accDataBefore := getAccountDataFromProof(calledLog, posCALLBefore)
					od.absorb(getAccountState(calledLog, posCREATEAfter))
				}
			}

		} else {
			logStack[sl-1] = i
		}
		//sanity check
		if len(logStack) != sLog.Depth {
			panic("tracking log stack failure")
		}
		callEnterAddress = currentContract

		if skipDepth != 0 {
			if skipDepth < sLog.Depth {
				continue
			} else {
				skipDepth = 0
			}
		}

		if exD := sLog.ExtraData; exD != nil && exD.CallFailed {
			//mark current op and next ops with more depth skippable
			skipDepth = sLog.Depth
			continue
		}

		switch vm.OpCode(sLog.Op) {
		case vm.CREATE, vm.CREATE2:
			state := getAccountState(sLog, posCREATE)
			od.absorb(state)
			//update contract to CREATE addr

			callEnterAddress = state.Address
		case vm.CALL, vm.CALLCODE:
			state := getAccountState(sLog, posCALL)
			od.absorb(state)
			callEnterAddress = state.Address
		case vm.STATICCALL:
			//static call has no update on target address
			callEnterAddress = getAccountState(sLog, posSTATICCALL).Address
		case vm.SLOAD:
			accountState := getAccountState(sLog, posSSTOREBefore)
			od.absorb(accountState)
		case vm.SSTORE:
			log.Debug("build SSTORE", "pc", sLog.Pc, "key", sLog.Stack[len(sLog.Stack)-1])
			accountState := copyAccountState(getAccountState(sLog, posSSTOREBefore))
			// notice the log only provide the value BEFORE store and it is not suitable for our protocol,
			// here we change it into value AFTER update
			accountState.Storage = &types.StorageWrapper{
				Key:   sLog.Stack[len(sLog.Stack)-1],
				Value: sLog.Stack[len(sLog.Stack)-2],
			}
			od.absorb(accountState)

		default:
		}
	}
}

func handleTx(od opOrderer, txResult *types.ExecutionResult) {

	// handle failed tx
	if txResult.Failed {
		handled := false
		for _, state := range txResult.AccountsAfter {
			if state.Address != txResult.From.Address {
				continue
			}
			od.absorb(state)
			handled = true
		}
		if !handled {
			panic(fmt.Errorf("no caller account in postTx status"))
		}
		return
	}

	var toAddr common.Address

	if state := txResult.AccountCreated; state != nil {
		od.absorb(state)
		toAddr = state.Address
	} else {
		toAddr = txResult.To.Address
	}

	handleLogs(od, toAddr, txResult.StructLogs)

	for _, state := range txResult.AccountsAfter {
		od.absorb(state)
	}

}

const defaultOrdererScheme = MPTWitnessRWTbl

var usedOrdererScheme = defaultOrdererScheme

// HandleBlockResult only for backward compatibility
func HandleBlockResult(block *types.BlockResult) ([]*StorageTrace, error) {
	return HandleBlockResultEx(block, usedOrdererScheme)
}

func HandleBlockResultEx(block *types.BlockResult, ordererScheme MPTWitnessType) ([]*StorageTrace, error) {

	writer, err := NewZkTrieProofWriter(block.StorageTrace)
	if err != nil {
		return nil, err
	}

	var od opOrderer
	switch ordererScheme {
	case MPTWitnessNothing:
		panic("should not come here when scheme is 0")
	case MPTWitnessNatural:
		od = &simpleOrderer{}
	case MPTWitnessRWTbl:
		od = newRWTblOrderer(writer.tracingAccounts)
	default:
		return nil, fmt.Errorf("unrecognized scheme %d", ordererScheme)
	}

	for _, tx := range block.ExecutionResults {
		handleTx(od, tx)
	}

	// notice some coinbase addr (like all zero) is in fact not exist and should not be update
	// TODO: not a good solution, just for patch ...
	if coinbaseData := writer.tracingAccounts[block.BlockTrace.Coinbase.Address]; coinbaseData != nil {
		od.absorb(block.BlockTrace.Coinbase)
	}

	opDisp := od.end_absorb()
	var outTrace []*StorageTrace

	for op := opDisp.next(); op != nil; op = opDisp.next() {
		trace, err := writer.HandleNewState(op)
		if err != nil {
			return nil, err
		}
		outTrace = append(outTrace, trace)
	}

	finalHash := writer.tracingZktrie.Hash()
	if !bytes.Equal(finalHash.Bytes(), block.StorageTrace.RootAfter.Bytes()) {
		return outTrace, fmt.Errorf("unmatch hash: [%x] vs [%x]", finalHash.Bytes(), block.StorageTrace.RootAfter.Bytes())
	}

	return outTrace, nil

}

func FillBlockResultForMPTWitness(order MPTWitnessType, block *types.BlockResult) error {

	if order == MPTWitnessNothing {
		return nil
	}

	trace, err := HandleBlockResultEx(block, order)
	if err != nil {
		return err
	}

	msg, err := json.Marshal(trace)
	if err != nil {
		return err
	}

	rawmsg := json.RawMessage(msg)

	block.MPTWitness = &rawmsg
	return nil
}