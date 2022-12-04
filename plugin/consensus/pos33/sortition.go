package pos33

import (
	"bytes"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/common/crypto"
	"github.com/33cn/chain33/common/difficulty"
	vrf "github.com/33cn/chain33/common/vrf/secp256k1"
	"github.com/33cn/chain33/types"
	secp256k1 "github.com/btcsuite/btcd/btcec"
	"github.com/golang/protobuf/proto"
	pt "github.com/yccproject/ycc/plugin/dapp/pos33/types"
)

var big1 = big.NewInt(1)
var max = big1.Lsh(big1, 256)
var fmax = big.NewFloat(0).SetInt(max) // 2^^256

const (
	Committee = 0
)

// 算法依据：
// 1. 通过签名，然后hash，得出的Hash值是在[0，max]的范围内均匀分布并且随机的, 那么Hash/max实在[1/max, 1]之间均匀分布的
// 2. 那么从N个选票中抽出M个选票，等价于计算N次Hash, 并且Hash/max < M/N

func calcuVrfHash(input proto.Message, priv crypto.PrivKey) ([]byte, []byte) {
	privKey, _ := secp256k1.PrivKeyFromBytes(secp256k1.S256(), priv.Bytes())
	vrfPriv := &vrf.PrivateKey{PrivateKey: (*ecdsa.PrivateKey)(privKey)}
	in := types.Encode(input)
	vrfHash, vrfProof := vrfPriv.Evaluate(in)
	return vrfHash[:], vrfProof
}

func sortF(vrfHash []byte, index, num int, diff float64, proof *pt.HashProof) *pt.Pos33SortMsg {
	data := fmt.Sprintf("%x+%d+%d", vrfHash, index, num)
	hash := hash2([]byte(data))

	tmpHash := make([]byte, len(hash))
	copy(tmpHash, hash)

	// 转为big.Float计算，比较难度diff
	y := difficulty.HashToBig(tmpHash)
	z := new(big.Float).SetInt(y)
	if new(big.Float).Quo(z, fmax).Cmp(big.NewFloat(diff)) > 0 {
		return nil
	}

	// 符合，表示抽中了
	m := &pt.Pos33SortMsg{
		SortHash: &pt.SortHash{Hash: hash, Index: int64(index), Num: int32(num)},
		Proof:    proof,
	}
	return m
}

type sortArg struct {
	vrfHash []byte
	index   int
	num     int
	diff    float64
	proof   *pt.HashProof
	ch      chan<- *pt.Pos33SortMsg
}

func (n *node) runSortition() {
	for i := 0; i < 8; i++ {
		go func() {
			for s := range n.sortCh {
				s.ch <- sortF(s.vrfHash, s.index, s.num, s.diff, s.proof)
			}
		}()
	}
}

func (n *node) doSort(vrfHash []byte, count, num int, diff float64, proof *pt.HashProof) []*pt.Pos33SortMsg {
	ch := make(chan *pt.Pos33SortMsg)
	go func() {
		for i := 0; i < count; i++ {
			n.sortCh <- &sortArg{vrfHash, i, num, diff, proof, ch}
		}
	}()
	j := 0
	var msgs []*pt.Pos33SortMsg
	for j < count {
		m := <-ch
		if m != nil {
			msgs = append(msgs, m)
		}
		j++
	}
	close(ch)
	return msgs
}

func (n *node) committeeSort(seed []byte, height int64, round, ty int) []*pt.Pos33SortMsg {
	count := n.queryTicketCount(n.myAddr, height-10)
	priv := n.getPriv()
	if priv == nil {
		return nil
	}

	diff := n.getDiff(height, round)

	input := &pt.VrfInput{Seed: seed, Height: height, Round: int32(round), Ty: int32(ty)}
	vrfHash, vrfProof := calcuVrfHash(input, priv)
	proof := &pt.HashProof{
		Input:    input,
		VrfHash:  vrfHash,
		VrfProof: vrfProof,
		Pubkey:   priv.PubKey().Bytes(),
	}

	msgs := n.doSort(vrfHash, int(count), 0, diff, proof)
	plog.Debug("voter sort", "height", height, "round", round, "mycount", count, "n", len(msgs), "diff", diff*1000000, "addr", address.PubKeyToAddr(ethID, proof.Pubkey)[:16])
	return msgs
}

