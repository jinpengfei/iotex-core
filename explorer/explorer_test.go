// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package explorer

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blockchain/action"
	"github.com/iotexproject/iotex-core/blockchain/trx"
	"github.com/iotexproject/iotex-core/config"
	ta "github.com/iotexproject/iotex-core/test/testaddress"
)

const (
	testingConfigPath = "../config.yaml"
)

func addTestingBlocks(bc blockchain.Blockchain) error {
	// Add block 1
	// test --> A, B, C, D, E, F
	payee := []*blockchain.Payee{}
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["alfa"].RawAddress, 20})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["bravo"].RawAddress, 30})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["charlie"].RawAddress, 50})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["delta"].RawAddress, 70})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["echo"].RawAddress, 110})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["foxtrot"].RawAddress, 50 << 20})
	transfers := []*action.Transfer{}
	transfers = append(transfers, action.NewTransfer(0, big.NewInt(1), ta.Addrinfo["miner"].RawAddress, ta.Addrinfo["charlie"].RawAddress))
	tx := bc.CreateTransaction(ta.Addrinfo["miner"], 280+(50<<20), payee)
	if tx == nil {
		return errors.New("empty tx for block 1")
	}
	blk, err := bc.MintNewBlock([]*trx.Tx{tx}, transfers, nil, ta.Addrinfo["miner"], "")
	if err != nil {
		return err
	}
	if err := bc.AddBlockCommit(blk); err != nil {
		return err
	}
	bc.ResetUTXO()

	// Add block 2
	// Charlie --> A, B, D, E, test
	payee = nil
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["alfa"].RawAddress, 1})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["bravo"].RawAddress, 1})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["charlie"].RawAddress, 1})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["delta"].RawAddress, 1})
	payee = append(payee, &blockchain.Payee{ta.Addrinfo["miner"].RawAddress, 1})
	tx = bc.CreateTransaction(ta.Addrinfo["charlie"], 5, payee)
	transfers = nil
	transfers = append(transfers, action.NewTransfer(0, big.NewInt(1), ta.Addrinfo["charlie"].RawAddress, ta.Addrinfo["alfa"].RawAddress))
	transfers = append(transfers, action.NewTransfer(0, big.NewInt(1), ta.Addrinfo["charlie"].RawAddress, ta.Addrinfo["bravo"].RawAddress))
	transfers = append(transfers, action.NewTransfer(0, big.NewInt(1), ta.Addrinfo["charlie"].RawAddress, ta.Addrinfo["delta"].RawAddress))
	transfers = append(transfers, action.NewTransfer(0, big.NewInt(1), ta.Addrinfo["charlie"].RawAddress, ta.Addrinfo["miner"].RawAddress))
	blk, err = bc.MintNewBlock([]*trx.Tx{tx}, transfers, nil, ta.Addrinfo["miner"], "")
	if err != nil {
		return err
	}
	if err := bc.AddBlockCommit(blk); err != nil {
		return err
	}
	bc.ResetUTXO()

	// Add block 3
	blk, err = bc.MintNewBlock(nil, nil, nil, ta.Addrinfo["miner"], "")
	if err != nil {
		return err
	}
	if err := bc.AddBlockCommit(blk); err != nil {
		return err
	}
	bc.ResetUTXO()

	// Add block 4
	blk, err = bc.MintNewBlock(nil, nil, nil, ta.Addrinfo["miner"], "")
	if err != nil {
		return err
	}
	if err := bc.AddBlockCommit(blk); err != nil {
		return err
	}
	bc.ResetUTXO()

	return nil
}

func TestExplorerApi(t *testing.T) {
	require := require.New(t)
	config, err := config.LoadConfigWithPathWithoutValidation(testingConfigPath)
	require.Nil(err)
	// disable account-based testing
	config.Chain.TrieDBPath = ""
	config.Chain.InMemTest = true
	// Disable block reward to make bookkeeping easier
	blockchain.Gen.BlockReward = uint64(0)

	// create chain
	bc := blockchain.CreateBlockchain(config, nil)
	require.NotNil(bc)
	height, err := bc.TipHeight()
	require.Nil(err)
	fmt.Printf("Open blockchain pass, height = %d\n", height)
	require.Nil(addTestingBlocks(bc))
	bc.Stop()

	svc := Service{
		bc: bc,
	}

	transfers, getErr := svc.GetTransfersByAddress(ta.Addrinfo["charlie"].RawAddress)
	require.Nil(getErr)
	require.Equal(len(transfers), 5)

	transfers, getErr = svc.GetLastTransfersByRange(4, 1, 3)
	require.Equal(len(transfers), 3)
	transfers, getErr = svc.GetLastTransfersByRange(4, 4, 5)
	require.Equal(len(transfers), 5)

	blks, getBlkErr := svc.GetLastBlocksByRange(3, 4)
	require.Nil(getBlkErr)
	require.Equal(len(blks), 4)

	transfers, getErr = svc.GetTransfersByBlockID(blks[2].ID)
	require.Nil(getErr)
	require.Equal(len(transfers), 2)

	transfer, getErr := svc.GetTransferByID(transfers[0].ID)
	require.Equal(transfer.Sender, transfers[0].Sender)
	require.Equal(transfer.Recipient, transfers[0].Recipient)

	blk, getErr := svc.GetBlockByID(blks[0].ID)
	require.Equal(blk.Height, blks[0].Height)
	require.Equal(blk.Timestamp, blks[0].Timestamp)

	stats, getStatsErr := svc.GetCoinStatistic()
	require.Nil(getStatsErr)
	require.Equal(stats.Supply, int64(10000000000))
	require.Equal(stats.Height, int64(4))

	balance, getBalanceErr := svc.GetAddressBalance(ta.Addrinfo["charlie"].RawAddress)
	require.Nil(getBalanceErr)
	require.Equal(balance, int64(46))

	addressDetails, getDetailErr := svc.GetAddressDetails(ta.Addrinfo["charlie"].RawAddress)
	require.Nil(getDetailErr)
	require.Equal(addressDetails.TotalBalance, int64(46))
	require.Equal(addressDetails.Address, ta.Addrinfo["charlie"].RawAddress)
}