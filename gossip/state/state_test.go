/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package state

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	pb "github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/configtx/test"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/committer"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/ledgermgmt"
	"github.com/hyperledger/fabric/core/mocks/validator"
	"github.com/hyperledger/fabric/gossip/api"
	"github.com/hyperledger/fabric/gossip/comm"
	"github.com/hyperledger/fabric/gossip/common"
	"github.com/hyperledger/fabric/gossip/discovery"
	"github.com/hyperledger/fabric/gossip/gossip"
	"github.com/hyperledger/fabric/gossip/identity"
	"github.com/hyperledger/fabric/gossip/state/mocks"
	gutil "github.com/hyperledger/fabric/gossip/util"
	pcomm "github.com/hyperledger/fabric/protos/common"
	proto "github.com/hyperledger/fabric/protos/gossip"
	"github.com/hyperledger/fabric/protos/ledger/rwset"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var (
	portPrefix = 5610
)

var orgID = []byte("ORG1")

type peerIdentityAcceptor func(identity api.PeerIdentityType) error

var noopPeerIdentityAcceptor = func(identity api.PeerIdentityType) error {
	return nil
}

type joinChanMsg struct {
}

func init() {
	gutil.SetupTestLogging()
}

// SequenceNumber returns the sequence number of the block that the message
// is derived from
func (*joinChanMsg) SequenceNumber() uint64 {
	return uint64(time.Now().UnixNano())
}

// Members returns the organizations of the channel
func (jcm *joinChanMsg) Members() []api.OrgIdentityType {
	return []api.OrgIdentityType{orgID}
}

// AnchorPeersOf returns the anchor peers of the given organization
func (jcm *joinChanMsg) AnchorPeersOf(org api.OrgIdentityType) []api.AnchorPeer {
	return []api.AnchorPeer{}
}

type orgCryptoService struct {
}

// OrgByPeerIdentity returns the OrgIdentityType
// of a given peer identity
func (*orgCryptoService) OrgByPeerIdentity(identity api.PeerIdentityType) api.OrgIdentityType {
	return orgID
}

// Verify verifies a JoinChannelMessage, returns nil on success,
// and an error on failure
func (*orgCryptoService) Verify(joinChanMsg api.JoinChannelMessage) error {
	return nil
}

type cryptoServiceMock struct {
	acceptor peerIdentityAcceptor
}

// GetPKIidOfCert returns the PKI-ID of a peer's identity
func (*cryptoServiceMock) GetPKIidOfCert(peerIdentity api.PeerIdentityType) common.PKIidType {
	return common.PKIidType(peerIdentity)
}

// VerifyBlock returns nil if the block is properly signed,
// else returns error
func (*cryptoServiceMock) VerifyBlock(chainID common.ChainID, seqNum uint64, signedBlock []byte) error {
	return nil
}

// Sign signs msg with this peer's signing key and outputs
// the signature if no error occurred.
func (*cryptoServiceMock) Sign(msg []byte) ([]byte, error) {
	clone := make([]byte, len(msg))
	copy(clone, msg)
	return clone, nil
}

// Verify checks that signature is a valid signature of message under a peer's verification key.
// If the verification succeeded, Verify returns nil meaning no error occurred.
// If peerCert is nil, then the signature is verified against this peer's verification key.
func (*cryptoServiceMock) Verify(peerIdentity api.PeerIdentityType, signature, message []byte) error {
	equal := bytes.Equal(signature, message)
	if !equal {
		return fmt.Errorf("Wrong signature:%v, %v", signature, message)
	}
	return nil
}

// VerifyByChannel checks that signature is a valid signature of message
// under a peer's verification key, but also in the context of a specific channel.
// If the verification succeeded, Verify returns nil meaning no error occurred.
// If peerIdentity is nil, then the signature is verified against this peer's verification key.
func (cs *cryptoServiceMock) VerifyByChannel(chainID common.ChainID, peerIdentity api.PeerIdentityType, signature, message []byte) error {
	return cs.acceptor(peerIdentity)
}

func (*cryptoServiceMock) ValidateIdentity(peerIdentity api.PeerIdentityType) error {
	return nil
}

func bootPeers(ids ...int) []string {
	peers := []string{}
	for _, id := range ids {
		peers = append(peers, fmt.Sprintf("localhost:%d", id+portPrefix))
	}
	return peers
}

// Simple presentation of peer which includes only
// communication module, gossip and state transfer
type peerNode struct {
	port   int
	g      gossip.Gossip
	s      GossipStateProvider
	cs     *cryptoServiceMock
	commit committer.Committer
}

// Shutting down all modules used
func (node *peerNode) shutdown() {
	node.s.Stop()
	node.g.Stop()
}

type mockCommitter struct {
	mock.Mock
	sync.Mutex
}

func (mc *mockCommitter) Commit(block *pcomm.Block) error {
	mc.Called(block)
	return nil
}

func (mc *mockCommitter) LedgerHeight() (uint64, error) {
	mc.Lock()
	defer mc.Unlock()
	if mc.Called().Get(1) == nil {
		return mc.Called().Get(0).(uint64), nil
	}
	return mc.Called().Get(0).(uint64), mc.Called().Get(1).(error)
}

func (mc *mockCommitter) GetBlocks(blockSeqs []uint64) []*pcomm.Block {
	if mc.Called(blockSeqs).Get(0) == nil {
		return nil
	}
	return mc.Called(blockSeqs).Get(0).([]*pcomm.Block)
}

func (*mockCommitter) Close() {
}

