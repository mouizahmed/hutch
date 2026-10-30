// Package durable stores files in the bucket as encrypted, content-addressed chunks
// listed by complete manifests, and holds the keys that protect them.
//
// The server's master key derives every other key: one per project (chunks and
// manifests of its templates and boxes), one for the server's own backups, and one
// for naming. Chunk ids are HMACs of the plaintext under the owning key, so chunks
// dedupe within a project and reveal nothing about their contents.
package durable

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

type Key [32]byte

func NewKey() Key {
	var k Key
	rand.Read(k[:])
	return k
}

func ParseKey(b []byte) (Key, error) {
	var k Key
	if len(b) != len(k) {
		return k, fmt.Errorf("want a %d-byte key, got %d bytes", len(k), len(b))
	}
	copy(k[:], b)
	return k, nil
}

// Derive returns the subkey of k for purpose.
func (k Key) Derive(purpose string) Key {
	out, err := hkdf.Key(sha256.New, k[:], nil, purpose, len(k))
	if err != nil {
		panic(err)
	}
	var d Key
	copy(d[:], out)
	return d
}

// ProjectKey encrypts a project's templates and boxes; workers get it while they host the project.
func (k Key) ProjectKey(project string) Key { return k.Derive("project:" + project) }

// ServerKey encrypts the server's backups.
func (k Key) ServerKey() Key { return k.Derive("server") }

// ProjectPrefix names a project in the bucket without revealing its ID.
func (k Key) ProjectPrefix(project string) string {
	nk := k.Derive("names")
	h := hmac.New(sha256.New, nk[:])
	h.Write([]byte(project))
	return "projects/" + hex.EncodeToString(h.Sum(nil))[:32]
}

// ChunkID names a chunk of plaintext.
func (k Key) ChunkID(plain []byte) string {
	ik := k.Derive("chunk-id")
	h := hmac.New(sha256.New, ik[:])
	h.Write(plain)
	return hex.EncodeToString(h.Sum(nil))
}

const sealVersion = 1

// Seal encrypts plain with AES-256-GCM, bound to aad (the object's name), so one
// object can't be passed off as another.
func (k Key) Seal(aad string, plain []byte) []byte {
	gcm := k.gcm()
	out := make([]byte, 1+gcm.NonceSize(), 1+gcm.NonceSize()+len(plain)+gcm.Overhead())
	out[0] = sealVersion
	rand.Read(out[1:])
	return gcm.Seal(out, out[1:], plain, []byte(aad))
}

func (k Key) Open(aad string, sealed []byte) ([]byte, error) {
	gcm := k.gcm()
	if len(sealed) < 1+gcm.NonceSize() || sealed[0] != sealVersion {
		return nil, fmt.Errorf("%s: not a sealed object", aad)
	}
	plain, err := gcm.Open(nil, sealed[1:1+gcm.NonceSize()], sealed[1+gcm.NonceSize():], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%s: wrong key or corrupt", aad)
	}
	return plain, nil
}

func (k Key) gcm() cipher.AEAD {
	sk := k.Derive("seal")
	block, err := aes.NewCipher(sk[:])
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return gcm
}

// Recovery is the master key wrapped by a key derived from the recovery passphrase.
type Recovery struct {
	KDF     string `json:"kdf"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
	Salt    []byte `json:"salt"`
	Sealed  []byte `json:"sealed"`
}

const recoveryAAD = "server/master"

// Wrap seals the master key under passphrase with Argon2id.
func Wrap(master Key, passphrase string) Recovery {
	return WrapBytes(recoveryAAD, master[:], passphrase)
}

func (r Recovery) Unwrap(passphrase string) (Key, error) {
	plain, err := r.UnwrapBytes(recoveryAAD, passphrase)
	if err != nil {
		return Key{}, err
	}
	return ParseKey(plain)
}

// WrapBytes seals plain, named by aad, under passphrase with Argon2id.
func WrapBytes(aad string, plain []byte, passphrase string) Recovery {
	r := Recovery{KDF: "argon2id", Time: 3, Memory: 256 << 10, Threads: 4, Salt: make([]byte, 16)}
	rand.Read(r.Salt)
	r.Sealed = r.kek(passphrase).Seal(aad, plain)
	return r
}

func (r Recovery) UnwrapBytes(aad, passphrase string) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	plain, err := r.kek(passphrase).Open(aad, r.Sealed)
	if err != nil {
		return nil, errors.New("wrong recovery passphrase")
	}
	return plain, nil
}

// The recovery header is unauthenticated until the derived key opens Sealed.
// Reject invalid Argon2 inputs and bound their cost before deriving that key.
// Current and historical Hutch files use time=3, memory=256 MiB, threads=4.
func (r Recovery) validate() error {
	if r.KDF != "argon2id" {
		return fmt.Errorf("unknown key derivation %q", r.KDF)
	}
	if r.Time < 1 || r.Time > 10 || r.Threads < 1 || r.Threads > 16 ||
		r.Memory < 8*uint32(r.Threads) || r.Memory > 1<<20 ||
		len(r.Salt) < 8 || len(r.Salt) > 64 {
		return errors.New("invalid recovery key derivation parameters")
	}
	return nil
}

func (r Recovery) kek(passphrase string) Key {
	var k Key
	copy(k[:], argon2.IDKey([]byte(passphrase), r.Salt, r.Time, r.Memory, r.Threads, uint32(len(k))))
	return k
}

func (r Recovery) Marshal() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}
