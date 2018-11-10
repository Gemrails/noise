package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"github.com/monnand/dhkx"
	"github.com/pkg/errors"
	"sync"
)

type PendingPeer struct {
	Done        chan struct{}
	Established *EstablishedPeer
}

type EstablishedPeer struct {
	sync.Mutex

	adapter     MessageAdapter
	kxState     KeyExchangeState
	kxDone      chan struct{}
	dhGroup     *dhkx.DHGroup
	dhKeypair   *dhkx.DHKey
	aead        cipher.AEAD
	localNonce  uint64
	remoteNonce uint64

	closed bool
}

type KeyExchangeState byte

const (
	KeyExchange_Invalid KeyExchangeState = iota
	KeyExchange_PassivelyWaitForPublicKey
	KeyExchange_ActivelyWaitForPublicKey
	KeyExchange_Failed
	KeyExchange_Done
)

func prependSimpleSignature(idAdapter IdentityAdapter, data []byte) []byte {
	ret := make([]byte, idAdapter.SignatureSize()+len(data))
	copy(ret, idAdapter.Sign(data))
	copy(ret[idAdapter.SignatureSize():], data)
	return ret

}

func EstablishPeerWithMessageAdapter(c *Controller, dhGroup *dhkx.DHGroup, dhKeypair *dhkx.DHKey, idAdapter IdentityAdapter, adapter MessageAdapter, passive bool) (*EstablishedPeer, error) {
	peer := &EstablishedPeer{
		adapter:   adapter,
		kxDone:    make(chan struct{}),
		dhGroup:   dhGroup,
		dhKeypair: dhKeypair,
	}
	if passive {
		peer.kxState = KeyExchange_PassivelyWaitForPublicKey
	} else {
		peer.kxState = KeyExchange_ActivelyWaitForPublicKey
		err := peer.adapter.SendMessage(c, prependSimpleSignature(idAdapter, peer.dhKeypair.Bytes()))
		if err != nil {
			return nil, err
		}
	}

	return peer, nil
}

func (p *EstablishedPeer) continueKeyExchange(c *Controller, idAdapter IdentityAdapter, raw []byte) error {
	p.Lock()
	defer p.Unlock()

	switch p.kxState {
	case KeyExchange_ActivelyWaitForPublicKey, KeyExchange_PassivelyWaitForPublicKey:
		if len(raw) < idAdapter.SignatureSize() {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
			return errors.New("incomplete message")
		}

		sig := raw[:idAdapter.SignatureSize()]
		rawPubKey := raw[idAdapter.SignatureSize():]
		if idAdapter.Verify(p.adapter.RemoteEndpoint(), rawPubKey, sig) == false {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
			return errors.New("signature verification failed")
		}

		peerPubKey := dhkx.NewPublicKey(rawPubKey)
		sharedKey, err := p.dhGroup.ComputeKey(peerPubKey, p.dhKeypair)
		if err != nil {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
			return err
		}

		if p.kxState == KeyExchange_PassivelyWaitForPublicKey {
			p.adapter.SendMessage(c, prependSimpleSignature(idAdapter, p.dhKeypair.Bytes())) // only sends the public key
		}

		p.dhGroup = nil
		p.dhKeypair = nil
		aesKey := sha256.Sum256(sharedKey.Bytes()) // FIXME: security?
		aesCipher, err := aes.NewCipher(aesKey[:])
		if err != nil {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
			return err
		}
		aead, err := cipher.NewGCM(aesCipher)
		if err != nil {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
			return err
		}
		p.aead = aead
		p.kxState = KeyExchange_Done
		close(p.kxDone)
		return nil
	case KeyExchange_Failed:
		return errors.New("failed previously")
	default:
		panic("unexpected key exchange state")
	}
}

func (p *EstablishedPeer) Close() {
	p.Lock()
	if !p.closed {
		p.closed = true
		p.adapter.Close()
		if p.kxState != KeyExchange_Done && p.kxState != KeyExchange_Failed {
			p.kxState = KeyExchange_Failed
			close(p.kxDone)
		}
	}
	p.Unlock()
}

func (p *EstablishedPeer) SendMessage(c *Controller, body []byte) error {
	p.Lock()
	defer p.Unlock()

	nonceBuffer := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonceBuffer, p.localNonce)
	p.localNonce++

	cipherText := p.aead.Seal(nil, nonceBuffer, body, nil)
	return p.adapter.SendMessage(c, cipherText)
}

func (p *EstablishedPeer) UnwrapMessage(c *Controller, raw []byte) ([]byte, error) {
	p.Lock()
	defer p.Unlock()

	nonceBuffer := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonceBuffer, p.remoteNonce)
	p.remoteNonce++

	return p.aead.Open(nil, nonceBuffer, raw, nil)
}

func (p *EstablishedPeer) RemoteEndpoint() []byte {
	return p.adapter.RemoteEndpoint()
}