// Default configuration to be used for gossip and communication modules
func newGossipConfig(id int, boot ...int) *gossip.Config {
	port := id + portPrefix
	return &gossip.Config{
		BindPort:                   port,
		BootstrapPeers:             bootPeers(boot...),
		ID:                         fmt.Sprintf("p%d", id),
		MaxBlockCountToStore:       0,
		MaxPropagationBurstLatency: time.Duration(10) * time.Millisecond,
		MaxPropagationBurstSize:    10,
		PropagateIterations:        1,
		PropagatePeerNum:           3,
		PullInterval:               time.Duration(4) * time.Second,
		PullPeerNum:                5,
		InternalEndpoint:           fmt.Sprintf("localhost:%d", port),
		PublishCertPeriod:          10 * time.Second,
		RequestStateInfoInterval:   4 * time.Second,
		PublishStateInfoInterval:   4 * time.Second,
	}
}

// Create gossip instance
func newGossipInstance(config *gossip.Config, mcs api.MessageCryptoService) gossip.Gossip {
	id := api.PeerIdentityType(config.InternalEndpoint)
	idMapper := identity.NewIdentityMapper(mcs, id)
	return gossip.NewGossipServiceWithServer(config, &orgCryptoService{}, mcs,
		idMapper, id, nil)
}

// Create new instance of KVLedger to be used for testing
func newCommitter(id int) committer.Committer {
	cb, _ := test.MakeGenesisBlock(strconv.Itoa(id))
	ledger, _ := ledgermgmt.CreateLedger(cb)
	return committer.NewLedgerCommitter(ledger, &validator.MockValidator{})
}

// Constructing pseudo peer node, simulating only gossip and state transfer part
func newPeerNodeWithGossip(config *gossip.Config, committer committer.Committer, acceptor peerIdentityAcceptor, g gossip.Gossip) *peerNode {
	cs := &cryptoServiceMock{acceptor: acceptor}
	// Gossip component based on configuration provided and communication module
	if g == nil {
		g = newGossipInstance(config, &cryptoServiceMock{acceptor: noopPeerIdentityAcceptor})
	}

	logger.Debug("Joinning channel", util.GetTestChainID())
	g.JoinChan(&joinChanMsg{}, common.ChainID(util.GetTestChainID()))

	// Initialize pseudo peer simulator, which has only three
	// basic parts

	servicesAdapater := &ServicesMediator{GossipAdapter: g, MCSAdapter: cs}
	sp := NewGossipStateProvider(util.GetTestChainID(), servicesAdapater, committer)
	if sp == nil {
		return nil
	}

	return &peerNode{
		port:   config.BindPort,
		g:      g,
		s:      sp,
		commit: committer,
		cs:     cs,
	}
}

// Constructing pseudo peer node, simulating only gossip and state transfer part
func newPeerNode(config *gossip.Config, committer committer.Committer, acceptor peerIdentityAcceptor) *peerNode {
	return newPeerNodeWithGossip(config, committer, acceptor, nil)
}

