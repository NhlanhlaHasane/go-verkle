package verkle

import (
	"bytes"
	"github.com/ethereum/go-ethereum/common"
	"github.com/protolambda/go-kzg/bls"
	"testing"
)

func TestGoerliInsertBug(t *testing.T) {
	root := New(10, lg1)
	root.InsertOrdered(common.Hex2Bytes("000c9f87eb59996c38b587bb3a5a49b85a64b8b6bb7dd76e87125fe1370071a2"), common.Hex2Bytes("f84b018701d7c17cd98200a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a0c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"), ks, nil)
	root.InsertOrdered(common.Hex2Bytes("000ca9506198b51956083dabde9b3c5c0c4251b56ea4741396ce02631c4be379"), common.Hex2Bytes("f84b018701d7b0b950b200a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a0c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"), ks, nil)
	root.InsertOrdered(common.Hex2Bytes("000ca9538ed7e9a5688464cc41c8c5f20af324c76ea78360abe7d57185c23834"), common.Hex2Bytes("f8440180a01a67cc51538c651f63e8d55094b0ae7bca7f623f05a9ff77ca815dd44d5c8322a010b37de11f39e0a372615c70e1d4d7c613937e8f61823d59be9bea62112e175c"), ks, nil)
	expected := common.Hex2Bytes("a4195c94ee2ecb3f2d978be1133be9abb54f1e86aacdfceca1e8e9833b30a92ee544e78cfbe803f30263f5d93b54c005")

	comm := root.ComputeCommitment(ks)
	got := bls.ToCompressedG1(comm)

	if !bytes.Equal(got, expected) {
		t.Fatalf("incorrect root commitment %x != %x", got, expected)
	}
}