func vrfVerify(pub []byte, input []byte, proof []byte, hash []byte) error {
	pubKey, err := secp256k1.ParsePubKey(pub, secp256k1.S256())
	if err != nil {
		plog.Error("vrfVerify", "err", err)
		return pt.ErrVrfVerify
	}
	vrfPub := &vrf.PublicKey{PublicKey: (*ecdsa.PublicKey)(pubKey)}
	vrfHash, err := vrfPub.ProofToHash(input, proof)
	if err != nil {
		plog.Error("vrfVerify", "err", err)
		return pt.ErrVrfVerify
	}
	if !bytes.Equal(vrfHash[:], hash) {
		plog.Error("vrfVerify", "err", fmt.Errorf("invalid VRF hash"))
		return pt.ErrVrfVerify
	}
	return nil
}

var errDiff = errors.New("diff error")

func (n *node) queryDeposit(addr string) (*pt.Pos33DepositMsg, error) {
	resp, err := n.GetAPI().Query(pt.Pos33TicketX, "Pos33Deposit", &types.ReqAddr{Addr: addr})
	if err != nil {
		return nil, err
	}
	reply := resp.(*pt.Pos33DepositMsg)
	return reply, nil
}

func (n *node) verifySort(height int64, ty int, seed []byte, m *pt.Pos33SortMsg) error {
	if height <= pt.Pos33SortBlocks {
		return nil
	}
	if m == nil || m.Proof == nil || m.SortHash == nil || m.Proof.Input == nil {
		return fmt.Errorf("verifySort error: sort msg is nil")
	}

	addr := address.PubKeyToAddr(ethID, m.Proof.Pubkey)
	count := n.queryTicketCount(addr, height-pt.Pos33SortBlocks)
	if count <= m.SortHash.Index {
		return fmt.Errorf("sort index %d > %d your count, height %d", m.SortHash.Index, count, height)
	}

	if m.Proof.Input.Height != height {
		return fmt.Errorf("verifySort error, height NOT match: %d!=%d", m.Proof.Input.Height, height)
	}
	if string(m.Proof.Input.Seed) != string(seed) {
		return fmt.Errorf("verifySort error, seed NOT match")
	}
	if m.Proof.Input.Ty != int32(ty) {
		return fmt.Errorf("verifySort error, step NOT match")
	}

	round := m.Proof.Input.Round
	input := &pt.VrfInput{Seed: seed, Height: height, Round: round, Ty: int32(ty)}
	in := types.Encode(input)
	err := vrfVerify(m.Proof.Pubkey, in, m.Proof.VrfProof, m.Proof.VrfHash)
	if err != nil {
		plog.Debug("vrfVerify error", "err", err, "height", height, "round", round, "ty", ty, "who", addr[:16])
		return err
	}
	data := fmt.Sprintf("%x+%d+%d", m.Proof.VrfHash, m.SortHash.Index, m.SortHash.Num)
	hash := hash2([]byte(data))
	if string(hash) != string(m.SortHash.Hash) {
		return fmt.Errorf("sort hash error")
	}

	tmpHash := make([]byte, len(hash))
	copy(tmpHash, hash)
	diff := n.getDiff(height, int(round))

	y := difficulty.HashToBig(tmpHash)
	z := new(big.Float).SetInt(y)
	if new(big.Float).Quo(z, fmax).Cmp(big.NewFloat(diff)) > 0 {
		plog.Error("verifySort diff error", "height", height, "ty", ty, "round", round, "diff", diff*1000000, "addr", address.PubKeyToAddr(ethID, m.Proof.Pubkey))
		return errDiff
	}

	return nil
}

func hash2(data []byte) []byte {
	return crypto.Sha256(crypto.Sha256(data))
}