func TestNilDirectMsg(t *testing.T) {
	mc := &mockCommitter{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	g := &mocks.GossipMock{}
	g.On("Accept", mock.Anything, false).Return(make(<-chan *proto.GossipMessage), nil)
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	p := newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	defer p.shutdown()
	p.s.(*GossipStateProviderImpl).handleStateRequest(nil)
	p.s.(*GossipStateProviderImpl).directMessage(nil)
	sMsg, _ := p.s.(*GossipStateProviderImpl).stateRequestMessage(uint64(10), uint64(8)).NoopSign()
	req := &comm.ReceivedMessageImpl{
		SignedGossipMessage: sMsg,
	}
	p.s.(*GossipStateProviderImpl).directMessage(req)
}

func TestNilAddPayload(t *testing.T) {
	mc := &mockCommitter{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	g := &mocks.GossipMock{}
	g.On("Accept", mock.Anything, false).Return(make(<-chan *proto.GossipMessage), nil)
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	p := newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	defer p.shutdown()
	err := p.s.AddPayload(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestAddPayloadLedgerUnavailable(t *testing.T) {
	mc := &mockCommitter{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	g := &mocks.GossipMock{}
	g.On("Accept", mock.Anything, false).Return(make(<-chan *proto.GossipMessage), nil)
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	p := newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	defer p.shutdown()
	// Simulate a problem in the ledger
	failedLedger := mock.Mock{}
	failedLedger.On("LedgerHeight", mock.Anything).Return(uint64(0), errors.New("cannot query ledger"))
	mc.Lock()
	mc.Mock = failedLedger
	mc.Unlock()

	rawblock := pcomm.NewBlock(uint64(1), []byte{})
	b, _ := pb.Marshal(rawblock)
	err := p.s.AddPayload(&proto.Payload{
		SeqNum: uint64(1),
		Data:   b,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Failed obtaining ledger height")
	assert.Contains(t, err.Error(), "cannot query ledger")
}

func TestOverPopulation(t *testing.T) {
	// Scenario: Add to the state provider blocks
	// with a gap in between, and ensure that the payload buffer
	// rejects blocks starting if the distance between the ledger height to the latest
	// block it contains is bigger than defMaxBlockDistance.

	mc := &mockCommitter{}
	blocksPassedToLedger := make(chan uint64, 10)
	mc.On("Commit", mock.Anything).Run(func(arg mock.Arguments) {
		blocksPassedToLedger <- arg.Get(0).(*pcomm.Block).Header.Number
	})
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	g := &mocks.GossipMock{}
	g.On("Accept", mock.Anything, false).Return(make(<-chan *proto.GossipMessage), nil)
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	p := newPeerNode(newGossipConfig(0), mc, noopPeerIdentityAcceptor)
	defer p.shutdown()

	// Add some blocks in a sequential manner and make sure it works
	for i := 1; i <= 4; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		b, _ := pb.Marshal(rawblock)
		assert.NoError(t, p.s.AddPayload(&proto.Payload{
			SeqNum: uint64(i),
			Data:   b,
		}))
	}

	// Add payloads from 10 to defMaxBlockDistance, while we're missing blocks [5,9]
	// Should succeed
	for i := 10; i <= defMaxBlockDistance; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		b, _ := pb.Marshal(rawblock)
		assert.NoError(t, p.s.AddPayload(&proto.Payload{
			SeqNum: uint64(i),
			Data:   b,
		}))
	}

	// Add payloads from defMaxBlockDistance + 2 to defMaxBlockDistance * 10
	// Should fail.
	for i := defMaxBlockDistance + 1; i <= defMaxBlockDistance*10; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		b, _ := pb.Marshal(rawblock)
		assert.Error(t, p.s.AddPayload(&proto.Payload{
			SeqNum: uint64(i),
			Data:   b,
		}))
	}

	// Ensure only blocks 1-4 were passed to the ledger
	close(blocksPassedToLedger)
	i := 1
	for seq := range blocksPassedToLedger {
		assert.Equal(t, uint64(i), seq)
		i++
	}
	assert.Equal(t, 5, i)

	// Ensure we don't store too many blocks in memory
	sp := p.s.(*GossipStateProviderImpl)
	assert.True(t, sp.payloads.Size() < defMaxBlockDistance)

}

func TestFailures(t *testing.T) {
	mc := &mockCommitter{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(0), nil)
	g := &mocks.GossipMock{}
	g.On("Accept", mock.Anything, false).Return(make(<-chan *proto.GossipMessage), nil)
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	g.On("PeersOfChannel", mock.Anything).Return([]discovery.NetworkMember{})
	assert.Panics(t, func() {
		newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	})
	// Reprogram mock
	mc.Mock = mock.Mock{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), errors.New("Failed accessing ledger"))
	assert.Nil(t, newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g))
	// Reprogram mock
	mc.Mock = mock.Mock{}
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	mc.On("GetBlocks", mock.Anything).Return(nil)
	p := newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	assert.Nil(t, p.s.GetBlock(uint64(1)))
}

func TestGossipReception(t *testing.T) {
	signalChan := make(chan struct{})
	rawblock := &pcomm.Block{
		Header: &pcomm.BlockHeader{
			Number: uint64(1),
		},
		Data: &pcomm.BlockData{
			Data: [][]byte{},
		},
	}
	b, _ := pb.Marshal(rawblock)

	createChan := func(signalChan chan struct{}) <-chan *proto.GossipMessage {
		c := make(chan *proto.GossipMessage)
		gMsg := &proto.GossipMessage{
			Channel: []byte("AAA"),
			Content: &proto.GossipMessage_DataMsg{
				DataMsg: &proto.DataMessage{
					Payload: &proto.Payload{
						SeqNum: 1,
						Data:   b,
					},
				},
			},
		}
		go func(c chan *proto.GossipMessage) {
			// Wait for Accept() to be called
			<-signalChan
			// Simulate a message reception from the gossip component with an invalid channel
			c <- gMsg
			gMsg.Channel = []byte(util.GetTestChainID())
			// Simulate a message reception from the gossip component
			c <- gMsg
		}(c)
		return c
	}

	g := &mocks.GossipMock{}
	rmc := createChan(signalChan)
	g.On("Accept", mock.Anything, false).Return(rmc, nil).Run(func(_ mock.Arguments) {
		signalChan <- struct{}{}
	})
	g.On("Accept", mock.Anything, true).Return(nil, make(<-chan proto.ReceivedMessage))
	g.On("PeersOfChannel", mock.Anything).Return([]discovery.NetworkMember{})
	mc := &mockCommitter{}
	receivedChan := make(chan struct{})
	mc.On("Commit", mock.Anything).Run(func(arguments mock.Arguments) {
		block := arguments.Get(0).(*pcomm.Block)
		assert.Equal(t, uint64(1), block.Header.Number)
		receivedChan <- struct{}{}
	})
	mc.On("LedgerHeight", mock.Anything).Return(uint64(1), nil)
	p := newPeerNodeWithGossip(newGossipConfig(0), mc, noopPeerIdentityAcceptor, g)
	defer p.shutdown()
	select {
	case <-receivedChan:
	case <-time.After(time.Second * 15):
		assert.Fail(t, "Didn't commit a block within a timely manner")
	}
}

func TestAccessControl(t *testing.T) {
	viper.Set("peer.fileSystemPath", "/tmp/tests/ledger/node")
	ledgermgmt.InitializeTestEnv()
	defer ledgermgmt.CleanupTestEnv()

	bootstrapSetSize := 5
	bootstrapSet := make([]*peerNode, 0)

	authorizedPeers := map[string]struct{}{
		"localhost:5610": {},
		"localhost:5615": {},
		"localhost:5618": {},
		"localhost:5621": {},
	}

	blockPullPolicy := func(identity api.PeerIdentityType) error {
		if _, isAuthorized := authorizedPeers[string(identity)]; isAuthorized {
			return nil
		}
		return errors.New("Not authorized")
	}

	for i := 0; i < bootstrapSetSize; i++ {
		commit := newCommitter(i)
		bootstrapSet = append(bootstrapSet, newPeerNode(newGossipConfig(i), commit, blockPullPolicy))
	}

	defer func() {
		for _, p := range bootstrapSet {
			p.shutdown()
		}
	}()

	msgCount := 5

	for i := 1; i <= msgCount; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		if b, err := pb.Marshal(rawblock); err == nil {
			payload := &proto.Payload{
				SeqNum: uint64(i),
				Data:   b,
			}
			bootstrapSet[0].s.AddPayload(payload)
		} else {
			t.Fail()
		}
	}

	standardPeerSetSize := 10
	peersSet := make([]*peerNode, 0)

	for i := 0; i < standardPeerSetSize; i++ {
		commit := newCommitter(bootstrapSetSize + i)
		peersSet = append(peersSet, newPeerNode(newGossipConfig(bootstrapSetSize+i, 0, 1, 2, 3, 4), commit, blockPullPolicy))
	}

	defer func() {
		for _, p := range peersSet {
			p.shutdown()
		}
	}()

	waitUntilTrueOrTimeout(t, func() bool {
		for _, p := range peersSet {
			if len(p.g.PeersOfChannel(common.ChainID(util.GetTestChainID()))) != bootstrapSetSize+standardPeerSetSize-1 {
				logger.Debug("Peer discovery has not finished yet")
				return false
			}
		}
		logger.Debug("All peer discovered each other!!!")
		return true
	}, 30*time.Second)

	logger.Debug("Waiting for all blocks to arrive.")
	waitUntilTrueOrTimeout(t, func() bool {
		logger.Debug("Trying to see all authorized peers get all blocks, and all non-authorized didn't")
		for _, p := range peersSet {
			height, err := p.commit.LedgerHeight()
			id := fmt.Sprintf("localhost:%d", p.port)
			if _, isAuthorized := authorizedPeers[id]; isAuthorized {
				if height != uint64(msgCount+1) || err != nil {
					return false
				}
			} else {
				if err == nil && height > 1 {
					assert.Fail(t, "Peer", id, "got message but isn't authorized! Height:", height)
				}
			}
		}
		logger.Debug("All peers have same ledger height!!!")
		return true
	}, 60*time.Second)
}

/*// Simple scenario to start first booting node, gossip a message
// then start second node and verify second node also receives it
func TestNewGossipStateProvider_GossipingOneMessage(t *testing.T) {
	bootId := 0
	ledgerPath := "/tmp/tests/ledger/"
	defer os.RemoveAll(ledgerPath)

	bootNodeCommitter := newCommitter(bootId, ledgerPath + "node/")
	defer bootNodeCommitter.Close()

	bootNode := newPeerNode(newGossipConfig(bootId, 100), bootNodeCommitter)
	defer bootNode.shutdown()

	rawblock := &peer.Block2{}
	if err := pb.Unmarshal([]byte{}, rawblock); err != nil {
		t.Fail()
	}

	if bytes, err := pb.Marshal(rawblock); err == nil {
		payload := &proto.Payload{1, "", bytes}
		bootNode.s.AddPayload(payload)
	} else {
		t.Fail()
	}

	waitUntilTrueOrTimeout(t, func() bool {
		if block := bootNode.s.GetBlock(uint64(1)); block != nil {
			return true
		}
		return false
	}, 5 * time.Second)

	bootNode.g.Gossip(createDataMsg(uint64(1), []byte{}, ""))

	peerCommitter := newCommitter(1, ledgerPath + "node/")
	defer peerCommitter.Close()

	peer := newPeerNode(newGossipConfig(1, 100, bootId), peerCommitter)
	defer peer.shutdown()

	ready := make(chan interface{})

	go func(p *peerNode) {
		for len(p.g.GetPeers()) != 1 {
			time.Sleep(100 * time.Millisecond)
		}
		ready <- struct{}{}
	}(peer)

	select {
	case <-ready:
		{
			break
		}
	case <-time.After(1 * time.Second):
		{
			t.Fail()
		}
	}

	// Let sure anti-entropy will have a chance to bring missing block
	waitUntilTrueOrTimeout(t, func() bool {
		if block := peer.s.GetBlock(uint64(1)); block != nil {
			return true
		}
		return false
	}, 2 * defAntiEntropyInterval + 1 * time.Second)

	block := peer.s.GetBlock(uint64(1))

	assert.NotNil(t, block)
}

func TestNewGossipStateProvider_RepeatGossipingOneMessage(t *testing.T) {
	for i := 0; i < 10; i++ {
		TestNewGossipStateProvider_GossipingOneMessage(t)
	}
}*/

func TestNewGossipStateProvider_SendingManyMessages(t *testing.T) {
	viper.Set("peer.fileSystemPath", "/tmp/tests/ledger/node")
	ledgermgmt.InitializeTestEnv()
	defer ledgermgmt.CleanupTestEnv()

	bootstrapSetSize := 5
	bootstrapSet := make([]*peerNode, 0)

	for i := 0; i < bootstrapSetSize; i++ {
		commit := newCommitter(i)
		bootstrapSet = append(bootstrapSet, newPeerNode(newGossipConfig(i), commit, noopPeerIdentityAcceptor))
	}

	defer func() {
		for _, p := range bootstrapSet {
			p.shutdown()
		}
	}()

	msgCount := 10

	for i := 1; i <= msgCount; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		if b, err := pb.Marshal(rawblock); err == nil {
			payload := &proto.Payload{
				SeqNum: uint64(i),
				Data:   b,
			}
			bootstrapSet[0].s.AddPayload(payload)
		} else {
			t.Fail()
		}
	}

	standartPeersSize := 10
	peersSet := make([]*peerNode, 0)

	for i := 0; i < standartPeersSize; i++ {
		commit := newCommitter(bootstrapSetSize + i)
		peersSet = append(peersSet, newPeerNode(newGossipConfig(bootstrapSetSize+i, 0, 1, 2, 3, 4), commit, noopPeerIdentityAcceptor))
	}

	defer func() {
		for _, p := range peersSet {
			p.shutdown()
		}
	}()

	waitUntilTrueOrTimeout(t, func() bool {
		for _, p := range peersSet {
			if len(p.g.PeersOfChannel(common.ChainID(util.GetTestChainID()))) != bootstrapSetSize+standartPeersSize-1 {
				logger.Debug("Peer discovery has not finished yet")
				return false
			}
		}
		logger.Debug("All peer discovered each other!!!")
		return true
	}, 30*time.Second)

	logger.Debug("Waiting for all blocks to arrive.")
	waitUntilTrueOrTimeout(t, func() bool {
		logger.Debug("Trying to see all peers get all blocks")
		for _, p := range peersSet {
			height, err := p.commit.LedgerHeight()
			if height != uint64(msgCount+1) || err != nil {
				return false
			}
		}
		logger.Debug("All peers have same ledger height!!!")
		return true
	}, 60*time.Second)
}

