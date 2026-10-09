package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

func checkAuthenticationCachesMySQL(t *testing.T, db *sqlx.DB) {
	t.Helper()
	resetAuthenticationCaches()
	t.Cleanup(resetAuthenticationCaches)
	dns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer dns.Close()
	t.Setenv("ISUPIPE_DNS_URL", dns.URL)
	e := echo.New()
	e.Use(measurementSession)
	e.POST("/api/register", registerHandler)
	e.POST("/api/login", loginHandler)
	e.GET("/api/user/me", getMeHandler)
	request := func(path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if rec.Code != status {
			t.Fatalf("%s: status=%d want=%d", path, rec.Code, status)
		}
		return rec
	}
	request("/api/register", `{"name":"auth-cache-user","password":"original","theme":{"dark_mode":true}}`, http.StatusCreated)
	var user UserModel
	if err := db.Get(&user, "SELECT * FROM users WHERE name=?", "auth-cache-user"); err != nil {
		t.Fatal(err)
	}
	if cost, err := bcrypt.Cost([]byte(user.HashedPassword)); err != nil || cost != bcryptDefaultCost {
		t.Fatal("registration changed bcrypt or its cost")
	}
	cache := authenticationCaches()
	key := passwordCacheKey{userID: user.ID, hash: user.HashedPassword}
	if proof, ok := cache.passwords[uint64(user.ID)&15].get(key); !ok || proof != cache.passwordProof("original") {
		t.Fatal("committed registration did not publish its password proof")
	}
	login := func(password string, status int) *httptest.ResponseRecorder {
		body, err := json.Marshal(LoginRequest{Username: user.Name, Password: password})
		if err != nil {
			t.Fatal(err)
		}
		return request("/api/login", string(body), status)
	}
	login("wrong", http.StatusUnauthorized)
	login("original", http.StatusOK)
	loggedIn := login("original", http.StatusOK)
	cookies := loggedIn.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != defaultSessionIDKey {
		t.Fatal("login did not issue the session cookie")
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api/user/me", nil)
		req.AddCookie(cookies[0])
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		var me User
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != http.StatusOK || me.ID != user.ID || rec.Header().Get("X-Measurement-Session-ID") == "" {
			t.Fatal("cached authentication changed user or measurement identity")
		}
	}
	changed, err := bcrypt.GenerateFromPassword([]byte("changed"), bcryptDefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE users SET password=? WHERE id=?", string(changed), user.ID); err != nil {
		t.Fatal(err)
	}
	login("original", http.StatusUnauthorized)
	login("changed", http.StatusOK)
	resetAuthenticationCaches()
	login("changed", http.StatusOK)
	login("wrong", http.StatusUnauthorized)
}
