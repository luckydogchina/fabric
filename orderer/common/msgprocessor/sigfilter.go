/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package msgprocessor

import (
	"fmt"

	"github.com/hyperledger/fabric/common/policies"
	cb "github.com/hyperledger/fabric/protos/common"

	"github.com/pkg/errors"
)

type sigFilter struct {
	policyName    string
	policyManager policies.Manager
}

// NewSigFilter creates a new signature filter, at every evaluation, the policy manager is called
// to retrieve the latest version of the policy
func NewSigFilter(policyName string, policyManager policies.Manager) Rule {
	return &sigFilter{
		policyName:    policyName,
		policyManager: policyManager,
	}
}

// Apply applies the policy given, resulting in Reject or Forward, never Accept
func (sf *sigFilter) Apply(message *cb.Envelope) error {
	signedData, err := message.AsSignedData()

	if err != nil {
		return fmt.Errorf("could not convert message to signedData: %s", err)
	}

	policy, ok := sf.policyManager.GetPolicy(sf.policyName)
	if !ok {
		return fmt.Errorf("could not find policy %s", sf.policyName)
	}

	err = policy.Evaluate(signedData)
	if err != nil {
		return errors.Wrap(errors.WithStack(ErrPermissionDenied), err.Error())
	}
	return nil
}