func TestGossipStateProvider_TestStateMessages(t *testing.T) {
	viper.Set("peer.fileSystemPath", "/tmp/tests/ledger/node")
	ledgermgmt.InitializeTestEnv()
	defer ledgermgmt.CleanupTestEnv()

	bootPeer := newPeerNode(newGossipConfig(0), newCommitter(0), noopPeerIdentityAcceptor)
	defer bootPeer.shutdown()

	peer := newPeerNode(newGossipConfig(1, 0), newCommitter(1), noopPeerIdentityAcceptor)
	defer peer.shutdown()

	naiveStateMsgPredicate := func(message interface{}) bool {
		return message.(proto.ReceivedMessage).GetGossipMessage().IsRemoteStateMessage()
	}

	_, bootCh := bootPeer.g.Accept(naiveStateMsgPredicate, true)
	_, peerCh := peer.g.Accept(naiveStateMsgPredicate, true)

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		msg := <-bootCh
		logger.Info("Bootstrap node got message, ", msg)
		assert.True(t, msg.GetGossipMessage().GetStateRequest() != nil)
		msg.Respond(&proto.GossipMessage{
			Content: &proto.GossipMessage_StateResponse{&proto.RemoteStateResponse{nil}},
		})
		wg.Done()
	}()

	go func() {
		msg := <-peerCh
		logger.Info("Peer node got an answer, ", msg)
		assert.True(t, msg.GetGossipMessage().GetStateResponse() != nil)
		wg.Done()

	}()

	readyCh := make(chan struct{})
	go func() {
		wg.Wait()
		readyCh <- struct{}{}
	}()

	time.Sleep(time.Duration(5) * time.Second)
	logger.Info("Sending gossip message with remote state request")

	chainID := common.ChainID(util.GetTestChainID())

	peer.g.Send(&proto.GossipMessage{
		Content: &proto.GossipMessage_StateRequest{&proto.RemoteStateRequest{0, 1}},
	}, &comm.RemotePeer{peer.g.PeersOfChannel(chainID)[0].Endpoint, peer.g.PeersOfChannel(chainID)[0].PKIid})
	logger.Info("Waiting until peers exchange messages")

	select {
	case <-readyCh:
		{
			logger.Info("Done!!!")

		}
	case <-time.After(time.Duration(10) * time.Second):
		{
			t.Fail()
		}
	}
}

