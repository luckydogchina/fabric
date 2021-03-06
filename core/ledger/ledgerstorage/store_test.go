/*
Copyright IBM Corp. 2017 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ledgerstorage

import (
	"os"
	"testing"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/ledger/testutil"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/protos/ledger/rwset"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	flogging.SetModuleLevel("ledgerstorage", "debug")
	flogging.SetModuleLevel("pvtdatastorage", "debug")
	viper.Set("peer.fileSystemPath", "/tmp/fabric/core/ledger/ledgerstorage")
	os.Exit(m.Run())
}

func TestStore(t *testing.T) {
	testEnv := newTestEnv(t)
	defer testEnv.cleanup()
	provider := NewProvider()
	defer provider.Close()
	store, err := provider.Open("testLedger")
	defer store.Shutdown()

	assert.NoError(t, err)
	sampleData := sampleData(t)
	for _, sampleDatum := range sampleData {
		assert.NoError(t, store.CommitWithPvtData(sampleDatum))
	}

	// block 1 has no pvt data
	pvtdata, err := store.GetPvtDataByNum(1, nil)
	assert.NoError(t, err)
	assert.Nil(t, pvtdata)

	// block 4 has no pvt data
	pvtdata, err = store.GetPvtDataByNum(4, nil)
	assert.NoError(t, err)
	assert.Nil(t, pvtdata)

	// block 2 has pvt data for tx 3 and 5 only
	pvtdata, err = store.GetPvtDataByNum(2, nil)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(pvtdata))
	assert.Equal(t, uint64(3), pvtdata[0].SeqInBlock)
	assert.Equal(t, uint64(5), pvtdata[1].SeqInBlock)

	// block 3 has pvt data for tx 4 and 6 only
	pvtdata, err = store.GetPvtDataByNum(3, nil)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(pvtdata))
	assert.Equal(t, uint64(4), pvtdata[0].SeqInBlock)
	assert.Equal(t, uint64(6), pvtdata[1].SeqInBlock)

	blockAndPvtdata, err := store.GetPvtDataAndBlockByNum(2, nil)
	assert.NoError(t, err)
	assert.Equal(t, sampleData[2], blockAndPvtdata)

	blockAndPvtdata, err = store.GetPvtDataAndBlockByNum(3, nil)
	assert.NoError(t, err)
	assert.Equal(t, sampleData[3], blockAndPvtdata)

	// pvt data retrieval for block 3 with filter should return filtered pvtdata
	filter := ledger.NewPvtNsCollFilter()
	filter.Add("ns-1", "coll-1")
	blockAndPvtdata, err = store.GetPvtDataAndBlockByNum(3, filter)
	assert.NoError(t, err)
	assert.Equal(t, sampleData[3].Block, blockAndPvtdata.Block)
	// two transactions should be present
	assert.Equal(t, 2, len(blockAndPvtdata.BlockPvtData))
	// both tran number 4 and 6 should have only one collection because of filter
	assert.Equal(t, 1, len(blockAndPvtdata.BlockPvtData[4].WriteSet.NsPvtRwset))
	assert.Equal(t, 1, len(blockAndPvtdata.BlockPvtData[6].WriteSet.NsPvtRwset))
	// any other transaction entry should be nil
	assert.Nil(t, blockAndPvtdata.BlockPvtData[2])
}

func sampleData(t *testing.T) []*ledger.BlockAndPvtData {
	var blockAndpvtdata []*ledger.BlockAndPvtData
	blocks := testutil.ConstructTestBlocks(t, 10)
	for i := 0; i < 10; i++ {
		blockAndpvtdata = append(blockAndpvtdata, &ledger.BlockAndPvtData{Block: blocks[i]})
	}
	// txNum 3, 5 in block 2 has pvtdata
	blockAndpvtdata[2].BlockPvtData = samplePvtData(t, []uint64{3, 5})
	// txNum 4, 6 in block 3 has pvtdata
	blockAndpvtdata[3].BlockPvtData = samplePvtData(t, []uint64{4, 6})

	return blockAndpvtdata
}

func samplePvtData(t *testing.T, txNums []uint64) map[uint64]*ledger.TxPvtData {
	pvtWriteSet := &rwset.TxPvtReadWriteSet{DataModel: rwset.TxReadWriteSet_KV}
	pvtWriteSet.NsPvtRwset = []*rwset.NsPvtReadWriteSet{
		&rwset.NsPvtReadWriteSet{
			Namespace: "ns-1",
			CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
				&rwset.CollectionPvtReadWriteSet{
					CollectionName: "coll-1",
					Rwset:          []byte("RandomBytes-PvtRWSet-ns1-coll1"),
				},
				&rwset.CollectionPvtReadWriteSet{
					CollectionName: "coll-2",
					Rwset:          []byte("RandomBytes-PvtRWSet-ns1-coll2"),
				},
			},
		},
	}
	var pvtData []*ledger.TxPvtData
	for _, txNum := range txNums {
		pvtData = append(pvtData, &ledger.TxPvtData{SeqInBlock: txNum, WriteSet: pvtWriteSet})
	}
	return constructPvtdataMap(pvtData)
}
