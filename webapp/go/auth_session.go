package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

const defaultSessionIDKey = "SESSIONID"
const sessionPayloadSize = 33

type authSession struct {
	UserID  int64
	Expires int64
	ID      string
}

func encodeSession(userID, expires int64) (string, authSession, error) {
	var token [sessionPayloadSize + sha256.Size]byte
	token[0] = 1
	binary.BigEndian.PutUint64(token[1:9], uint64(userID))
	binary.BigEndian.PutUint64(token[9:17], uint64(expires))
	if _, err := rand.Read(token[17:sessionPayloadSize]); err != nil {
		return "", authSession{}, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("isupipe-session-v1:"))
	_, _ = mac.Write(token[:sessionPayloadSize])
	copy(token[sessionPayloadSize:], mac.Sum(nil))
	value := base64.RawURLEncoding.EncodeToString(token[:])
	session := authSession{UserID: userID, Expires: expires, ID: hex.EncodeToString(token[sessionPayloadSize:])}
	authenticationCaches().rememberSession(value, session)
	return value, session, nil
}

func decodeSession(value string, now int64) (authSession, error) {
	if len(value) != base64.RawURLEncoding.EncodedLen(sessionPayloadSize+sha256.Size) {
		return authSession{}, errors.New("invalid session")
	}
	cache := authenticationCaches()
	if session, ok := cache.sessions[value[32]&15].get(value); ok {
		if now > session.Expires {
			return authSession{}, errors.New("session has expired or has invalid user")
		}
		return session, nil
	}
	session, err := decodeSessionUncached(value, now)
	if err == nil {
		cache.rememberSession(value, session)
	}
	return session, err
}

func decodeSessionUncached(value string, now int64) (authSession, error) {
	var token [sessionPayloadSize + sha256.Size]byte
	n, err := base64.RawURLEncoding.Decode(token[:], []byte(value))
	if err != nil || n != len(token) || token[0] != 1 {
		return authSession{}, errors.New("invalid session")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("isupipe-session-v1:"))
	_, _ = mac.Write(token[:sessionPayloadSize])
	if !hmac.Equal(token[sessionPayloadSize:], mac.Sum(nil)) {
		return authSession{}, errors.New("invalid session signature")
	}
	s := authSession{UserID: int64(binary.BigEndian.Uint64(token[1:9])), Expires: int64(binary.BigEndian.Uint64(token[9:17])), ID: hex.EncodeToString(token[sessionPayloadSize:])}
	if s.UserID <= 0 || now > s.Expires {
		return authSession{}, errors.New("session has expired or has invalid user")
	}
	return s, nil
}

func sessionFromRequest(r *http.Request) (authSession, error) {
	cookie, err := r.Cookie(defaultSessionIDKey)
	if err != nil {
		return authSession{}, err
	}
	return decodeSession(cookie.Value, time.Now().Unix())
}

func requestSession(c echo.Context) (authSession, error) {
	if s, ok := c.Get("auth_session").(authSession); ok {
		return s, nil
	}
	s, err := sessionFromRequest(c.Request())
	if err == nil {
		c.Set("auth_session", s)
	}
	return s, err
}

func sessionUserID(c echo.Context) int64 {
	s, _ := requestSession(c)
	return s.UserID
}

func verifyUserSession(c echo.Context) error {
	_, err := requestSession(c)
	if errors.Is(err, http.ErrNoCookie) {
		return echo.NewHTTPError(http.StatusForbidden, "failed to get session")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
	}
	return nil
}