// Start one bootstrap peer and submit defAntiEntropyBatchSize + 5 messages into
// local ledger, next spawning a new peer waiting for anti-entropy procedure to
// complete missing blocks. Since state transfer messages now batched, it is expected
// to see _exactly_ two messages with state transfer response.
func TestNewGossipStateProvider_BatchingOfStateRequest(t *testing.T) {
	viper.Set("peer.fileSystemPath", "/tmp/tests/ledger/node")
	ledgermgmt.InitializeTestEnv()
	defer ledgermgmt.CleanupTestEnv()

	bootPeer := newPeerNode(newGossipConfig(0), newCommitter(0), noopPeerIdentityAcceptor)
	defer bootPeer.shutdown()

	msgCount := defAntiEntropyBatchSize + 5
	expectedMessagesCnt := 2

	for i := 1; i <= msgCount; i++ {
		rawblock := pcomm.NewBlock(uint64(i), []byte{})
		if b, err := pb.Marshal(rawblock); err == nil {
			payload := &proto.Payload{
				SeqNum: uint64(i),
				Data:   b,
			}
			bootPeer.s.AddPayload(payload)
		} else {
			t.Fail()
		}
	}

	peer := newPeerNode(newGossipConfig(1, 0), newCommitter(1), noopPeerIdentityAcceptor)
	defer peer.shutdown()

	naiveStateMsgPredicate := func(message interface{}) bool {
		return message.(proto.ReceivedMessage).GetGossipMessage().IsRemoteStateMessage()
	}
	_, peerCh := peer.g.Accept(naiveStateMsgPredicate, true)

	messageCh := make(chan struct{})
	stopWaiting := make(chan struct{})

	// Number of submitted messages is defAntiEntropyBatchSize + 5, therefore
	// expected number of batches is expectedMessagesCnt = 2. Following go routine
	// makes sure it receives expected amount of messages and sends signal of success
	// to continue the test
	go func(expected int) {
		cnt := 0
		for cnt < expected {
			select {
			case <-peerCh:
				{
					cnt++
				}

			case <-stopWaiting:
				{
					return
				}
			}
		}

		messageCh <- struct{}{}
	}(expectedMessagesCnt)

	// Waits for message which indicates that expected number of message batches received
	// otherwise timeouts after 2 * defAntiEntropyInterval + 1 seconds
	select {
	case <-messageCh:
		{
			// Once we got message which indicate of two batches being received,
			// making sure messages indeed committed.
			waitUntilTrueOrTimeout(t, func() bool {
				if len(peer.g.PeersOfChannel(common.ChainID(util.GetTestChainID()))) != 1 {
					logger.Debug("Peer discovery has not finished yet")
					return false
				}
				logger.Debug("All peer discovered each other!!!")
				return true
			}, 30*time.Second)

			logger.Debug("Waiting for all blocks to arrive.")
			waitUntilTrueOrTimeout(t, func() bool {
				logger.Debug("Trying to see all peers get all blocks")
				height, err := peer.commit.LedgerHeight()
				if height != uint64(msgCount+1) || err != nil {
					return false
				}
				logger.Debug("All peers have same ledger height!!!")
				return true
			}, 60*time.Second)
		}
	case <-time.After(defAntiEntropyInterval*2 + time.Second*1):
		{
			close(stopWaiting)
			t.Fatal("Expected to receive two batches with missing payloads")
		}
	}
}

