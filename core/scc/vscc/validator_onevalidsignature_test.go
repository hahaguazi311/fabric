/*
Copyright IBM Corp. 2016 All Rights Reserved.
SPDX-License-Identifier: Apache-2.0
*/
package vscc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/cauthdsl"
	mc "github.com/hyperledger/fabric/common/mocks/config"
	lm "github.com/hyperledger/fabric/common/mocks/ledger"
	"github.com/hyperledger/fabric/common/mocks/scc"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/chaincode/shim"
	"github.com/hyperledger/fabric/core/committer/txvalidator"
	"github.com/hyperledger/fabric/core/common/ccpackage"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/common/privdata"
	"github.com/hyperledger/fabric/core/common/sysccprovider"
	cutils "github.com/hyperledger/fabric/core/container/util"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	per "github.com/hyperledger/fabric/core/peer"
	"github.com/hyperledger/fabric/core/policy"
	"github.com/hyperledger/fabric/core/scc/lscc"
	"github.com/hyperledger/fabric/msp"
	mspmgmt "github.com/hyperledger/fabric/msp/mgmt"
	msptesttools "github.com/hyperledger/fabric/msp/mgmt/testtools"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/ledger/rwset/kvrwset"
	mspproto "github.com/hyperledger/fabric/protos/msp"
	"github.com/hyperledger/fabric/protos/peer"
	"github.com/hyperledger/fabric/protos/utils"
	"github.com/stretchr/testify/assert"
)

func createTx(endorsedByDuplicatedIdentity bool) (*common.Envelope, error) {
	ccid := &peer.ChaincodeID{Name: "foo", Version: "v1"}
	cis := &peer.ChaincodeInvocationSpec{ChaincodeSpec: &peer.ChaincodeSpec{ChaincodeId: ccid}}

	prop, _, err := utils.CreateProposalFromCIS(common.HeaderType_ENDORSER_TRANSACTION, util.GetTestChainID(), cis, sid)
	if err != nil {
		return nil, err
	}

	presp, err := utils.CreateProposalResponse(prop.Header, prop.Payload, &peer.Response{Status: 200}, []byte("res"), nil, ccid, nil, id)
	if err != nil {
		return nil, err
	}

	var env *common.Envelope
	if endorsedByDuplicatedIdentity {
		env, err = utils.CreateSignedTx(prop, id, presp, presp)
	} else {
		env, err = utils.CreateSignedTx(prop, id, presp)
	}
	if err != nil {
		return nil, err
	}
	return env, err
}

func processSignedCDS(cds *peer.ChaincodeDeploymentSpec, policy *common.SignaturePolicyEnvelope) ([]byte, error) {
	env, err := ccpackage.OwnerCreateSignedCCDepSpec(cds, policy, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create package %s", err)
	}

	b := utils.MarshalOrPanic(env)

	ccpack := &ccprovider.SignedCDSPackage{}
	cd, err := ccpack.InitFromBuffer(b)
	if err != nil {
		return nil, fmt.Errorf("error owner creating package %s", err)
	}

	if err = ccpack.PutChaincodeToFS(); err != nil {
		return nil, fmt.Errorf("error putting package on the FS %s", err)
	}

	cd.InstantiationPolicy = utils.MarshalOrPanic(policy)

	return utils.MarshalOrPanic(cd), nil
}

func constructDeploymentSpec(name string, path string, version string, initArgs [][]byte, createFS bool) (*peer.ChaincodeDeploymentSpec, error) {
	spec := &peer.ChaincodeSpec{Type: 1, ChaincodeId: &peer.ChaincodeID{Name: name, Path: path, Version: version}, Input: &peer.ChaincodeInput{Args: initArgs}}

	codePackageBytes := bytes.NewBuffer(nil)
	gz := gzip.NewWriter(codePackageBytes)
	tw := tar.NewWriter(gz)

	err := cutils.WriteBytesToPackage("src/garbage.go", []byte(name+path+version), tw)
	if err != nil {
		return nil, err
	}

	tw.Close()
	gz.Close()

	chaincodeDeploymentSpec := &peer.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: codePackageBytes.Bytes()}

	if createFS {
		err := ccprovider.PutChaincodeIntoFS(chaincodeDeploymentSpec)
		if err != nil {
			return nil, err
		}
	}

	return chaincodeDeploymentSpec, nil
}

