// Extract the server's character weights without modifying any database.
package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := flag.String("dsn", "root@unix(/var/run/mysqld/mysqld.sock)/", "read-only MySQL connection")
	flag.Parse()
	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		log.Fatal(err)
	}
	weights := make([]byte, (65536+1)*2)
	var supplementary []byte
	count := 0
	for start := 0; start <= 0x10ffff; start += 512 {
		rows, err := db.Query(`WITH RECURSIVE seq(n) AS (
			SELECT CAST(? AS UNSIGNED) UNION ALL SELECT n+1 FROM seq WHERE n<?)
			SELECT n, WEIGHT_STRING(CONVERT(CONVERT(UNHEX(LPAD(HEX(n),8,'0')) USING utf32) USING utf8mb4) COLLATE utf8mb4_general_ci)
			FROM seq WHERE n NOT BETWEEN 55296 AND 57343`, start, start+511)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var n int
			var weight []byte
			if err := rows.Scan(&n, &weight); err != nil {
				log.Fatal(err)
			}
			if len(weight) != 2 {
				log.Fatalf("U+%04X has %d weight bytes", n, len(weight))
			}
			if n < 65536 {
				copy(weights[n*2:], weight)
			} else {
				if supplementary == nil {
					supplementary = append([]byte{}, weight...)
				}
				if !bytes.Equal(weight, supplementary) {
					log.Fatalf("nonuniform supplementary weight at U+%X", n)
				}
			}
			count++
		}
		if err := rows.Err(); err != nil {
			log.Fatal(err)
		}
		rows.Close()
	}
	copy(weights[65536*2:], supplementary)
	fmt.Fprintf(os.Stderr, "MySQL %s: verified %d Unicode scalars; supplementary weight %04X; table %d bytes\n", version, count, binary.BigEndian.Uint16(supplementary), len(weights))
	if _, err := os.Stdout.Write(weights); err != nil {
		log.Fatal(err)
	}
}