// coordinatorMock mocking structure to capture mock interface for
// coord to simulate coord flow during the test
type coordinatorMock struct {
	mock.Mock
}

func (mock *coordinatorMock) GetPvtDataAndBlockByNum(seqNum uint64, filter PvtDataFilter) (*pcomm.Block, PvtDataCollections, error) {
	args := mock.Called(seqNum)
	return args.Get(0).(*pcomm.Block), args.Get(1).(PvtDataCollections), args.Error(2)
}

func (mock *coordinatorMock) GetBlockByNum(seqNum uint64) (*pcomm.Block, error) {
	args := mock.Called(seqNum)
	return args.Get(0).(*pcomm.Block), args.Error(1)
}

func (mock *coordinatorMock) StoreBlock(block *pcomm.Block, data ...PvtDataCollections) ([]string, error) {
	args := mock.Called(block, data)
	return args.Get(0).([]string), args.Error(1)
}

func (mock *coordinatorMock) LedgerHeight() (uint64, error) {
	args := mock.Called()
	return args.Get(0).(uint64), args.Error(1)
}

func (mock *coordinatorMock) Close() {
	mock.Called()
}

type receivedMessageMock struct {
	mock.Mock
}

func (mock *receivedMessageMock) Respond(msg *proto.GossipMessage) {
	mock.Called(msg)
}

func (mock *receivedMessageMock) GetGossipMessage() *proto.SignedGossipMessage {
	args := mock.Called()
	return args.Get(0).(*proto.SignedGossipMessage)
}

func (mock *receivedMessageMock) GetSourceEnvelope() *proto.Envelope {
	args := mock.Called()
	return args.Get(0).(*proto.Envelope)
}

func (mock *receivedMessageMock) GetConnectionInfo() *proto.ConnectionInfo {
	args := mock.Called()
	return args.Get(0).(*proto.ConnectionInfo)
}

type testData struct {
	block   *pcomm.Block
	pvtData PvtDataCollections
}