func createCCDataRWset(nameK, nameV, version string, policy []byte) ([]byte, error) {
	cd := &ccprovider.ChaincodeData{
		Name:                nameV,
		Version:             version,
		InstantiationPolicy: policy,
	}

	cdbytes := utils.MarshalOrPanic(cd)

	rwsetBuilder := rwsetutil.NewRWSetBuilder()
	rwsetBuilder.AddToWriteSet("lscc", nameK, cdbytes)
	sr, err := rwsetBuilder.GetTxSimulationResults()
	if err != nil {
		return nil, err
	}
	return sr.GetPubSimulationBytes()
}

func createLSCCTx(ccname, ccver, f string, res []byte) (*common.Envelope, error) {
	return createLSCCTxPutCds(ccname, ccver, f, res, nil, true)
}

func createLSCCTxPutCds(ccname, ccver, f string, res, cdsbytes []byte, putcds bool) (*common.Envelope, error) {
	cds := &peer.ChaincodeDeploymentSpec{
		ChaincodeSpec: &peer.ChaincodeSpec{
			ChaincodeId: &peer.ChaincodeID{
				Name:    ccname,
				Version: ccver,
			},
			Type: peer.ChaincodeSpec_GOLANG,
		},
	}

	cdsBytes, err := proto.Marshal(cds)
	if err != nil {
		return nil, err
	}

	var cis *peer.ChaincodeInvocationSpec
	if putcds {
		if cdsbytes != nil {
			cdsBytes = cdsbytes
		}
		cis = &peer.ChaincodeInvocationSpec{
			ChaincodeSpec: &peer.ChaincodeSpec{
				ChaincodeId: &peer.ChaincodeID{Name: "lscc"},
				Input: &peer.ChaincodeInput{
					Args: [][]byte{[]byte(f), []byte("barf"), cdsBytes},
				},
				Type: peer.ChaincodeSpec_GOLANG,
			},
		}
	} else {
		cis = &peer.ChaincodeInvocationSpec{
			ChaincodeSpec: &peer.ChaincodeSpec{
				ChaincodeId: &peer.ChaincodeID{Name: "lscc"},
				Input: &peer.ChaincodeInput{
					Args: [][]byte{[]byte(f), []byte("barf")},
				},
				Type: peer.ChaincodeSpec_GOLANG,
			},
		}
	}

	prop, _, err := utils.CreateProposalFromCIS(common.HeaderType_ENDORSER_TRANSACTION, util.GetTestChainID(), cis, sid)
	if err != nil {
		return nil, err
	}

	ccid := &peer.ChaincodeID{Name: ccname, Version: ccver}

	presp, err := utils.CreateProposalResponse(prop.Header, prop.Payload, &peer.Response{Status: 200}, res, nil, ccid, nil, id)
	if err != nil {
		return nil, err
	}

	return utils.CreateSignedTx(prop, id, presp)
}

func TestInit(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	if res := stub.MockInit("1", nil); res.Status != shim.OK {
		t.Fatalf("vscc init failed with %s", res.Message)
	}
}

func getSignedByMSPMemberPolicy(mspID string) ([]byte, error) {
	p := cauthdsl.SignedByMspMember(mspID)

	b, err := utils.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("Could not marshal policy, err %s", err)
	}

	return b, err
}

func getSignedByOneMemberTwicePolicy(mspID string) ([]byte, error) {
	principal := &mspproto.MSPPrincipal{
		PrincipalClassification: mspproto.MSPPrincipal_ROLE,
		Principal:               utils.MarshalOrPanic(&mspproto.MSPRole{Role: mspproto.MSPRole_MEMBER, MspIdentifier: mspID})}

	p := &common.SignaturePolicyEnvelope{
		Version:    0,
		Rule:       cauthdsl.NOutOf(2, []*common.SignaturePolicy{cauthdsl.SignedBy(0), cauthdsl.SignedBy(0)}),
		Identities: []*mspproto.MSPPrincipal{principal},
	}
	b, err := utils.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("Could not marshal policy, err %s", err)
	}

	return b, err
}

