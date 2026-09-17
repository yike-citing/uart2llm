// Package security implements ESP-IDF Protocomm Security 2 patch 1.
// Wire definitions and SRP arithmetic were checked against ESP-IDF v6.0.1
// components/protocomm and its esp_local_ctrl Python reference client.
package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
)

const primeHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"

// RFC5054's 3072-bit prime; initialization below validates accidental edits.
var prime = func() *big.Int {
	n, ok := new(big.Int).SetString(primeHex, 16)
	if !ok || n.BitLen() != 3072 {
		panic("invalid SRP group")
	}
	return n
}()
var generator = big.NewInt(5)

type RPC func(context.Context, string, []byte) ([]byte, error)
type Client struct {
	mu                 sync.Mutex
	username, password string
	a, A               *big.Int
	aead               cipher.AEAD
	nonce              []byte
	rpc                RPC
	started, ready     bool
}

func New(username, password string) (*Client, error) {
	if len(username) == 0 || len(username) > 128 || len(password) == 0 || len(password) > 512 {
		return nil, errors.New("pairing username/password missing or too long")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	secret[0] |= 0x80
	a := new(big.Int).SetBytes(secret)
	return &Client{username: username, password: password, a: a, A: new(big.Int).Exp(generator, a, prime)}, nil
}
func hash(parts ...[]byte) []byte {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}
func integer(b []byte) *big.Int { return new(big.Int).SetBytes(b) }
func padded(n *big.Int) []byte  { return n.FillBytes(make([]byte, 384)) }

func (c *Client) challenge(salt, pub []byte) (proof, expected, key []byte, err error) {
	if len(salt) == 0 || len(salt) > 64 || len(pub) == 0 || len(pub) > 384 {
		return nil, nil, nil, errors.New("invalid SRP challenge lengths")
	}
	B := integer(pub)
	if B.Sign() == 0 || B.Cmp(prime) >= 0 {
		return nil, nil, nil, errors.New("unsafe SRP public key")
	}
	u := integer(hash(padded(c.A), padded(B)))
	if u.Sign() == 0 {
		return nil, nil, nil, errors.New("unsafe SRP scrambling parameter")
	}
	k := integer(hash(padded(prime), padded(generator)))
	x := integer(hash(salt, hash([]byte(c.username+":"+c.password))))
	v := new(big.Int).Exp(generator, x, prime)
	base := new(big.Int).Sub(B, new(big.Int).Mul(k, v))
	base.Mod(base, prime)
	exponent := new(big.Int).Add(c.a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exponent, prime)
	if S.Sign() == 0 {
		return nil, nil, nil, errors.New("unsafe SRP shared secret")
	}
	key = hash(S.Bytes())
	hn, hg := hash(prime.Bytes()), hash(padded(generator))
	for i := range hn {
		hn[i] ^= hg[i]
	}
	proof = hash(hn, hash([]byte(c.username)), salt, c.A.Bytes(), pub, key)
	expected = hash(c.A.Bytes(), proof, key)
	return
}

func (c *Client) Handshake(ctx context.Context, rpc RPC) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("security handshake requires a fresh client")
	}
	c.started = true
	c.rpc = rpc
	command := session(0, 20, append(fieldBytes(1, []byte(c.username)), fieldBytes(2, c.A.Bytes())...))
	response, err := rpc(ctx, "sec-session", command)
	if err != nil {
		return err
	}
	body, err := responseBody(response, 1, 21)
	if err != nil {
		return err
	}
	pub, err := getBytes(body, 2)
	if err != nil {
		return err
	}
	salt, err := getBytes(body, 3)
	if err != nil {
		return err
	}
	proof, expected, key, err := c.challenge(salt, pub)
	if err != nil {
		return err
	}
	response, err = rpc(ctx, "sec-session", session(2, 22, fieldBytes(1, proof)))
	if err != nil {
		return err
	}
	body, err = responseBody(response, 3, 23)
	if err != nil {
		return err
	}
	peerProof, err := getBytes(body, 2)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(peerProof, expected) != 1 {
		return errors.New("device SRP proof verification failed")
	}
	nonce, err := getBytes(body, 3)
	if err != nil {
		return err
	}
	if len(nonce) != 12 || binary.BigEndian.Uint32(nonce[8:]) == 0 {
		return errors.New("invalid security2 patch1 nonce")
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return err
	}
	c.aead, err = cipher.NewGCM(block)
	if err != nil {
		return err
	}
	c.nonce = append([]byte(nil), nonce...)
	c.ready = true
	c.password = ""
	c.a = nil
	return nil
}
func (c *Client) invalidate() { c.ready = false; c.aead = nil; c.nonce = nil }

