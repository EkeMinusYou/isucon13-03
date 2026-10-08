package main

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

func TestNGMatcherPatternSemantics(t *testing.T) {
	if len(ngGeneralCI) != (65536+1)*2 {
		t.Fatal("invalid collation table")
	}
	for _, tc := range []struct {
		text, word string
		want       bool
	}{
		{"CAFE", "café", true}, {"a\nb", "a_b", true}, {"q123r", "q%r", true},
		{"slash_", `slash\_`, true}, {"slashX", `slash\_`, false},
		{"", "", true}, {"abc", "", true}, {"a\x00b", "a_b", true},
		{"abc%", `abc\`, true}, {"abc%tail", `abc\`, false},
		{`abc\tail`, `abc\\`, true}, {"cafe", "café ", false},
		{"strasse", "straße", false}, {"xxa%bb_yy", `a\%b%\_`, true},
	} {
		if got := matchesNG(tc.text, []*ngPattern{compileNG(tc.word)}); got != tc.want {
			t.Errorf("text=%q word=%q: got=%t want=%t", tc.text, tc.word, got, tc.want)
		}
	}
}

func checkNGMatcherMySQL(t *testing.T, db *sqlx.DB) {
	t.Helper()
	type pair struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
		Word string `json:"word"`
	}
	batch := make([]pair, 0, 512)
	checked := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		data, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		var results []struct {
			ID  int  `db:"id"`
			Hit bool `db:"hit"`
		}
		// Use the native LIKE expression as the oracle, not WEIGHT_STRING.
		err = db.Select(&results, `SELECT c.id, c.text COLLATE utf8mb4_general_ci
			LIKE CONCAT('%', c.word COLLATE utf8mb4_general_ci, '%') AS hit
			FROM JSON_TABLE(?, '$[*]' COLUMNS(id INT PATH '$.id',
			text VARCHAR(255) CHARACTER SET utf8mb4 PATH '$.text',
			word VARCHAR(255) CHARACTER SET utf8mb4 PATH '$.word')) AS c`, string(data))
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != len(batch) {
			t.Fatal("incomplete LIKE oracle results")
		}
		for _, result := range results {
			p := batch[result.ID]
			if got := matchesNG(p.Text, []*ngPattern{compileNG(p.Word)}); got != result.Hit {
				t.Fatalf("MySQL LIKE differs: text=%q word=%q Go=%t MySQL=%t", p.Text, p.Word, got, result.Hit)
			}
		}
		checked += len(batch)
		batch = batch[:0]
	}
	add := func(text, word string) {
		batch = append(batch, pair{ID: len(batch), Text: text, Word: word})
		if len(batch) == cap(batch) {
			flush()
		}
	}
	escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	// Exercise every valid BMP character against a representative of its weight
	// class, including NUL, accents, non-Latin alphabets and unmapped characters.
	canonical := make(map[uint32]rune)
	for r := rune(0); r <= 65535; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		weight := ngWeight(r)
		if _, ok := canonical[weight]; !ok {
			canonical[weight] = r
		}
		add("prefix"+string(canonical[weight])+"suffix", escape.Replace(string(r)))
	}
	for r := rune(65536); r <= utf8.MaxRune; r += 8191 {
		add("\ufffd", string(r))
	}
	texts := []string{"", " ", "CAFE", "café", "cafe\u0301", "straße", "strasse", "ß", "s", "İıI", "Σσς", "æAE", "ＡA", "あア", "😀", "😁", "\ufffd", "a\x00b", "a\nb", "a%b_c\\", "abc%", "abc%tail", `abc\`, "xyzz", strings.Repeat("a", 255)}
	words := []string{"", " ", "café", "é", "e\u0301", "ß", "ss", "s", "i", "σ", "æ", "AE", "A", "あ", "😀", "😁", "\ufffd", "_", "%", `\`, `\\`, `\%`, `\_`, `a\`, "a_b", "a%b", "a%_b", "%%_", `%\`, `abc\`, "\x00", "\n", "y%z", "y__z"}
	for _, text := range texts {
		for _, word := range words {
			add(text, word)
		}
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("aAbBéßİıΣσς😀😁%_\\\x00\n ")
	random := func(n int) string {
		runes := make([]rune, n)
		for i := range runes {
			runes[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(runes)
	}
	for i := 0; i < 4096; i++ {
		add(random(rng.Intn(20)), random(rng.Intn(10)))
	}
	flush()
	t.Logf("in-memory matcher agrees with native MySQL LIKE for %d cases", checked)
}