func getSignedByMSPAdminPolicy(mspID string) ([]byte, error) {
	p := cauthdsl.SignedByMspAdmin(mspID)

	b, err := utils.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("Could not marshal policy, err %s", err)
	}

	return b, err
}

func TestInvoke(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)
	if res := stub.MockInit("1", nil); res.Status != shim.OK {
		t.Fatalf("vscc init failed with %s", res.Message)
	}

	// Failed path: Invalid arguments
	args := [][]byte{[]byte("dv")}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// not enough args
	args = [][]byte{[]byte("dv"), []byte("tx")}
	args[1] = nil
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// nil args
	args = [][]byte{nil, nil, nil}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// nil args
	args = [][]byte{[]byte("a"), []byte("a"), nil}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// broken Envelope
	args = [][]byte{[]byte("a"), []byte("a"), []byte("a")}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// (still) broken Envelope
	args = [][]byte{[]byte("a"), utils.MarshalOrPanic(&common.Envelope{Payload: []byte("barf")}), []byte("a")}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// (still) broken Envelope
	b := utils.MarshalOrPanic(&common.Envelope{Payload: utils.MarshalOrPanic(&common.Payload{Header: &common.Header{ChannelHeader: []byte("barf")}})})
	args = [][]byte{[]byte("a"), b, []byte("a")}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	tx, err := createTx(false)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// broken policy
	args = [][]byte{[]byte("dv"), envBytes, []byte("barf")}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	// broken type
	b = utils.MarshalOrPanic(&common.Envelope{Payload: utils.MarshalOrPanic(&common.Payload{Header: &common.Header{ChannelHeader: utils.MarshalOrPanic(&common.ChannelHeader{Type: int32(common.HeaderType_ORDERER_TRANSACTION)})}})})
	args = [][]byte{[]byte("dv"), b, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// broken tx payload
	b = utils.MarshalOrPanic(&common.Envelope{Payload: utils.MarshalOrPanic(&common.Payload{Header: &common.Header{ChannelHeader: utils.MarshalOrPanic(&common.ChannelHeader{Type: int32(common.HeaderType_ORDERER_TRANSACTION)})}})})
	args = [][]byte{[]byte("dv"), b, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// good path: signed by the right MSP
	args = [][]byte{[]byte("dv"), envBytes, policy}
	res := stub.MockInvoke("1", args)
	if res.Status != shim.OK {
		t.Fatalf("vscc invoke returned err %s", err)
	}

	// bad path: signed by the wrong MSP
	policy, err = getSignedByMSPMemberPolicy("barf")
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	// bad path: signed by duplicated MSP identity
	policy, err = getSignedByOneMemberTwicePolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}
	tx, err = createTx(true)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}
	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}
	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR || res.Message != DUPLICATED_IDENTITY_ERROR {
		t.Fatalf("vscc invoke should have failed due to policy evaluation failure caused by duplicated identity")
	}
}