func TestTransferOfPrivateRWSet(t *testing.T) {
	chainID := "testChainID"

	// First gossip instance
	g := &mocks.GossipMock{}
	coord1 := new(coordinatorMock)

	gossipChannel := make(chan *proto.GossipMessage)
	commChannel := make(chan proto.ReceivedMessage)

	gossipChannelFactory := func(ch chan *proto.GossipMessage) <-chan *proto.GossipMessage {
		return ch
	}

	commChannelFactory := func(ch chan proto.ReceivedMessage) <-chan proto.ReceivedMessage {
		return ch
	}

	g.On("Accept", mock.Anything, false).Return(gossipChannelFactory(gossipChannel), nil)
	g.On("Accept", mock.Anything, true).Return(nil, commChannelFactory(commChannel))

	g.On("UpdateChannelMetadata", mock.Anything, mock.Anything)
	g.On("PeersOfChannel", mock.Anything).Return([]discovery.NetworkMember{})
	g.On("Close")

	coord1.On("LedgerHeight", mock.Anything).Return(uint64(5), nil)

	var data map[uint64]*testData = map[uint64]*testData{
		uint64(2): {
			block: &pcomm.Block{
				Header: &pcomm.BlockHeader{
					Number:       2,
					DataHash:     []byte{0, 1, 1, 1},
					PreviousHash: []byte{0, 0, 0, 1},
				},
				Data: &pcomm.BlockData{
					Data: [][]byte{{1}, {2}, {3}},
				},
			},
			pvtData: PvtDataCollections{
				{
					Payload: &ledger.TxPvtData{
						SeqInBlock: uint64(0),
						WriteSet: &rwset.TxPvtReadWriteSet{
							DataModel: rwset.TxReadWriteSet_KV,
							NsPvtRwset: []*rwset.NsPvtReadWriteSet{
								{
									Namespace: "myCC:v1",
									CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
										{
											CollectionName: "mysecrectCollection",
											Rwset:          []byte{1, 2, 3, 4, 5},
										},
									},
								},
							},
						},
					},
				},
			},
		},

		uint64(3): {
			block: &pcomm.Block{
				Header: &pcomm.BlockHeader{
					Number:       3,
					DataHash:     []byte{1, 1, 1, 1},
					PreviousHash: []byte{0, 1, 1, 1},
				},
				Data: &pcomm.BlockData{
					Data: [][]byte{{4}, {5}, {6}},
				},
			},
			pvtData: PvtDataCollections{
				{
					Payload: &ledger.TxPvtData{
						SeqInBlock: uint64(2),
						WriteSet: &rwset.TxPvtReadWriteSet{
							DataModel: rwset.TxReadWriteSet_KV,
							NsPvtRwset: []*rwset.NsPvtReadWriteSet{
								{
									Namespace: "otherCC:v1",
									CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
										{
											CollectionName: "topClassified",
											Rwset:          []byte{0, 0, 0, 4, 2},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for seqNum, each := range data {
		coord1.On("GetPvtDataAndBlockByNum", seqNum).Return(each.block, each.pvtData, nil /* no error*/)
	}

	coord1.On("Close")

	servicesAdapater := &ServicesMediator{GossipAdapter: g, MCSAdapter: &cryptoServiceMock{acceptor: noopPeerIdentityAcceptor}}
	st := NewGossipCoordinatedStateProvider(chainID, servicesAdapater, coord1)
	defer st.Stop()

	// Mocked state request message
	requestMsg := new(receivedMessageMock)

	// Get state request message, blocks [2...3]
	requestGossipMsg := &proto.GossipMessage{
		// Copy nonce field from the request, so it will be possible to match response
		Nonce:   1,
		Tag:     proto.GossipMessage_CHAN_OR_ORG,
		Channel: []byte(chainID),
		Content: &proto.GossipMessage_StateRequest{&proto.RemoteStateRequest{
			StartSeqNum: 2,
			EndSeqNum:   3,
		}},
	}

	msg, _ := requestGossipMsg.NoopSign()

	requestMsg.On("GetGossipMessage").Return(msg)

	// Channel to send responses back
	responseChannel := make(chan proto.ReceivedMessage)
	defer close(responseChannel)

	requestMsg.On("Respond", mock.Anything).Run(func(args mock.Arguments) {
		// Get gossip response to respond back on state request
		response := args.Get(0).(*proto.GossipMessage)
		// Wrap it up into received response
		receivedMsg := new(receivedMessageMock)
		// Create sign response
		msg, _ := response.NoopSign()
		// Mock to respond
		receivedMsg.On("GetGossipMessage").Return(msg)
		// Send response
		responseChannel <- receivedMsg
	})

	// Send request message via communication channel into state transfer
	commChannel <- requestMsg

	// State transfer request should result in state response back
	response := <-responseChannel

	// Start the assertion section
	stateResponse := response.GetGossipMessage().GetStateResponse()

	assertion := assert.New(t)
	// Nonce should be equal to Nonce of the request
	assertion.Equal(response.GetGossipMessage().Nonce, uint64(1))
	// Payload should not need be nil
	assertion.NotNil(stateResponse)
	assertion.NotNil(stateResponse.Payloads)
	// Exactly two messages expected
	assertion.Equal(len(stateResponse.Payloads), 2)

	// Assert we have all data and it's same as we expected it
	for _, each := range stateResponse.Payloads {
		block := &pcomm.Block{}
		err := pb.Unmarshal(each.Data, block)
		assertion.NoError(err)

		assertion.NotNil(block.Header)

		testBlock, ok := data[block.Header.Number]
		assertion.True(ok)

		for i, d := range testBlock.block.Data.Data {
			assertion.True(bytes.Equal(d, block.Data.Data[i]))
		}

		for i, p := range testBlock.pvtData {
			pvtDataPayload := &proto.PvtDataPayload{}
			err := pb.Unmarshal(each.PrivateData[i], pvtDataPayload)
			assertion.NoError(err)
			pvtRWSet := &rwset.TxPvtReadWriteSet{}
			err = pb.Unmarshal(pvtDataPayload.Payload, pvtRWSet)
			assertion.NoError(err)
			assertion.Equal(p.Payload.WriteSet, pvtRWSet)
		}
	}
}

type testPeer struct {
	*mocks.GossipMock
	id            string
	gossipChannel chan *proto.GossipMessage
	commChannel   chan proto.ReceivedMessage
	coord         *coordinatorMock
}

func (t testPeer) Gossip() <-chan *proto.GossipMessage {
	return t.gossipChannel
}

func (t testPeer) Comm() <-chan proto.ReceivedMessage {
	return t.commChannel
}

var peers map[string]testPeer = map[string]testPeer{
	"peer1": {
		id:            "peer1",
		gossipChannel: make(chan *proto.GossipMessage),
		commChannel:   make(chan proto.ReceivedMessage),
		GossipMock:    &mocks.GossipMock{},
		coord:         new(coordinatorMock),
	},
	"peer2": {
		id:            "peer2",
		gossipChannel: make(chan *proto.GossipMessage),
		commChannel:   make(chan proto.ReceivedMessage),
		GossipMock:    &mocks.GossipMock{},
		coord:         new(coordinatorMock),
	},
}

func TestTransferOfPvtDataBetweenPeers(t *testing.T) {
	/*
	   This test covers pretty basic scenario, there are two peers: "peer1" and "peer2",
	   while peer2 missing a few blocks in the ledger therefore asking to replicate those
	   blocks from the first peers.

	   Test going to check that block from one peer will be replicated into second one and
	   have identical content.
	*/

	chainID := "testChainID"

	// Initialize peer
	for _, peer := range peers {
		peer.On("Accept", mock.Anything, false).Return(peer.Gossip(), nil)
		peer.On("Accept", mock.Anything, true).Return(nil, peer.Comm())
		peer.On("UpdateChannelMetadata", mock.Anything, mock.Anything)
		peer.coord.On("Close")
		peer.On("Close")
	}

	// First peer going to have more advanced ledger
	peers["peer1"].coord.On("LedgerHeight", mock.Anything).Return(uint64(3), nil)

	// Second peer has a gap of one block, hence it will have to replicate it from previous
	peers["peer2"].coord.On("LedgerHeight", mock.Anything).Return(uint64(2), nil)

	peers["peer1"].coord.On("GetPvtDataAndBlockByNum", uint64(2)).Return(&pcomm.Block{
		Header: &pcomm.BlockHeader{
			Number:       2,
			DataHash:     []byte{0, 1, 1, 1},
			PreviousHash: []byte{0, 0, 0, 1},
		},
		Data: &pcomm.BlockData{
			Data: [][]byte{{1}, {2}, {3}},
		},
	}, PvtDataCollections{}, nil)

	peers["peer1"].coord.On("GetPvtDataAndBlockByNum", uint64(3)).Return(&pcomm.Block{
		Header: &pcomm.BlockHeader{
			Number:       3,
			DataHash:     []byte{0, 0, 0, 1},
			PreviousHash: []byte{0, 1, 1, 1},
		},
		Data: &pcomm.BlockData{
			Data: [][]byte{{4}, {5}, {6}},
		},
	}, PvtDataCollections{&PvtData{
		Payload: &ledger.TxPvtData{
			SeqInBlock: uint64(1),
			WriteSet: &rwset.TxPvtReadWriteSet{
				DataModel: rwset.TxReadWriteSet_KV,
				NsPvtRwset: []*rwset.NsPvtReadWriteSet{
					{
						Namespace: "myCC:v1",
						CollectionPvtRwset: []*rwset.CollectionPvtReadWriteSet{
							{
								CollectionName: "mysecrectCollection",
								Rwset:          []byte{1, 2, 3, 4, 5},
							},
						},
					},
				},
			},
		},
	}}, nil)

	// Return membership of the peers
	metastate := &NodeMetastate{LedgerHeight: uint64(2)}
	metaBytes, err := metastate.Bytes()
	assert.NoError(t, err)
	member2 := discovery.NetworkMember{
		PKIid:            common.PKIidType([]byte{2}),
		Endpoint:         "peer2:7051",
		InternalEndpoint: "peer2:7051",
		Metadata:         metaBytes,
	}

	metastate = &NodeMetastate{LedgerHeight: uint64(3)}
	metaBytes, err = metastate.Bytes()
	assert.NoError(t, err)
	member1 := discovery.NetworkMember{
		PKIid:            common.PKIidType([]byte{1}),
		Endpoint:         "peer1:7051",
		InternalEndpoint: "peer1:7051",
		Metadata:         metaBytes,
	}

	peers["peer1"].On("PeersOfChannel", mock.Anything).Return([]discovery.NetworkMember{member2})
	peers["peer2"].On("PeersOfChannel", mock.Anything).Return([]discovery.NetworkMember{member1})

	peers["peer2"].On("Send", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		request := args.Get(0).(*proto.GossipMessage)
		requestMsg := new(receivedMessageMock)
		msg, _ := request.NoopSign()
		requestMsg.On("GetGossipMessage").Return(msg)

		requestMsg.On("Respond", mock.Anything).Run(func(args mock.Arguments) {
			response := args.Get(0).(*proto.GossipMessage)
			receivedMsg := new(receivedMessageMock)
			msg, _ := response.NoopSign()
			receivedMsg.On("GetGossipMessage").Return(msg)
			// Send response back to the peer
			peers["peer2"].commChannel <- receivedMsg
		})

		peers["peer1"].commChannel <- requestMsg
	})

	wg := sync.WaitGroup{}
	wg.Add(2)
	peers["peer2"].coord.On("StoreBlock", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		wg.Done() // Done once second peer hits commit of the block
	}).Return([]string{}, nil) // No pvt data to complete and no error

	cryptoService := &cryptoServiceMock{acceptor: noopPeerIdentityAcceptor}

	mediator := &ServicesMediator{GossipAdapter: peers["peer1"], MCSAdapter: cryptoService}
	peer1State := NewGossipCoordinatedStateProvider(chainID, mediator, peers["peer1"].coord)
	defer peer1State.Stop()

	mediator = &ServicesMediator{GossipAdapter: peers["peer2"], MCSAdapter: cryptoService}
	peer2State := NewGossipCoordinatedStateProvider(chainID, mediator, peers["peer2"].coord)
	defer peer2State.Stop()

	// Make sure state was replicated
	done := make(chan struct{})
	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-done:
		break
	case <-time.After(30 * time.Second):
		t.Fail()
	}
}

func waitUntilTrueOrTimeout(t *testing.T, predicate func() bool, timeout time.Duration) {
	ch := make(chan struct{})
	go func() {
		logger.Debug("Started to spin off, until predicate will be satisfied.")
		for !predicate() {
			time.Sleep(1 * time.Second)
		}
		ch <- struct{}{}
		logger.Debug("Done.")
	}()

	select {
	case <-ch:
		break
	case <-time.After(timeout):
		t.Fatal("Timeout has expired")
		break
	}
	logger.Debug("Stop waiting until timeout or true")
}
