package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

const (
	passwordCacheBudget = 128 << 10 // Per shard; 2 MiB in total.
	sessionCacheBudget  = 256 << 10 // Per shard; 4 MiB in total.
)

type passwordCacheKey struct {
	userID int64
	hash   string
}

type authenticationCache struct {
	key       string
	passwords [16]boundedCache[passwordCacheKey, [sha256.Size]byte]
	sessions  [16]boundedCache[string, authSession]
}

var currentAuthenticationCache atomic.Pointer[authenticationCache]

func authenticationCaches() *authenticationCache {
	for {
		previous := currentAuthenticationCache.Load()
		if previous != nil && previous.key == string(secret) {
			return previous
		}
		next := &authenticationCache{key: string(secret)}
		if currentAuthenticationCache.CompareAndSwap(previous, next) {
			return next
		}
	}
}

func resetAuthenticationCaches() {
	currentAuthenticationCache.Store(&authenticationCache{key: string(secret)})
}

func (c *authenticationCache) passwordProof(password string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, []byte(c.key))
	_, _ = mac.Write([]byte("isupipe-password-proof-v1:"))
	_, _ = mac.Write([]byte(password))
	var proof [sha256.Size]byte
	mac.Sum(proof[:0])
	return proof
}

// Call only after successful registration or bcrypt verification. The current
// database hash is part of the key; neither passwords nor failed proofs are kept.
func (c *authenticationCache) rememberPassword(userID int64, hash, password string) {
	key := passwordCacheKey{userID: userID, hash: hash}
	c.passwords[uint64(userID)&15].put(key, c.passwordProof(password), 256+len(hash), passwordCacheBudget)
}

func (c *authenticationCache) comparePassword(userID int64, hash, password string) error {
	key := passwordCacheKey{userID: userID, hash: hash}
	proof := c.passwordProof(password)
	if cached, ok := c.passwords[uint64(userID)&15].get(key); ok && hmac.Equal(cached[:], proof[:]) {
		return nil
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return err
	}
	c.passwords[uint64(userID)&15].put(key, proof, 256+len(hash), passwordCacheBudget)
	return nil
}

func (c *authenticationCache) rememberSession(value string, session authSession) {
	if session.UserID > 0 {
		// Cookie values may be slices of a much larger request header.
		c.sessions[value[32]&15].put(strings.Clone(value), session, 256+len(value)+len(session.ID), sessionCacheBudget)
	}
}