func TestInvalidFunction(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	res, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.GETCCDATA, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestRWSetTooBig(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	cd := &ccprovider.ChaincodeData{
		Name:                ccname,
		Version:             ccver,
		InstantiationPolicy: nil,
	}

	cdbytes := utils.MarshalOrPanic(cd)

	rwsetBuilder := rwsetutil.NewRWSetBuilder()
	rwsetBuilder.AddToWriteSet("lscc", ccname, cdbytes)
	rwsetBuilder.AddToWriteSet("lscc", "spurious", []byte("spurious"))

	sr, err := rwsetBuilder.GetTxSimulationResults()
	assert.NoError(t, err)
	srBytes, err := sr.GetPubSimulationBytes()
	assert.NoError(t, err)
	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, srBytes)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestValidateDeployFail(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	/*********************/
	/* test no write set */
	/*********************/

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, nil)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/************************/
	/* test bogus write set */
	/************************/

	rwsetBuilder := rwsetutil.NewRWSetBuilder()
	rwsetBuilder.AddToWriteSet("lscc", ccname, []byte("barf"))
	sr, err := rwsetBuilder.GetTxSimulationResults()
	assert.NoError(t, err)
	resBogusBytes, err := sr.GetPubSimulationBytes()
	assert.NoError(t, err)
	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, resBogusBytes)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/**********************/
	/* test bad LSCC args */
	/**********************/

	res, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err = createLSCCTxPutCds(ccname, ccver, lscc.DEPLOY, res, nil, false)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/**********************/
	/* test bad LSCC args */
	/**********************/

	res, err = createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err = createLSCCTxPutCds(ccname, ccver, lscc.DEPLOY, res, []byte("barf"), true)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/***********************/
	/* test bad cc version */
	/***********************/

	res, err = createCCDataRWset(ccname, ccname, ccver+".1", nil)
	assert.NoError(t, err)

	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/*************/
	/* bad rwset */
	/*************/

	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, []byte("barf"))
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/********************/
	/* test bad cc name */
	/********************/

	res, err = createCCDataRWset(ccname+".badbad", ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/**********************/
	/* test bad cc name 2 */
	/**********************/

	res, err = createCCDataRWset(ccname, ccname+".badbad", ccver, nil)
	assert.NoError(t, err)

	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}

	/************************/
	/* test suprious writes */
	/************************/

	cd := &ccprovider.ChaincodeData{
		Name:                ccname,
		Version:             ccver,
		InstantiationPolicy: nil,
	}

	cdbytes := utils.MarshalOrPanic(cd)
	rwsetBuilder = rwsetutil.NewRWSetBuilder()
	rwsetBuilder.AddToWriteSet("lscc", ccname, cdbytes)
	rwsetBuilder.AddToWriteSet("bogusbogus", "key", []byte("val"))
	sr, err = rwsetBuilder.GetTxSimulationResults()
	assert.NoError(t, err)
	srBytes, err := sr.GetPubSimulationBytes()
	assert.NoError(t, err)
	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, srBytes)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestAlreadyDeployed(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, true)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	sProp2, _ := utils.MockSignedEndorserProposal2OrPanic(chainId, &peer.ChaincodeSpec{}, id)
	args := [][]byte{[]byte("deploy"), []byte(ccname), b}
	if res := stublccc.MockInvokeWithSignedProposal("1", args, sProp2); res.Status != shim.OK {
		fmt.Printf("%#v\n", res)
		t.FailNow()
	}

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invocation should have failed")
	}
}

func TestValidateDeployNoLedger(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		QErr: fmt.Errorf("Simulated error"),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	defaultPolicy, err := getSignedByMSPAdminPolicy(mspid)
	assert.NoError(t, err)
	res, err := createCCDataRWset(ccname, ccname, ccver, defaultPolicy)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestValidateDeployOK(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	defaultPolicy, err := getSignedByMSPAdminPolicy(mspid)
	assert.NoError(t, err)
	res, err := createCCDataRWset(ccname, ccname, ccver, defaultPolicy)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.OK {
		t.Fatalf("vscc invoke returned err %s", res.Message)
	}
}

func TestValidateDeployWithPolicies(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	/*********************************************/
	/* test 1: success with an accept-all policy */
	/*********************************************/

	res, err := createCCDataRWset(ccname, ccname, ccver, cauthdsl.MarshaledAcceptAllPolicy)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.OK {
		t.Fatalf("vscc invoke returned err %s", res.Message)
	}

	/********************************************/
	/* test 2: failure with a reject-all policy */
	/********************************************/

	res, err = createCCDataRWset(ccname, ccname, ccver, cauthdsl.MarshaledRejectAllPolicy)
	assert.NoError(t, err)

	tx, err = createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err = utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err = getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestInvalidUpgrade(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "2"

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invocation should have failed")
	}
}

func TestValidateUpgradeOK(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, true)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	sProp2, _ := utils.MockSignedEndorserProposal2OrPanic(chainId, &peer.ChaincodeSpec{}, id)
	args := [][]byte{[]byte("deploy"), []byte(ccname), b}
	if res := stublccc.MockInvokeWithSignedProposal("1", args, sProp2); res.Status != shim.OK {
		fmt.Printf("%#v\n", res)
		t.FailNow()
	}

	ccver = "2"

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.OK {
		t.Fatalf("vscc invoke returned err %s", res.Message)
	}
}

