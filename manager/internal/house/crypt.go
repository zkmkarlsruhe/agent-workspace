// SPDX-License-Identifier: MIT

package house

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// keyInfo is the HKDF context of docs/adr/README.md §4.1; a new scheme gets /v2.
const keyInfo = "oc-verbindung/v1"

// deriveKey turns OC_MASTER_KEY into the AES-256 key for stored secrets, so
// the container password derivation (lifecycle.Password) and this key never
// share bytes.
func deriveKey(master []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, master, nil, keyInfo, 32)
}

// seal encrypts with AES-256-GCM; aad binds the blob to its row (uid|service),
// so a copied blob does not open under another person.
func seal(key, aad, plain []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	return g.Seal(nonce, nonce, plain, aad), nil
}

func open(key, aad, blob []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < g.NonceSize() {
		return nil, errors.New("house: secret too short")
	}
	return g.Open(nil, blob[:g.NonceSize()], blob[g.NonceSize():], aad)
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
