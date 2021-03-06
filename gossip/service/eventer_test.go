/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package service

import (
	"reflect"
	"testing"

	"github.com/hyperledger/fabric/common/config/channel"
	"github.com/hyperledger/fabric/gossip/util"
	"github.com/hyperledger/fabric/protos/peer"
)

const testChainID = "foo"

func init() {
	util.SetupTestLogging()
}

type mockReceiver struct {
	orgs     map[string]config.ApplicationOrg
	sequence uint64
}

func (mr *mockReceiver) configUpdated(config Config) {
	logger.Debugf("[TEST] Setting config to %d %v", config.Sequence(), config.Organizations())
	mr.orgs = config.Organizations()
	mr.sequence = config.Sequence()
}

type mockConfig mockReceiver

func (mc *mockConfig) Sequence() uint64 {
	return mc.sequence
}

func (mc *mockConfig) Organizations() map[string]config.ApplicationOrg {
	return mc.orgs
}

func (mc *mockConfig) ChainID() string {
	return testChainID
}

const testOrgID = "testID"

func TestInitialUpdate(t *testing.T) {
	mc := &mockConfig{
		sequence: 7,
		orgs: map[string]config.ApplicationOrg{
			testOrgID: &appGrp{
				anchorPeers: []*peer.AnchorPeer{{Port: 9}},
			},
		},
	}

	mr := &mockReceiver{}

	ce := newConfigEventer(mr)
	ce.ProcessConfigUpdate(mc)

	if !reflect.DeepEqual(mc, (*mockConfig)(mr)) {
		t.Fatalf("Should have updated config on initial update but did not")
	}
}

func TestSecondUpdate(t *testing.T) {
	appGrps := map[string]config.ApplicationOrg{
		testOrgID: &appGrp{
			anchorPeers: []*peer.AnchorPeer{{Port: 9}},
		},
	}
	mc := &mockConfig{
		sequence: 7,
		orgs:     appGrps,
	}

	mr := &mockReceiver{}

	ce := newConfigEventer(mr)
	ce.ProcessConfigUpdate(mc)

	mc.sequence = 8
	appGrps[testOrgID] = &appGrp{
		anchorPeers: []*peer.AnchorPeer{{Port: 10}},
	}

	ce.ProcessConfigUpdate(mc)

	if !reflect.DeepEqual(mc, (*mockConfig)(mr)) {
		t.Fatal("Should have updated config on initial update but did not")
	}
}

func TestSecondSameUpdate(t *testing.T) {
	mc := &mockConfig{
		sequence: 7,
		orgs: map[string]config.ApplicationOrg{
			testOrgID: &appGrp{
				anchorPeers: []*peer.AnchorPeer{{Port: 9}},
			},
		},
	}

	mr := &mockReceiver{}

	ce := newConfigEventer(mr)
	ce.ProcessConfigUpdate(mc)
	mr.sequence = 0
	mr.orgs = nil
	ce.ProcessConfigUpdate(mc)

	if mr.sequence != 0 {
		t.Error("Should not have updated sequence when reprocessing same config")
	}

	if mr.orgs != nil {
		t.Error("Should not have updated anchor peers when reprocessing same config")
	}
}

func TestUpdatedSeqOnly(t *testing.T) {
	mc := &mockConfig{
		sequence: 7,
		orgs: map[string]config.ApplicationOrg{
			testOrgID: &appGrp{
				anchorPeers: []*peer.AnchorPeer{{Port: 9}},
			},
		},
	}

	mr := &mockReceiver{}

	ce := newConfigEventer(mr)
	ce.ProcessConfigUpdate(mc)
	mc.sequence = 9
	ce.ProcessConfigUpdate(mc)

	if mr.sequence != 7 {
		t.Errorf("Should not have updated sequence when reprocessing same config")
	}

	if !reflect.DeepEqual(mr.orgs, mc.orgs) {
		t.Errorf("Should not have cleared anchor peers when reprocessing newer config with higher sequence")
	}
}