func TestInvalidateUpgradeBadVersion(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, true)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	sProp2, _ := utils.MockSignedEndorserProposal2OrPanic(chainId, &peer.ChaincodeSpec{}, id)
	args := [][]byte{[]byte("deploy"), []byte(ccname), b}
	if res := stublccc.MockInvokeWithSignedProposal("1", args, sProp2); res.Status != shim.OK {
		fmt.Printf("%#v\n", res)
		t.FailNow()
	}

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invoke should have failed")
	}
}

func TestValidateUpgradeWithPoliciesOK(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, false)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	_, err = processSignedCDS(cds, cauthdsl.AcceptAllPolicy)
	assert.NoError(t, err)
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	sProp2, _ := utils.MockSignedEndorserProposal2OrPanic(chainId, &peer.ChaincodeSpec{}, id)
	args := [][]byte{[]byte("deploy"), []byte(ccname), b}
	if res := stublccc.MockInvokeWithSignedProposal("1", args, sProp2); res.Status != shim.OK {
		fmt.Printf("%#v\n", res)
		t.FailNow()
	}

	ccver = "2"

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args = [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.OK {
		t.Fatalf("vscc invoke returned err %s", res.Message)
	}
}

func TestValidateUpgradeWithNewFailAllIP(t *testing.T) {
	// we're testing upgrade.
	// In particular, we want to test the scenario where the upgrader
	// complies with the instantiation policy of the current version
	// BUT NOT the instantiation policy of the new version. For this
	// reason we first deploy a cc with IP whic is equal to the AcceptAllPolicy
	// and then try to upgrade with a cc with the RejectAllPolicy.
	// We run this test twice, once with the V11 capability (and expect
	// a failure) and once without (and we expect success).

	validateUpgradeWithNewFailAllIP(t, true, true)
	validateUpgradeWithNewFailAllIP(t, false, false)
}

func validateUpgradeWithNewFailAllIP(t *testing.T, v11capability, expecterr bool) {
	// create the validator
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{V1_1ValidationRv: v11capability}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	// init both chaincodes
	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	// deploy the chaincode with an accept all policy

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"
	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, false)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	_, err = processSignedCDS(cds, cauthdsl.AcceptAllPolicy)
	assert.NoError(t, err)
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	sProp2, _ := utils.MockSignedEndorserProposal2OrPanic(chainId, &peer.ChaincodeSpec{}, id)
	args := [][]byte{[]byte("deploy"), []byte(ccname), b}
	if res := stublccc.MockInvokeWithSignedProposal("1", args, sProp2); res.Status != shim.OK {
		fmt.Printf("%#v\n", res)
		t.FailNow()
	}

	// if we're here, we have a cc deployed with an accept all IP

	// now we upgrade, with v 2 of the same cc, with the crucial difference that it has a reject all IP

	ccver = "2"

	simresres, err := createCCDataRWset(ccname, ccname, ccver,
		cauthdsl.MarshaledRejectAllPolicy, // here's where we specify the IP of the upgraded cc
	)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	// execute the upgrade tx
	args = [][]byte{[]byte("dv"), envBytes, policy}
	if expecterr {
		if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
			t.Fatalf("vscc invoke should have failed")
		}
	} else {
		if res := stub.MockInvoke("1", args); res.Status != shim.OK {
			t.Fatalf("vscc invoke failed with %s", res.Message)
		}
	}
}

func TestValidateUpgradeWithPoliciesFail(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	State := make(map[string]map[string][]byte)
	State["lscc"] = stublccc.State
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(State),
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"
	path := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	ppath := lccctestpath + "/" + ccname + "." + ccver

	os.Remove(ppath)

	cds, err := constructDeploymentSpec(ccname, path, ccver, [][]byte{[]byte("init"), []byte("a"), []byte("100"), []byte("b"), []byte("200")}, false)
	if err != nil {
		fmt.Printf("%s\n", err)
		t.FailNow()
	}
	cdbytes, err := processSignedCDS(cds, cauthdsl.RejectAllPolicy)
	assert.NoError(t, err)
	defer os.Remove(ppath)
	var b []byte
	if b, err = proto.Marshal(cds); err != nil || b == nil {
		t.FailNow()
	}

	// Simulate the lscc invocation whilst skipping the policy validation,
	// otherwise we wouldn't be able to deply a chaincode with a reject all policy
	stublccc.MockTransactionStart("barf")
	err = stublccc.PutState(ccname, cdbytes)
	assert.NoError(t, err)
	stublccc.MockTransactionEnd("barf")

	ccver = "2"

	simresres, err := createCCDataRWset(ccname, ccname, ccver, nil)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.UPGRADE, simresres)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	if res := stub.MockInvoke("1", args); res.Status != shim.ERROR {
		t.Fatalf("vscc invocation should have failed")
	}
}