// Call serializes encrypted request/response pairs because Security2 patch 1
// shares one nonce counter in both directions. Any ambiguous failure consumes
// this client: establish a new transport session before pairing again.
func (c *Client) Call(ctx context.Context, endpoint string, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready {
		return nil, errors.New("device is not paired")
	}
	counter := binary.BigEndian.Uint32(c.nonce[8:])
	if counter >= 0xfffffffe {
		c.invalidate()
		return nil, errors.New("security nonce exhausted; reconnect")
	}
	encrypted := c.aead.Seal(nil, c.nonce, payload, nil)
	binary.BigEndian.PutUint32(c.nonce[8:], counter+1)
	response, err := c.rpc(ctx, endpoint, encrypted)
	if err != nil {
		c.invalidate()
		return nil, err
	}
	plain, err := c.aead.Open(nil, c.nonce, response, nil)
	if err != nil {
		c.invalidate()
		return nil, errors.New("device response authentication failed")
	}
	binary.BigEndian.PutUint32(c.nonce[8:], counter+2)
	return plain, nil
}
func (c *Client) Ready() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.ready }

// Small bounded protobuf codec for the four frozen SessionData messages. It
// accepts unknown fields but rejects duplicate required fields and malformed
// lengths. No generated-code/runtime dependency is needed on the offline host.
func varint(v uint64) []byte { var b [10]byte; n := binary.PutUvarint(b[:], v); return b[:n] }
func fieldBytes(id int, b []byte) []byte {
	out := varint(uint64(id<<3 | 2))
	out = append(out, varint(uint64(len(b)))...)
	return append(out, b...)
}
func fieldInt(id int, v uint64) []byte { return append(varint(uint64(id<<3)), varint(v)...) }
func session(msg uint64, id int, payload []byte) []byte {
	inner := append(fieldInt(1, msg), fieldBytes(id, payload)...)
	return append(fieldInt(2, 2), fieldBytes(12, inner)...)
}

type value struct {
	wire uint64
	b    []byte
	n    uint64
}

func fields(b []byte) (map[int]value, error) {
	if len(b) > 4096 {
		return nil, errors.New("protobuf too large")
	}
	out := map[int]value{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 {
			return nil, errors.New("invalid protobuf tag")
		}
		b = b[n:]
		id := int(tag >> 3)
		v := value{wire: tag & 7}
		if _, exists := out[id]; exists {
			return nil, errors.New("duplicate protobuf field")
		}
		switch v.wire {
		case 0:
			x, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("invalid protobuf integer")
			}
			v.n = x
			b = b[n:]
		case 2:
			x, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("invalid protobuf length")
			}
			b = b[n:]
			if x > uint64(len(b)) {
				return nil, errors.New("truncated protobuf")
			}
			v.b = b[:int(x)]
			b = b[int(x):]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("truncated fixed64")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("truncated fixed32")
			}
			b = b[4:]
		default:
			return nil, errors.New("unsupported protobuf wire type")
		}
		out[id] = v
	}
	return out, nil
}
func getBytes(b []byte, id int) ([]byte, error) {
	f, err := fields(b)
	if err != nil {
		return nil, err
	}
	v, ok := f[id]
	if !ok || v.wire != 2 {
		return nil, fmt.Errorf("protobuf bytes field %d missing", id)
	}
	return v.b, nil
}
func responseBody(b []byte, msg uint64, id int) ([]byte, error) {
	f, err := fields(b)
	if err != nil {
		return nil, err
	}
	if f[2].wire != 0 || f[2].n != 2 || f[12].wire != 2 {
		return nil, errors.New("incorrect security scheme")
	}
	s, err := fields(f[12].b)
	if err != nil {
		return nil, err
	}
	if s[1].wire != 0 || s[1].n != msg || s[id].wire != 2 {
		return nil, errors.New("incorrect security response")
	}
	p, err := fields(s[id].b)
	if err != nil {
		return nil, err
	}
	if p[1].wire != 0 || p[1].n != 0 {
		return nil, errors.New("device rejected security handshake")
	}
	return s[id].b, nil
}
