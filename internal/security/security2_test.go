package security

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
)

func vector(t *testing.T) map[string]string {
	t.Helper()
	b, e := os.ReadFile("testdata/esp_idf_srp_vector.json")
	if e != nil {
		t.Fatal(e)
	}
	v := map[string]string{}
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func referenceClient(t *testing.T) (*Client, map[string]string) {
	t.Helper()
	v := vector(t)
	c, e := New(v["username"], v["password"])
	if e != nil {
		t.Fatal(e)
	}
	c.a = integer(unhex(t, v["a"]))
	c.A = integer(unhex(t, v["A"]))
	return c, v
}
func TestESPReferenceSRPVector(t *testing.T) {
	c, v := referenceClient(t)
	proof, server, key, e := c.challenge(unhex(t, v["salt"]), unhex(t, v["B"]))
	if e != nil {
		t.Fatal(e)
	}
	for name, b := range map[string][]byte{"proof": proof, "server_proof": server, "key": key} {
		if hex.EncodeToString(b) != v[name] {
			t.Fatalf("official ESP-IDF %s mismatch", name)
		}
	}
}
func referenceRPC(t *testing.T, v map[string]string, wrongProof, corrupt bool) RPC {
	t.Helper()
	block, e := aes.NewCipher(unhex(t, v["key"])[:32])
	if e != nil {
		t.Fatal(e)
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		t.Fatal(e)
	}
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 1}
	step := 0
	return func(ctx context.Context, endpoint string, b []byte) ([]byte, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if endpoint == "sec-session" {
			step++
			if step == 1 {
				f, e := fields(b)
				if e != nil || f[2].n != 2 {
					t.Fatal("invalid scheme")
				}
				sec, e := fields(f[12].b)
				if e != nil {
					t.Fatal(e)
				}
				cmd, e := fields(sec[20].b)
				if e != nil {
					t.Fatal(e)
				}
				if string(cmd[1].b) != v["username"] || !bytes.Equal(cmd[2].b, unhex(t, v["A"])) {
					t.Fatal("wrong SRP command0")
				}
				return session(1, 21, append(fieldBytes(2, unhex(t, v["B"])), fieldBytes(3, unhex(t, v["salt"]))...)), nil
			}
			if step == 2 {
				f, _ := fields(b)
				sec, _ := fields(f[12].b)
				cmd, _ := fields(sec[22].b)
				if !bytes.Equal(cmd[1].b, unhex(t, v["proof"])) {
					t.Fatal("wrong SRP command1")
				}
				proof := unhex(t, v["server_proof"])
				if wrongProof {
					proof[0] ^= 1
				}
				return session(3, 23, append(fieldBytes(2, proof), fieldBytes(3, nonce)...)), nil
			}
			return nil, errors.New("unexpected handshake")
		}
		plain, e := aead.Open(nil, nonce, b, nil)
		if e != nil {
			return nil, e
		}
		n := binary.BigEndian.Uint32(nonce[8:])
		binary.BigEndian.PutUint32(nonce[8:], n+1)
		out := aead.Seal(nil, nonce, plain, nil)
		binary.BigEndian.PutUint32(nonce[8:], n+2)
		if corrupt {
			out[0] ^= 1
		}
		return out, nil
	}
}
func TestSecurityHandshakeAndConcurrentEncryptedRPC(t *testing.T) {
	c, v := referenceClient(t)
	if e := c.Handshake(context.Background(), referenceRPC(t, v, false, false)); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := c.Call(context.Background(), "rpc", []byte(`{"method":"state.get"}`))
			if e != nil || string(out) != `{"method":"state.get"}` {
				t.Errorf("RPC failed: %s %v", out, e)
			}
		}()
	}
	wg.Wait()
	if !c.Ready() {
		t.Fatal("client lost readiness")
	}
	if e := c.Handshake(context.Background(), nil); e == nil {
		t.Fatal("allowed nonce/session reuse")
	}
}
func TestRejectsBadProofAndAuthenticatedData(t *testing.T) {
	for _, badProof := range []bool{true, false} {
		c, v := referenceClient(t)
		e := c.Handshake(context.Background(), referenceRPC(t, v, badProof, !badProof))
		if badProof {
			if e == nil {
				t.Fatal("accepted forged proof")
			}
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.Call(context.Background(), "rpc", []byte("secret")); e == nil || c.Ready() {
			t.Fatal("accepted altered ciphertext or retained unsafe session")
		}
		if _, e = c.Call(context.Background(), "rpc", []byte("secret")); e == nil {
			t.Fatal("reused invalidated cipher")
		}
	}
}
func TestRejectsUnsafeChallenges(t *testing.T) {
	c, _ := referenceClient(t)
	for _, b := range [][]byte{nil, {0}, prime.Bytes(), make([]byte, 385)} {
		if _, _, _, e := c.challenge([]byte{1}, b); e == nil {
			t.Fatal("unsafe SRP key accepted")
		}
	}
}
func FuzzProtobufParser(f *testing.F) {
	f.Add(session(1, 21, fieldBytes(2, []byte("sample"))))
	f.Add([]byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = fields(b); _, _ = responseBody(b, 1, 21) })
}