var id msp.SigningIdentity
var sid []byte
var mspid string
var chainId string = util.GetTestChainID()

type mockPolicyCheckerFactory struct {
}

func (c *mockPolicyCheckerFactory) NewPolicyChecker() policy.PolicyChecker {
	return &mockPolicyChecker{}
}

type mockPolicyChecker struct {
}

func (c *mockPolicyChecker) CheckPolicy(channelID, policyName string, signedProp *peer.SignedProposal) error {
	return nil
}

func (c *mockPolicyChecker) CheckPolicyBySignedData(channelID, policyName string, sd []*common.SignedData) error {
	return nil
}

func (c *mockPolicyChecker) CheckPolicyNoChannel(policyName string, signedProp *peer.SignedProposal) error {
	return nil
}

func TestValidateDeployRWSetAndCollection(t *testing.T) {
	chid := "ch"
	ccid := "cc"

	cd := &ccprovider.ChaincodeData{Name: "mycc"}

	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	State := make(map[string]map[string][]byte)
	State["lscc"] = make(map[string][]byte)
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{Qe: lm.NewMockQueryExecutor(State)})

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	rwset := &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}, {Key: "b"}, {Key: "c"}}}

	err := v.validateDeployRWSetAndCollection(rwset, nil, nil, chid, ccid)
	assertNonIntermittentError(t, err)

	rwset = &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}, {Key: "b"}}}

	err = v.validateDeployRWSetAndCollection(rwset, cd, nil, chid, ccid)
	assertNonIntermittentError(t, err)

	rwset = &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}}}

	err = v.validateDeployRWSetAndCollection(rwset, cd, nil, chid, ccid)
	assertNonIntermittentError(t, err)

	lsccargs := [][]byte{nil, nil, nil, nil, nil, nil}

	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	rwset = &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}, {Key: privdata.BuildCollectionKVSKey("mycc")}}}

	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	lsccargs = [][]byte{nil, nil, nil, nil, nil, []byte("barf")}

	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	lsccargs = [][]byte{nil, nil, nil, nil, nil, []byte("barf")}
	rwset = &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}, {Key: privdata.BuildCollectionKVSKey("mycc"), Value: []byte("barf")}}}

	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	cc := &common.CollectionConfig{Payload: &common.CollectionConfig_StaticCollectionConfig{&common.StaticCollectionConfig{Name: "mycollection"}}}
	ccp := &common.CollectionConfigPackage{[]*common.CollectionConfig{cc}}
	ccpBytes, err := proto.Marshal(ccp)
	assert.NoError(t, err)
	assert.NotNil(t, ccpBytes)

	lsccargs = [][]byte{nil, nil, nil, nil, nil, ccpBytes}
	rwset = &kvrwset.KVRWSet{Writes: []*kvrwset.KVWrite{{Key: "a"}, {Key: privdata.BuildCollectionKVSKey("mycc"), Value: ccpBytes}}}

	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	State["lscc"][(&collectionStoreSupport{v.sccprovider}).GetCollectionKVSKey(common.CollectionCriteria{Channel: chid, Namespace: ccid})] = []byte("barf")
	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	State["lscc"][(&collectionStoreSupport{v.sccprovider}).GetCollectionKVSKey(common.CollectionCriteria{Channel: chid, Namespace: ccid})] = ccpBytes
	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertNonIntermittentError(t, err)

	delete(State, "lscc") // missing namespace in this mock query executor causes to return an error
	err = v.validateDeployRWSetAndCollection(rwset, cd, lsccargs, chid, ccid)
	assertIntermittentError(t, err)
}

var lccctestpath = "/tmp/lscc-validation-test"

