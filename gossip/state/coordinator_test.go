/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package state

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/ledger/rwset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type committerMock struct {
	mock.Mock
}

func (mock *committerMock) Commit(block *common.Block) error {
	args := mock.Called(block)
	return args.Error(0)
}

func (mock *committerMock) LedgerHeight() (uint64, error) {
	args := mock.Called()
	return args.Get(0).(uint64), args.Error(1)
}

func (mock *committerMock) GetBlocks(blockSeqs []uint64) []*common.Block {
	args := mock.Called(blockSeqs)
	seqs := args.Get(0)
	if seqs == nil {
		return nil
	}
	return seqs.([]*common.Block)
}

func (mock *committerMock) Close() {
	mock.Called()
}

func TestPvtDataCollections_FailOnEmptyPayload(t *testing.T) {
	collection := &PvtDataCollections{
		&PvtData{
			Payload: &ledger.TxPvtData{
				SeqInBlock: uint64(1),
				WriteSet: &rwset.TxPvtReadWriteSet{
					DataModel: rwset.TxReadWriteSet_KV,
					NsPvtRwset: []*rwset.NsPvtReadWriteSet{
						{
							Namespace: "ns1",
							CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
								{
									CollectionName: "secretCollection",
									Rwset:          []byte{1, 2, 3, 4, 5, 6, 7},
								},
							},
						},
					},
				},
			},
		},

		&PvtData{
			Payload: nil,
		},
	}

	_, err := collection.Marshal()
	assertion := assert.New(t)
	assertion.Error(err, "Expected to fail since second item has nil payload")
	assertion.Equal("Mallformed private data payload, rwset index 1, payload is nil", fmt.Sprintf("%s", err))
}

func TestPvtDataCollections_FailMarshalingWriteSet(t *testing.T) {
	collection := &PvtDataCollections{
		&PvtData{
			Payload: &ledger.TxPvtData{
				SeqInBlock: uint64(1),
				WriteSet:   nil,
			},
		},
	}

	_, err := collection.Marshal()
	assertion := assert.New(t)
	assertion.Error(err, "Expected to fail since first item has nil writeset")
	assertion.Contains(fmt.Sprintf("%s", err), "Could not marshal private rwset index 0")
}

func TestPvtDataCollections_Marshal(t *testing.T) {
	collection := &PvtDataCollections{
		&PvtData{
			Payload: &ledger.TxPvtData{
				SeqInBlock: uint64(1),
				WriteSet: &rwset.TxPvtReadWriteSet{
					DataModel: rwset.TxReadWriteSet_KV,
					NsPvtRwset: []*rwset.NsPvtReadWriteSet{
						{
							Namespace: "ns1",
							CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
								{
									CollectionName: "secretCollection",
									Rwset:          []byte{1, 2, 3, 4, 5, 6, 7},
								},
							},
						},
					},
				},
			},
		},

		&PvtData{
			Payload: &ledger.TxPvtData{
				SeqInBlock: uint64(2),
				WriteSet: &rwset.TxPvtReadWriteSet{
					DataModel: rwset.TxReadWriteSet_KV,
					NsPvtRwset: []*rwset.NsPvtReadWriteSet{
						{
							Namespace: "ns1",
							CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
								{
									CollectionName: "secretCollection",
									Rwset:          []byte{42, 42, 42, 42, 42, 42, 42},
								},
							},
						},
						{
							Namespace: "ns2",
							CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
								{
									CollectionName: "otherCollection",
									Rwset:          []byte{10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
								},
							},
						},
					},
				},
			},
		},
	}

	bytes, err := collection.Marshal()

	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.NotNil(bytes)
	assertion.Equal(2, len(bytes))
}

func TestPvtDataCollections_Unmarshal(t *testing.T) {
	collection := PvtDataCollections{
		&PvtData{
			Payload: &ledger.TxPvtData{
				SeqInBlock: uint64(1),
				WriteSet: &rwset.TxPvtReadWriteSet{
					DataModel: rwset.TxReadWriteSet_KV,
					NsPvtRwset: []*rwset.NsPvtReadWriteSet{
						{
							Namespace: "ns1",
							CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
								{
									CollectionName: "secretCollection",
									Rwset:          []byte{1, 2, 3, 4, 5, 6, 7},
								},
							},
						},
					},
				},
			},
		},
	}

	bytes, err := collection.Marshal()

	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.NotNil(bytes)
	assertion.Equal(1, len(bytes))

	var newCol PvtDataCollections

	err = newCol.Unmarshal(bytes)
	assertion.NoError(err)
	assertion.Equal(newCol, collection)
}

func TestNewCoordinator(t *testing.T) {
	assertion := assert.New(t)

	committer := new(committerMock)

	block := &common.Block{
		Header: &common.BlockHeader{
			Number:       1,
			PreviousHash: []byte{0, 0, 0},
			DataHash:     []byte{1, 1, 1},
		},
		Data: &common.BlockData{
			Data: [][]byte{{1, 2, 3, 4, 5, 6}},
		},
	}

	blockToCommit := &common.Block{
		Header: &common.BlockHeader{
			Number:       2,
			PreviousHash: []byte{1, 1, 1},
			DataHash:     []byte{2, 2, 2},
		},
		Data: &common.BlockData{
			Data: [][]byte{{11, 12, 13, 14, 15, 16}},
		},
	}

	committer.On("GetBlocks", []uint64{1}).Return([]*common.Block{block})
	committer.On("GetBlocks", []uint64{2}).Return(nil)

	committer.On("LedgerHeight").Return(uint64(1), nil)
	committer.On("Commit", blockToCommit).Return(nil)

	coord := NewCoordinator(committer)

	b, err := coord.GetBlockByNum(1)

	assertion.NoError(err)
	assertion.Equal(block, b)

	b, err = coord.GetBlockByNum(2)

	assertion.Error(err)
	assertion.Nil(b)

	height, err := coord.LedgerHeight()
	assertion.NoError(err)
	assertion.Equal(uint64(1), height)

	missingPvtTx, err := coord.StoreBlock(blockToCommit)

	assertion.NoError(err)
	assertion.Empty(missingPvtTx)
}
