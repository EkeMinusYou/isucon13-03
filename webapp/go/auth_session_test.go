package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestCompactSessionAuthentication(t *testing.T) {
	now := time.Now().Unix()
	value, issued, err := encodeSession(42, now+60)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSession(value, now)
	if err != nil || got != issued {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	if _, err := decodeSession(value, now+61); err == nil {
		t.Fatal("expired session accepted")
	}
	for _, bad := range []string{value[:10], "x" + value[1:], value[:20] + "!" + value[21:]} {
		if _, err := decodeSession(bad, now); err == nil {
			t.Fatal("tampered session accepted")
		}
	}
	old := secret
	secret = []byte("different-key")
	_, err = decodeSession(value, now)
	secret = old
	if err == nil {
		t.Fatal("wrong key accepted")
	}
	e := echo.New()
	e.Use(measurementSession)
	e.GET("/private", func(c echo.Context) error {
		if err := verifyUserSession(c); err != nil {
			return err
		}
		return c.JSON(200, sessionUserID(c))
	})
	for _, tc := range []struct {
		cookie string
		code   int
	}{{value, 200}, {"", 403}, {"invalid", 401}} {
		req := httptest.NewRequest("GET", "/private", nil)
		if tc.cookie != "" {
			req.AddCookie(&http.Cookie{Name: defaultSessionIDKey, Value: tc.cookie})
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("status=%d want=%d", rec.Code, tc.code)
		}
		if tc.code == 200 && (rec.Body.String() != "42\n" || rec.Header().Get("X-Measurement-Session-ID") != issued.ID) {
			t.Fatal("identity differs between authentication and measurement")
		}
	}
}