func TestMain(m *testing.M) {
	ccprovider.SetChaincodesPath(lccctestpath)
	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	policy.RegisterPolicyCheckerFactory(&mockPolicyCheckerFactory{})

	mspGetter := func(cid string) []string {
		return []string{"DEFAULT"}
	}

	per.MockSetMSPIDGetter(mspGetter)

	var err error

	// setup the MSP manager so that we can sign/verify
	msptesttools.LoadMSPSetupForTesting()

	id, err = mspmgmt.GetLocalMSP().GetDefaultSigningIdentity()
	if err != nil {
		fmt.Printf("GetSigningIdentity failed with err %s", err)
		os.Exit(-1)
	}

	sid, err = id.Serialize()
	if err != nil {
		fmt.Printf("Serialize failed with err %s", err)
		os.Exit(-1)
	}

	// determine the MSP identifier for the first MSP in the default chain
	var msp msp.MSP
	mspMgr := mspmgmt.GetManagerForChain(chainId)
	msps, err := mspMgr.GetMSPs()
	if err != nil {
		fmt.Printf("Could not retrieve the MSPs for the chain manager, err %s", err)
		os.Exit(-1)
	}
	if len(msps) == 0 {
		fmt.Printf("At least one MSP was expected")
		os.Exit(-1)
	}
	for _, m := range msps {
		msp = m
		break
	}
	mspid, err = msp.GetIdentifier()
	if err != nil {
		fmt.Printf("Failure getting the msp identifier, err %s", err)
		os.Exit(-1)
	}

	// also set the MSP for the "test" chain
	mspmgmt.XXXSetMSPManager("mycc", mspmgmt.GetManagerForChain(util.GetTestChainID()))

	os.Exit(m.Run())
}

func TestIntermittentErrorResponse(t *testing.T) {
	v := new(ValidatorOneValidSignature)
	stub := shim.NewMockStub("validatoronevalidsignature", v)

	lccc := lscc.NewLifeCycleSysCC()
	stublccc := shim.NewMockStub("lscc", lccc)

	sysccprovider.RegisterSystemChaincodeProviderFactory(&scc.MocksccProviderFactory{
		Qe: lm.NewMockQueryExecutor(nil), // mock query executor causes an error if supplied with an empty state
		ApplicationConfigBool: true,
		ApplicationConfigRv:   &mc.MockApplication{&mc.MockApplicationCapabilities{}},
	})
	stub.MockPeerChaincode("lscc", stublccc)

	r1 := stub.MockInit("1", [][]byte{})
	if r1.Status != shim.OK {
		fmt.Println("Init failed", string(r1.Message))
		t.FailNow()
	}

	r := stublccc.MockInit("1", [][]byte{})
	if r.Status != shim.OK {
		fmt.Println("Init failed", string(r.Message))
		t.FailNow()
	}

	ccname := "mycc"
	ccver := "1"

	defaultPolicy, err := getSignedByMSPAdminPolicy(mspid)
	assert.NoError(t, err)
	res, err := createCCDataRWset(ccname, ccname, ccver, defaultPolicy)
	assert.NoError(t, err)

	tx, err := createLSCCTx(ccname, ccver, lscc.DEPLOY, res)
	if err != nil {
		t.Fatalf("createTx returned err %s", err)
	}

	envBytes, err := utils.GetBytesEnvelope(tx)
	if err != nil {
		t.Fatalf("GetBytesEnvelope returned err %s", err)
	}

	// good path: signed by the right MSP
	policy, err := getSignedByMSPMemberPolicy(mspid)
	if err != nil {
		t.Fatalf("failed getting policy, err %s", err)
	}

	args := [][]byte{[]byte("dv"), envBytes, policy}
	shimRes := stub.MockInvoke("1", args)
	t.Logf("shimRes = %#v", shimRes)
	if res := stub.MockInvoke("1", args); res.Status != txvalidator.IntermittentErrorCode {
		t.Fatalf("vscc invocation should have failed with an intermittent error response")
	}
}

func assertIntermittentError(t *testing.T, err error) {
	_, ok := err.(*intermittentError)
	assert.True(t, ok)
}

func assertNonIntermittentError(t *testing.T, err error) {
	_, ok := err.(*intermittentError)
	assert.False(t, ok)
}
