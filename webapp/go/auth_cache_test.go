package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticationCachesPasswordBoundaries(t *testing.T) {
	resetAuthenticationCaches()
	t.Cleanup(resetAuthenticationCaches)
	cache := authenticationCaches()
	password := strings.Repeat("a", 72)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptDefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cache.rememberPassword(42, string(hash), password)
	if err := cache.comparePassword(42, string(hash), password); err != nil {
		t.Fatal(err)
	}
	if err := cache.comparePassword(42, string(hash), "wrong"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("wrong password accepted: %v", err)
	}
	if err := cache.comparePassword(43, "invalid hash", password); err == nil {
		t.Fatal("proof reused for another user or stored hash")
	}
	changed, err := bcrypt.GenerateFromPassword([]byte("changed"), bcryptDefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.comparePassword(42, string(changed), password); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("stale password proof accepted: %v", err)
	}
	if err := cache.comparePassword(42, string(changed), "changed"); err != nil {
		t.Fatal(err)
	}
	// An exact-proof miss must preserve bcrypt's handling of long inputs.
	want := bcrypt.CompareHashAndPassword(hash, []byte(password+"extra"))
	got := cache.comparePassword(42, string(hash), password+"extra")
	if !errors.Is(got, want) {
		t.Fatalf("bcrypt fallback differs: got=%v want=%v", got, want)
	}
	resetAuthenticationCaches()
	if authenticationCaches() == cache {
		t.Fatal("initialization retained authentication caches")
	}
	if err := authenticationCaches().comparePassword(42, string(hash), password); err != nil {
		t.Fatalf("cold cache failed: %v", err)
	}
}

func TestAuthenticationCachesSessionBoundaries(t *testing.T) {
	resetAuthenticationCaches()
	t.Cleanup(resetAuthenticationCaches)
	now := time.Now().Unix()
	value, session, err := encodeSession(42, now+60)
	if err != nil {
		t.Fatal(err)
	}
	for _, clear := range []bool{false, true} {
		if clear {
			resetAuthenticationCaches()
		}
		if got, err := decodeSession(value, now+60); err != nil || got != session {
			t.Fatalf("expiry boundary, clear=%v: %v", clear, err)
		}
		if _, err := decodeSession(value, now+61); err == nil {
			t.Fatal("cached expired session accepted")
		}
		bad := []byte(value)
		if bad[60] == 'A' {
			bad[60] = 'B'
		} else {
			bad[60] = 'A'
		}
		if _, err := decodeSession(string(bad), now); err == nil {
			t.Fatal("modified cached token accepted")
		}
	}
	invalid, _, err := encodeSession(0, now+60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSession(invalid, now); err == nil {
		t.Fatal("invalid issued user accepted")
	}
}

func TestAuthenticationCachesConcurrent(t *testing.T) {
	resetAuthenticationCaches()
	t.Cleanup(resetAuthenticationCaches)
	cache := authenticationCaches()
	hash, err := bcrypt.GenerateFromPassword([]byte("password"), bcryptDefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cache.rememberPassword(42, string(hash), "password")
	now := time.Now().Unix()
	value, session, err := encodeSession(42, now+60)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 50 {
				cache.rememberPassword(42, string(hash), "password")
				if err := cache.comparePassword(42, string(hash), "password"); err != nil {
					t.Error(err)
				}
				if got, err := decodeSession(value, now); err != nil || got != session {
					t.Errorf("concurrent session: %v", err)
				}
			}
		}()
	}
	workers.Wait()
}
